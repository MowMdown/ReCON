package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type webPlayer struct {
	ID   int    `json:"id"`
	IP   string `json:"ip"`
	Port string `json:"port"`
	Ping int    `json:"ping"`
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type webBan struct {
	Index   int    `json:"index"`
	GUID    string `json:"guid"`
	Name    string `json:"name"`
	Minutes string `json:"minutes"`
	Perm    bool   `json:"perm"`
	Reason  string `json:"reason"`
}

var webUI = struct {
	PageMargin          string
	TableWidth          string
	CellPadding         string
	RowHeight           string
	HeaderFontSize      string
	BodyFontSize        string
	IDWidth             string
	PlayerWidth         string
	IPWidth             string
	GUIDWidth           string
	PingWidth           string
	ActionsWidth        string
	ModalWidth          string
	ModalPadding        string
	InputPadding        string
	ButtonPadding       string
	RefreshInterval     time.Duration
	BansRefreshInterval time.Duration
}{
	PageMargin:          "24px",
	TableWidth:          "100%",
	CellPadding:         "9px",
	RowHeight:           "42px",
	HeaderFontSize:      "14px",
	BodyFontSize:        "14px",
	IDWidth:             "60px",
	PlayerWidth:         "220px",
	IPWidth:             "150px",
	GUIDWidth:           "280px",
	PingWidth:           "80px",
	ActionsWidth:        "240px",
	ModalWidth:          "420px",
	ModalPadding:        "20px",
	InputPadding:        "9px",
	ButtonPadding:       "6px 10px",
	RefreshInterval:     5 * time.Second,
	BansRefreshInterval: 10 * time.Second,
}

var webRCON = struct {
	sync.Mutex
	s *live
}{}

func webConnect() (*live, error) {
	webRCON.Lock()
	defer webRCON.Unlock()
	if webRCON.s != nil && !webRCON.s.dead() {
		return webRCON.s, nil
	}
	if webRCON.s != nil {
		webRCON.s.close()
		webRCON.s = nil
	}
	s, err := dialLive(false)
	if err != nil {
		return nil, err
	}
	webRCON.s = s
	return s, nil
}

func webRun(command string, wait time.Duration, allowRetry bool) (string, bool, error) {
	maxAttempts := 1
	if allowRetry {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		s, err := webConnect()
		if err != nil {
			lastErr = err
			time.Sleep(1 * time.Second)
			continue
		}
		out, acked, err := s.run(command, wait)
		if err == nil && acked {
			return out, acked, nil
		}
		if err != nil {
			lastErr = err
		}
		if err != nil || !s.alive() {
			webRCON.Lock()
			if webRCON.s == s {
				s.close()
				webRCON.s = nil
			}
			webRCON.Unlock()
		}
		if allowRetry {
			time.Sleep(1 * time.Second)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("RCON command unacknowledged")
	}
	return "", false, lastErr
}

func parseWebPlayers(out string) []webPlayer {
	players := []webPlayer{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		host, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ping, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		guid := fields[3]
		if idx := strings.IndexByte(guid, '('); idx != -1 {
			guid = guid[:idx]
		}
		name := strings.Join(fields[4:], " ")
		players = append(players, webPlayer{ID: id, IP: host, Port: port, Ping: ping, GUID: guid, Name: name})
	}
	return players
}

func findWebPlayer(out string, id int) (webPlayer, bool) {
	for _, p := range parseWebPlayers(out) {
		if p.ID == id {
			return p, true
		}
	}
	return webPlayer{}, false
}

func webReason(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// banOpMu serialises everything that changes or mirrors the ban list
// (ban, unban, and BattlEye->DB sync) so a sync can't act on a stale
// list and delete a ban that was just added.
var banOpMu sync.Mutex

func doBan(w http.ResponseWriter, p webPlayer, req webActionRequest) {
	banOpMu.Lock()
	defer banOpMu.Unlock()
	// BattlEye ban is GUID-only.
	guidCmd := fmt.Sprintf("addBan %s %d %s", p.GUID, req.Minutes, req.Reason)
	_, guidAck, guidErr := webRun(guidCmd, 1500*time.Millisecond, false)
	if guidErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "guid_ban": false, "guid_error": guidErr.Error()})
		return
	}
	if !guidAck {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "guid_ban": false, "error": "GUID ban was not acknowledged"})
		return
	}
	// Disconnect the player now, using the ban reason as the kick reason.
	// The ban is already in place, so a failed kick doesn't fail the request.
	kicked := false
	kickCmd := fmt.Sprintf("kick %d %s", p.ID, req.Reason)
	_, kickAck, kickErr := webRun(kickCmd, 1500*time.Millisecond, false)
	kicked = kickErr == nil && kickAck
	if !kicked {
		log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
	}
	// BattlEye uses duration 0 for a permanent ban.
	perm := 0
	if req.Minutes == 0 {
		perm = 1
	}
	// Record the GUID, player name, and current IP in SQLite.
	// The IP is informational only and is NOT banned.
	if err := globalDB.BanUser(p.GUID, p.Name, p.IP, req.Reason, perm); err != nil {
		log.Printf("error recording ban in DB: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "guid_ban": true, "db_updated": false, "kicked": kicked, "error": "GUID ban succeeded but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guid_ban": true, "db_updated": true, "kicked": kicked, "perm": perm == 1})
}

// startPoller tracks players (and periodically the ban list) in the
// background so the DB stays current without any web page open.
// It is the only caller of SyncPlayers.
func startPoller(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		fails := 0
		n := 0
		for range t.C {
			n++
			out, acked, err := webRun("players", 3*time.Second, false)
			if err != nil || !acked {
				fails++
				// Server probably down: close open sessions once so
				// playtime doesn't keep accruing while it's offline.
				if fails == 3 {
					if err := globalDB.SyncPlayers(nil); err != nil {
						log.Printf("poller: close sessions: %v", err)
					}
				}
				continue
			}
			fails = 0
			if err := globalDB.SyncPlayers(parseWebPlayers(out)); err != nil {
				log.Printf("poller: sync: %v", err)
			}
			// Ban list changes rarely: reconcile at start and every 6th poll (~30s).
			if n%6 == 1 {
				if _, err := syncBans(); err != nil {
					log.Printf("poller: bans: %v", err)
				}
			}
		}
	}()
}

func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "use GET"})
		return
	}
	out, acked, err := webRun("players", 3*time.Second, true)
	if err != nil || !acked {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": "RCON unavailable"})
		return
	}
	players := parseWebPlayers(out)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": true, "players": players, "count": len(players)})
}

type webActionRequest struct {
	Reason  string `json:"reason"`
	Minutes int    `json:"minutes"`
	Message string `json:"message"`
}

func parsePlayerIDPath(path string) (int, bool) {
	const prefix = "/api/players/"
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return 0, false
	}
	id, err := strconv.Atoi(parts[0])
	return id, err == nil && id >= 0
}

func readWebAction(w http.ResponseWriter, r *http.Request, dst *webActionRequest) error {
	if r.ContentLength > 4096 {
		return fmt.Errorf("body too large")
	}
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(dst)
}

func handlePlayerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "use POST"})
		return
	}
	id, ok := parsePlayerIDPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "invalid player action"})
		return
	}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	action := parts[len(parts)-1]
	var req webActionRequest
	if err := readWebAction(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}

	if action == "message" {
		msg := webReason(req.Message)
		if msg == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "message is required"})
			return
		}
		cmd := fmt.Sprintf("say %d %s", id, msg)
		_, acked, err := webRun(cmd, 2*time.Second, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	out, _, err := webRun("players", 3*time.Second, true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": "RCON unavailable"})
		return
	}
	p, found := findWebPlayer(out, id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "player is no longer connected"})
		return
	}
	req.Reason = webReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	if action == "kick" {
		cmd := fmt.Sprintf("kick %d %s", p.ID, req.Reason)
		_, acked, err := webRun(cmd, 1500*time.Millisecond, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	if action == "ban" {
		if req.Minutes < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "minutes must be 0 or greater"})
			return
		}
		doBan(w, p, req)
		return
	}

	writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown action"})
}

// parseBans parses BattlEye `bans` output, e.g.
//
//	GUID Bans:
//	[#] [GUID] [Minutes left] [Reason]
//	----------------------------------------
//	0  49aacf68461f67163d0a2ecb6a4cee94 perm testing
//
//	IP Bans:
//	...
//
// Only the GUID section is read; IP bans are ignored. ok is false when the
// "GUID Bans" header wasn't found, so callers never treat an unrecognised
// reply as an empty ban list.
func parseBans(out string) (bans []webBan, ok bool) {
	bans = []webBan{}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "guid bans"):
			section, ok = "guid", true
			continue
		case strings.HasPrefix(low, "ip bans"):
			section = "ip"
			continue
		}
		if section != "guid" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || !guidRe.MatchString(fields[1]) {
			continue
		}
		perm := strings.EqualFold(fields[2], "perm")
		if !perm {
			if _, err := strconv.Atoi(fields[2]); err != nil {
				continue
			}
		}
		bans = append(bans, webBan{
			Index:   idx,
			GUID:    strings.ToLower(fields[1]),
			Minutes: strings.ToLower(fields[2]),
			Perm:    perm,
			Reason:  strings.Join(fields[3:], " "),
		})
	}
	return bans, ok
}

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// fetchBans reads the live GUID ban list from BattlEye.
func fetchBans() ([]webBan, error) {
	out, acked, err := webRun("bans", 4*time.Second, true)
	if err != nil {
		return nil, err
	}
	if !acked {
		return nil, fmt.Errorf("RCON unavailable")
	}
	bans, ok := parseBans(out)
	if !ok {
		if len(out) > 500 {
			out = out[:500]
		}
		log.Printf("bans: unrecognised output: %q", out)
		return nil, fmt.Errorf("could not parse bans output")
	}
	return bans, nil
}

// syncBans reads the BattlEye list and mirrors it into the DB.
func syncBans() ([]webBan, error) {
	banOpMu.Lock()
	defer banOpMu.Unlock()
	bans, err := fetchBans()
	if err != nil {
		return nil, err
	}
	if err := globalDB.SyncBans(bans); err != nil {
		return bans, err
	}
	return bans, nil
}

func handleBansJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "use GET"})
		return
	}
	bans, err := syncBans()
	if bans == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("error syncing bans to DB: %v", err)
	}
	names, _ := globalDB.BanNames()
	// BattlEye may list one GUID several times; show it once.
	seen := map[string]bool{}
	list := make([]webBan, 0, len(bans))
	for _, b := range bans {
		if seen[b.GUID] {
			continue
		}
		seen[b.GUID] = true
		b.Name = names[b.GUID]
		if b.Name == "" {
			b.Name = "Unknown"
		}
		list = append(list, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": true, "bans": list, "count": len(list)})
}

// handleBanAction serves POST /api/bans/<guid>/unban.
func handleBanAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "use POST"})
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/bans/"), "/"), "/")
	if len(parts) != 2 || parts[1] != "unban" || !guidRe.MatchString(parts[0]) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "invalid ban action"})
		return
	}
	guid := strings.ToLower(parts[0])

	banOpMu.Lock()
	defer banOpMu.Unlock()

	// BattlEye indexes shift after every removal and the same GUID can be
	// listed more than once, so remove one entry at a time from a fresh list
	// until the GUID is gone.
	removed := 0
	for removed < 20 {
		bans, err := fetchBans()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": err.Error()})
			return
		}
		idx := -1
		for _, b := range bans {
			if b.GUID == guid {
				idx = b.Index
				break
			}
		}
		if idx < 0 {
			break
		}
		_, acked, err := webRun(fmt.Sprintf("removeBan %d", idx), 1500*time.Millisecond, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": err.Error()})
			return
		}
		if !acked {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": "removeBan was not acknowledged"})
			return
		}
		removed++
	}
	if removed >= 20 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": "ban still present after 20 removals"})
		return
	}

	saved := false
	_, saved, _ = webRun("writeBans", 1500*time.Millisecond, false)

	// The list was re-read after the last removal, so BattlEye no longer
	// has this GUID; now drop it from the database.
	if err := globalDB.DeleteBan(guid); err != nil {
		log.Printf("error deleting ban from DB: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "be_removed": true, "db_removed": false, "error": "unbanned in BattlEye but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "be_removed": removed > 0, "db_removed": true, "removed": removed, "saved": saved})
}

var baseTmpl = template.Must(template.New("base").Parse(`{{define "style"}}<style>
body { font-family: system-ui, sans-serif; margin: {{.PageMargin}}; background: #111; color: #eee; }
table { width: {{.TableWidth}}; border-collapse: collapse; table-layout: fixed; }
th, td { padding: {{.CellPadding}}; height: {{.RowHeight}}; border-bottom: 1px solid #333; text-align: left; font-size: {{.BodyFontSize}}; vertical-align: middle; overflow: hidden; }
th { font-size: {{.HeaderFontSize}}; }
button { margin-right: 5px; padding: {{.ButtonPadding}}; }
#status { margin-bottom: 15px; color: #aaa; }
.modal { display: none; position: fixed; inset: 0; background: #0009; align-items: center; justify-content: center; }
.box { background: #222; padding: {{.ModalPadding}}; border-radius: 8px; width: min({{.ModalWidth}}, 90vw); }
input, textarea { box-sizing: border-box; width: 100%; margin: 6px 0 14px; padding: {{.InputPadding}}; background: #111; color: #eee; border: 1px solid #555; }
.buttons { text-align: right; }
.mono { font-family: ui-monospace, Consolas, monospace; font-size: 12px; word-break: break-all; }
nav { margin-bottom: 16px; }
nav a { color: #8ab4f8; margin-right: 16px; text-decoration: none; }
nav a.active { color: #eee; font-weight: bold; }
</style>{{end}}`))

var playersPage = template.Must(template.Must(baseTmpl.Clone()).New("players").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Players</title>
{{template "style" .}}
<style>
td:nth-child(2) { white-space: nowrap; text-overflow: ellipsis; }
td:nth-child(3), td:nth-child(4) { white-space: nowrap; text-overflow: ellipsis; }
</style>
</head>
<body>
<nav><a class="active" href="/players">Players</a><a href="/bans">Server Bans</a></nav>
<h1>DayZ Players</h1>
<div id="status">Connecting...</div>
<table>
	<colgroup>
		<col style="width: {{.IDWidth}}">
		<col style="width: {{.PlayerWidth}}">
		<col style="width: {{.IPWidth}}">
		<col style="width: {{.GUIDWidth}}">
		<col style="width: {{.PingWidth}}">
		<col style="width: {{.ActionsWidth}}">
	</colgroup>
	<thead>
		<tr><th>ID</th><th>Player</th><th>IP</th><th>GUID</th><th>Ping</th><th>Actions</th></tr>
	</thead>
	<tbody id="players"></tbody>
</table>
<div id="modal" class="modal">
	<div class="box">
		<h2 id="title"></h2>
		<div id="durationWrap">
			<label>Duration (minutes)</label>
			<input id="minutes" type="number" min="0" step="1">
		</div>
		<label id="textLabel"></label>
		<textarea id="text" rows="4"></textarea>
		<div class="buttons">
			<button onclick="closeModal()">Cancel</button>
			<button id="submit">Submit</button>
		</div>
	</div>
</div>
<script>
let current = null;
let action = null;

function esc(s) {
	return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

async function api(url, opt = {}) { return fetch(url, opt); }

async function refresh() {
	try {
		let r = await api('/api/players');
		let d = await r.json();
		if (!d.ok) {
			document.getElementById('status').textContent = 'Server unavailable — retrying...';
			return;
		}
		document.getElementById('status').textContent = 'Connected — ' + d.count + ' player(s)';
		document.getElementById('players').innerHTML = d.players.map(function(p) {
			let n = esc(p.name);
			return '<tr>' +
				'<td>' + p.id + '</td>' +
				'<td>' + n + '</td>' +
				'<td class="mono">' + esc(p.ip) + '</td>' +
				'<td class="mono">' + esc(p.guid) + '</td>' +
				'<td>' + p.ping + '</td>' +
				'<td>' +
					'<button data-id="' + p.id + '" data-action="message" data-name="' + n + '" onclick="handleActionClick(this)">Message</button>' +
					'<button data-id="' + p.id + '" data-action="kick" data-name="' + n + '" onclick="handleActionClick(this)">Kick</button>' +
					'<button data-id="' + p.id + '" data-action="ban" data-name="' + n + '" onclick="handleActionClick(this)">Ban</button>' +
				'</td>' +
			'</tr>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable — retrying...';
	}
}

function handleActionClick(btn) {
	openModal(parseInt(btn.getAttribute('data-id'), 10), btn.getAttribute('data-action'), btn.getAttribute('data-name'));
}

function openModal(id, a, name) {
	current = id;
	action = a;
	document.getElementById('title').textContent = a[0].toUpperCase() + a.slice(1) + ' ' + name;
	document.getElementById('durationWrap').style.display = a === 'ban' ? 'block' : 'none';
	document.getElementById('textLabel').textContent = a === 'message' ? 'Message' : 'Reason';
	document.getElementById('text').value = '';
	document.getElementById('minutes').value = '';
	document.getElementById('modal').style.display = 'flex';
}

function closeModal() { document.getElementById('modal').style.display = 'none'; }

document.getElementById('submit').onclick = async () => {
	let body = action === 'message'
		? { message: document.getElementById('text').value }
		: { reason: document.getElementById('text').value, minutes: parseInt(document.getElementById('minutes').value, 10) };
	if (action === 'ban' && (!Number.isInteger(body.minutes) || body.minutes < 0)) {
		alert('Duration must be a whole number of minutes (0 = permanent).');
		return;
	}
	let r = await api('/api/players/' + current + '/' + action, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body)
	});
	let d = await r.json();
	if (!d.ok) {
		alert(d.error || 'Action failed');
		return;
	}
	closeModal();
	if (action === 'ban' && d.kicked === false) {
		alert('Player was banned, but the kick was not confirmed.');
	}
	refresh();
};

refresh();
setInterval(refresh, {{.RefreshInterval.Milliseconds}});
</script>
</body>
</html>`))

func handlePlayersPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := playersPage.Execute(w, webUI); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

var bansPage = template.Must(template.Must(baseTmpl.Clone()).New("bans").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Server Bans</title>
{{template "style" .}}
<style>
td:nth-child(2), td:nth-child(3), td:nth-child(5) { white-space: nowrap; text-overflow: ellipsis; }
</style>
</head>
<body>
<nav><a href="/players">Players</a><a class="active" href="/bans">Server Bans</a></nav>
<h1>Server Bans</h1>
<div id="status">Connecting...</div>
<table>
	<colgroup>
		<col style="width: {{.IDWidth}}">
		<col style="width: {{.PlayerWidth}}">
		<col style="width: {{.GUIDWidth}}">
		<col style="width: 130px">
		<col>
		<col style="width: 120px">
	</colgroup>
	<thead>
		<tr><th>#</th><th>Player</th><th>GUID</th><th>Time left</th><th>Reason</th><th>Actions</th></tr>
	</thead>
	<tbody id="bans"></tbody>
</table>
<script>
function esc(s) {
	return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function fmtMinutes(b) {
	if (b.perm) return 'Permanent';
	let m = parseInt(b.minutes, 10);
	if (isNaN(m)) return esc(b.minutes);
	let d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60);
	m = m % 60;
	return (d ? d + 'd ' : '') + (h ? h + 'h ' : '') + m + 'm';
}

async function refresh() {
	try {
		let r = await fetch('/api/bans');
		let d = await r.json();
		if (!d.ok) {
			document.getElementById('status').textContent = (d.error || 'Server unavailable') + ' — retrying...';
			return;
		}
		document.getElementById('status').textContent = 'Connected — ' + d.count + ' ban(s)';
		document.getElementById('bans').innerHTML = d.bans.map(function(b) {
			let n = esc(b.name);
			return '<tr>' +
				'<td>' + b.index + '</td>' +
				'<td title="' + n + '">' + n + '</td>' +
				'<td class="mono">' + esc(b.guid) + '</td>' +
				'<td>' + fmtMinutes(b) + '</td>' +
				'<td title="' + esc(b.reason) + '">' + esc(b.reason) + '</td>' +
				'<td><button data-guid="' + esc(b.guid) + '" data-name="' + n + '" onclick="unban(this)">Unban</button></td>' +
			'</tr>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable — retrying...';
	}
}

async function unban(btn) {
	let guid = btn.getAttribute('data-guid');
	let name = btn.getAttribute('data-name');
	if (!confirm('Unban ' + name + '?')) return;
	btn.disabled = true;
	try {
		let r = await fetch('/api/bans/' + encodeURIComponent(guid) + '/unban', { method: 'POST' });
		let d = await r.json();
		if (!d.ok) alert(d.error || 'Unban failed');
	} catch(e) {
		alert('Request failed');
	}
	refresh();
}

refresh();
setInterval(refresh, {{.BansRefreshInterval.Milliseconds}});
</script>
</body>
</html>`))

func handleBansPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := bansPage.Execute(w, webUI); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}
