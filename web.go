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

// ============================================================================
// web.go overview
// ----------------------------------------------------------------------------
// This file is the "management UI + JSON API" layer that sits on top of the
// RCON plumbing in main.go. It provides:
//
//   - Two HTML pages (/players and /bans) rendered from Go templates
//   - JSON endpoints the pages poll and call:
//       GET  /api/players                  list connected players
//       POST /api/players/<id>/message     private message to a player
//       POST /api/players/<id>/kick        kick a player
//       POST /api/players/<id>/ban         ban a player by GUID (+kick)
//       GET  /api/bans                     list GUID bans (also syncs to DB)
//       POST /api/bans/<guid>/unban        remove every ban entry for a GUID
//   - A shared, auto-reconnecting RCON session (runShared) for all of the above
//   - A background poller that keeps the SQLite DB in sync with the server
//
// Concurrency note: everything that mutates or mirrors the ban list takes
// banListLock, so a background sync can never act on a stale list.
// ============================================================================

// Player is one row of the BattlEye `players` output, also the JSON shape
// returned by /api/players.
type Player struct {
	ID   int    `json:"id"`   // BattlEye player number (used by kick/say)
	IP   string `json:"ip"`   // client IP (informational only)
	Port string `json:"port"` // client UDP port
	Ping int    `json:"ping"` // ping in ms
	GUID string `json:"guid"` // BattlEye GUID (32 hex chars), the identity used for bans
	Name string `json:"name"` // in-game name
}

// Ban is one entry of the GUID section of the BattlEye `bans` output, also the
// JSON shape returned by /api/bans.
type Ban struct {
	Index   int    `json:"index"`   // position in the BattlEye list (shifts after each removal!)
	GUID    string `json:"guid"`    // banned GUID, lower-cased
	Name    string `json:"name"`    // last-known player name, filled from the DB (BattlEye doesn't store it)
	Minutes string `json:"minutes"` // minutes left as text, or "perm"
	Perm    bool   `json:"perm"`    // true for permanent bans
	Reason  string `json:"reason"`
}

// pageSettings holds every tweakable layout value used by the HTML templates
// (widths, paddings, font sizes) plus the browser polling intervals. It's
// passed to the templates as their data object, so editing a value here
// changes the page without touching the HTML.
var pageSettings = struct {
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
	RefreshInterval     time.Duration // how often the players page polls /api/players
	BansRefreshInterval time.Duration // how often the bans page polls /api/bans
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

// ---------------------------------------------------------------------------
// Shared RCON session
// ---------------------------------------------------------------------------

// sharedConnection is the single long-lived RCON session used by the web
// layer and the background poller. The embedded Mutex protects the pointer
// (not the commands themselves; persistentConnection serialises those).
// Keepalive is disabled for it (see connectShared) because the 5s poller
// already generates traffic well inside BattlEye's idle timeout.
var sharedConnection = struct {
	sync.Mutex
	connection *persistentConnection
}{}

// connectShared returns the shared session, creating or replacing it as needed:
//   - healthy existing session -> returned as is
//   - existing but dead        -> closed, cleared, and a new one dialed
//   - none                     -> a new one is dialed
//
// The lock is held during dialing, so concurrent callers wait for one login
// rather than each opening their own.
func connectShared() (*persistentConnection, error) {
	sharedConnection.Lock()
	defer sharedConnection.Unlock()
	if sharedConnection.connection != nil && !sharedConnection.connection.isClosed() {
		return sharedConnection.connection, nil
	}
	if sharedConnection.connection != nil {
		sharedConnection.connection.close()
		sharedConnection.connection = nil
	}
	s, err := dialPersistent("web", false)
	if err != nil {
		return nil, err
	}
	sharedConnection.connection = s
	return s, nil
}

// runShared runs a command on the shared session with optional retry/reconnect.
//
//   - allowRetry=false: one attempt. Used for state-changing commands (kick,
//     ban, say, ...) so a slow-but-successful command is never repeated.
//   - allowRetry=true: up to 3 attempts with 1s pauses. Used for read-only
//     commands (players, bans) where repeating is harmless.
//
// After a failed attempt, if there was an error or the session no longer
// answers a probe (isResponsive), the session is discarded so the next
// attempt (or caller) logs in fresh. The pointer comparison under the lock
// avoids discarding a newer session another goroutine already created.
//
// Success means err == nil AND the server acknowledged the command.
func runShared(command string, wait time.Duration, allowRetry bool) (string, bool, error) {
	maxAttempts := 1
	if allowRetry {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		s, err := connectShared()
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
		// Drop the session if it errored or has gone silent.
		if err != nil || !s.isResponsive() {
			sharedConnection.Lock()
			if sharedConnection.connection == s {
				s.close()
				sharedConnection.connection = nil
			}
			sharedConnection.Unlock()
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

// ---------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------

// parsePlayers turns the text output of the `players` command into Player
// structs. Each data line looks like:
//
//	12   203.0.113.5:2304   47   a1b2c3...(OK)   Some Name
//
// Fields are whitespace-separated: id, ip:port, ping, guid(status), then the
// name (which may itself contain spaces, so all remaining fields are rejoined).
// The "(OK)"/"(?)" verification suffix is stripped from the GUID. Header lines,
// separators and the footer fail one of the checks and are skipped.
func parsePlayers(out string) []Player {
	players := []Player{}
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
		players = append(players, Player{ID: id, IP: host, Port: port, Ping: ping, GUID: guid, Name: name})
	}
	return players
}

// findPlayer parses `players` output and returns the player with the given ID.
// The bool is false if that ID isn't connected.
func findPlayer(out string, id int) (Player, bool) {
	for _, p := range parsePlayers(out) {
		if p.ID == id {
			return p, true
		}
	}
	return Player{}, false
}

// cleanReason trims whitespace and replaces CR/LF with spaces so user-supplied
// text can't break out of a single RCON command line.
func cleanReason(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// ---------------------------------------------------------------------------
// Banning
// ---------------------------------------------------------------------------

// banListLock serialises everything that changes or mirrors the ban list
// (ban, unban, and BattlEye->DB sync) so a sync can't act on a stale
// list and delete a ban that was just added.
var banListLock sync.Mutex

// banPlayer bans player p and writes the JSON response itself.
//
// Steps:
//  1. Lock the ban list.
//  2. `addBan <guid> <minutes> <reason>` (GUID-only ban). Failure or no ack
//     -> 502, nothing else happens.
//  3. Kick the player immediately with the same reason. The ban already
//     exists, so a failed kick is only logged and reported via "kicked".
//  4. Record GUID/name/IP/reason/perm in SQLite. If this fails the response is
//     500 with guid_ban=true, so the UI knows the in-game ban did succeed.
//
// BattlEye treats duration 0 as permanent. The IP is stored for reference
// only and is NOT banned.
func banPlayer(w http.ResponseWriter, p Player, req actionRequest) {
	banListLock.Lock()
	defer banListLock.Unlock()
	// BattlEye ban is GUID-only.
	guidCmd := fmt.Sprintf("addBan %s %d %s", p.GUID, req.Minutes, req.Reason)
	_, guidAck, guidErr := runShared(guidCmd, 1500*time.Millisecond, false)
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
	_, kickAck, kickErr := runShared(kickCmd, 1500*time.Millisecond, false)
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
	if err := globalDatabase.BanUser(p.GUID, p.Name, p.IP, req.Reason, perm); err != nil {
		log.Printf("error recording ban in DB: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "guid_ban": true, "db_updated": false, "kicked": kicked, "error": "GUID ban succeeded but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guid_ban": true, "db_updated": true, "kicked": kicked, "perm": perm == 1})
}

// ---------------------------------------------------------------------------
// Background sync
// ---------------------------------------------------------------------------

// startBackgroundSync tracks players (and periodically the ban list) in the
// background so the DB stays current without any web page open.
// It is the only caller of SyncPlayers.
//
// Every `interval`:
//   - Poll `players` (single attempt, 3s wait).
//   - On failure, count consecutive failures. At exactly 3 in a row the server
//     is assumed down and SyncPlayers(nil) closes all open sessions once, so
//     playtime stops accruing while it's offline.
//   - On success, reset the failure counter and sync the parsed list into the DB.
//   - On the 1st poll and every 6th after (~30s at a 5s interval), also
//     reconcile the ban list, since it changes rarely.
func startBackgroundSync(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		fails := 0
		n := 0
		for range t.C {
			n++
			out, acked, err := runShared("players", 3*time.Second, false)
			if err != nil || !acked {
				fails++
				// Server probably down: close open sessions once so
				// playtime doesn't keep accruing while it's offline.
				if fails == 3 {
					if err := globalDatabase.SyncPlayers(nil); err != nil {
						log.Printf("poller: close sessions: %v", err)
					}
				}
				continue
			}
			fails = 0
			if err := globalDatabase.SyncPlayers(parsePlayers(out)); err != nil {
				log.Printf("poller: sync: %v", err)
			}
			// Ban list changes rarely: reconcile at start and every 6th poll (~30s).
			if n%6 == 1 {
				if _, err := syncBanList(); err != nil {
					log.Printf("poller: bans: %v", err)
				}
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Player API
// ---------------------------------------------------------------------------

// handlePlayersJSON serves GET /api/players: the live player list.
// Uses retrying runShared since this is a read. If RCON can't be reached it
// returns 503 with retrying=true so the page shows "retrying..." instead of
// an error.
func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	out, acked, err := runShared("players", 3*time.Second, true)
	if err != nil || !acked {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": "RCON unavailable"})
		return
	}
	players := parsePlayers(out)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": true, "players": players, "count": len(players)})
}

// actionRequest is the JSON body for the per-player action endpoints.
// Which fields matter depends on the action:
//
//	kick    -> Reason
//	ban     -> Reason, Minutes (0 = permanent)
//	message -> Message
type actionRequest struct {
	Reason  string `json:"reason"`
	Minutes int    `json:"minutes"`
	Message string `json:"message"`
}

// parsePlayerPath validates a path of the form /api/players/<id>/<action>
// (an optional trailing slash is allowed) and returns the numeric id.
// ok is false for a wrong prefix, wrong segment count, or a non-numeric/negative id.
// The action segment is extracted separately by the caller.
func parsePlayerPath(path string) (int, bool) {
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

// readActionRequest decodes the JSON body into dst. The body is capped at
// 4 KiB (rejected up front by Content-Length, and enforced while reading by
// MaxBytesReader for requests that don't declare a length).
func readActionRequest(w http.ResponseWriter, r *http.Request, dst *actionRequest) error {
	if r.ContentLength > 4096 {
		return fmt.Errorf("body too large")
	}
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(dst)
}

// handlePlayerAction serves POST /api/players/<id>/<action> for
// action = message | kick | ban.
//
// Flow:
//  1. Validate method, path and JSON body.
//  2. "message": send `say <id> <text>` right away (no lookup needed).
//  3. Otherwise re-fetch `players` and make sure the target is still
//     connected. This both gets the GUID/name/IP needed for a ban and guards
//     against IDs having been reassigned since the page last refreshed.
//  4. Default an empty reason to "No reason specified".
//  5. "kick" runs `kick <id> <reason>`; "ban" validates minutes >= 0 and
//     delegates to banPlayer; anything else is 404.
func handlePlayerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	id, ok := parsePlayerPath(r.URL.Path)
	if !ok {
		fail(w, http.StatusNotFound, "invalid player action")
		return
	}
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	action := parts[len(parts)-1]
	var req actionRequest
	if err := readActionRequest(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}

	if action == "message" {
		msg := cleanReason(req.Message)
		if msg == "" {
			fail(w, http.StatusBadRequest, "message is required")
			return
		}
		cmd := fmt.Sprintf("say %d %s", id, msg)
		_, acked, err := runShared(cmd, 2*time.Second, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	// kick/ban: confirm the player is still there (retrying read).
	out, _, err := runShared("players", 3*time.Second, true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": "RCON unavailable"})
		return
	}
	p, found := findPlayer(out, id)
	if !found {
		fail(w, http.StatusNotFound, "player is no longer connected")
		return
	}
	req.Reason = cleanReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	if action == "kick" {
		cmd := fmt.Sprintf("kick %d %s", p.ID, req.Reason)
		_, acked, err := runShared(cmd, 1500*time.Millisecond, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	if action == "ban" {
		if req.Minutes < 0 {
			fail(w, http.StatusBadRequest, "minutes must be 0 or greater")
			return
		}
		banPlayer(w, p, req)
		return
	}

	fail(w, http.StatusNotFound, "unknown action")
}

// ---------------------------------------------------------------------------
// Ban list: parsing, reading, syncing
// ---------------------------------------------------------------------------

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
//
// The parser is a tiny state machine: `section` flips to "guid" or "ip" when
// a header line is seen, and only lines in the "guid" section are parsed.
// A data line needs: numeric index, a 32-hex GUID, then "perm" or a number
// of minutes; everything after that is the reason. Lines failing any check
// (column titles, dashes, blank lines) are skipped.
func parseBans(out string) (bans []Ban, ok bool) {
	bans = []Ban{}
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
		if err != nil || !guidPattern.MatchString(fields[1]) {
			continue
		}
		perm := strings.EqualFold(fields[2], "perm")
		if !perm {
			if _, err := strconv.Atoi(fields[2]); err != nil {
				continue
			}
		}
		bans = append(bans, Ban{
			Index:   idx,
			GUID:    strings.ToLower(fields[1]),
			Minutes: strings.ToLower(fields[2]),
			Perm:    perm,
			Reason:  strings.Join(fields[3:], " "),
		})
	}
	return bans, ok
}

// guidPattern matches a BattlEye GUID: exactly 32 hex characters. Used both to
// recognise ban rows and to validate the GUID in unban URLs.
var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// readBanList reads the live GUID ban list from BattlEye (retrying read,
// 4s wait). An unrecognised reply is logged (first 500 chars) and returned
// as an error rather than as an empty list.
func readBanList() ([]Ban, error) {
	out, acked, err := runShared("bans", 4*time.Second, true)
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

// syncBanList reads the BattlEye list and mirrors it into the DB.
// Holds banListLock for the whole read+mirror so it can't interleave with a
// ban/unban. Return values matter to callers:
//   - (nil, err)   -> couldn't read the live list at all
//   - (bans, err)  -> list was read fine, but the DB mirror failed
//   - (bans, nil)  -> fully synced
func syncBanList() ([]Ban, error) {
	banListLock.Lock()
	defer banListLock.Unlock()
	bans, err := readBanList()
	if err != nil {
		return nil, err
	}
	if err := globalDatabase.SyncBans(bans); err != nil {
		return bans, err
	}
	return bans, nil
}

// handleBansJSON serves GET /api/bans.
//
// It syncs first (so viewing the page refreshes the DB too). If the live list
// couldn't be read it returns 503; if only the DB mirror failed, it logs the
// problem and still serves the live list. Player names come from the DB since
// BattlEye only stores GUIDs; unknown GUIDs show as "Unknown". BattlEye can
// list one GUID multiple times, so duplicates are collapsed to the first.
func handleBansJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	bans, err := syncBanList()
	if bans == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("error syncing bans to DB: %v", err)
	}
	names, _ := globalDatabase.BanNames()
	// BattlEye may list one GUID several times; show it once.
	seen := map[string]bool{}
	list := make([]Ban, 0, len(bans))
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
//
// BattlEye removes bans by list index, and indexes shift after every removal;
// the same GUID can also appear more than once. So the handler loops:
//
//	re-read the list -> find the first entry for the GUID -> removeBan <index>
//
// until the GUID no longer appears (safety cap: 20 removals). Each iteration
// uses a fresh list, so a stale index is never used.
//
// After that it runs `writeBans` to persist the list to disk (best effort,
// reported as "saved") and deletes the GUID from the DB. Partial states are
// reported explicitly (be_removed / db_removed) so the UI/logs can tell what
// actually happened. The whole operation runs under banListLock.
func handleBanAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/bans/"), "/"), "/")
	if len(parts) != 2 || parts[1] != "unban" || !guidPattern.MatchString(parts[0]) {
		fail(w, http.StatusNotFound, "invalid ban action")
		return
	}
	guid := strings.ToLower(parts[0])

	banListLock.Lock()
	defer banListLock.Unlock()

	// BattlEye indexes shift after every removal and the same GUID can be
	// listed more than once, so remove one entry at a time from a fresh list
	// until the GUID is gone.
	removed := 0
	for removed < 20 {
		bans, err := readBanList()
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
			break // GUID no longer listed: done
		}
		_, acked, err := runShared(fmt.Sprintf("removeBan %d", idx), 1500*time.Millisecond, false)
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

	// Ask BattlEye to write bans.txt to disk. Best effort: only the ack is kept.
	saved := false
	_, saved, _ = runShared("writeBans", 1500*time.Millisecond, false)

	// The list was re-read after the last removal, so BattlEye no longer
	// has this GUID; now drop it from the database.
	if err := globalDatabase.DeleteBan(guid); err != nil {
		log.Printf("error deleting ban from DB: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "be_removed": true, "db_removed": false, "error": "unbanned in BattlEye but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "be_removed": removed > 0, "db_removed": true, "removed": removed, "saved": saved})
}

// ---------------------------------------------------------------------------
// HTML templates and page handlers
// ---------------------------------------------------------------------------

// baseTemplate defines a reusable "style" block (the shared CSS) that both
// pages include with {{template "style" .}}. Layout values come from
// pageSettings. Each page template below is created from a Clone() of this
// base so they share the "style" definition without affecting each other.
var baseTemplate = template.Must(template.New("base").Parse(`{{define "style"}}<style>
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

// playersPage is the /players page: a table of connected players that polls
// /api/players every RefreshInterval, with Message / Kick / Ban buttons that
// open one shared modal dialog.
//
// Client-side script outline:
//   - esc():              HTML-escapes any server data before it's put into innerHTML
//   - refresh():          fetches /api/players and rebuilds the table rows
//   - handleActionClick():reads data-* attributes off the clicked button and opens the modal
//   - openModal():        configures the modal for message/kick/ban (duration field only for ban)
//   - submit handler:     validates ban duration, POSTs JSON to /api/players/<id>/<action>,
//                         alerts on failure, warns if the ban succeeded but the kick didn't
//
// Note: player data is passed through data-* attributes rather than inline JS
// strings, which avoids quoting/injection problems with odd player names.
var playersPage = template.Must(template.Must(baseTemplate.Clone()).New("players").Parse(`<!doctype html>
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

// handlePlayersPage serves GET /players by rendering playersPage with the
// layout settings. Only GET is allowed. Rendering errors become a plain 500.
// (If Execute fails midway, part of the page may already have been sent.)
func handlePlayersPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := playersPage.Execute(w, pageSettings); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

// bansPage is the /bans page: a read-mostly table of GUID bans that polls
// /api/bans every BansRefreshInterval, with an Unban button per row.
//
// Client-side script outline:
//   - esc():         HTML-escapes server data before it's inserted
//   - fmtMinutes():  turns minutes-left into "Permanent" or "Xd Yh Zm"
//   - refresh():     fetches /api/bans and rebuilds the table; shows the
//                    server's error text if unavailable
//   - unban():       confirms, disables the button to prevent double clicks,
//                    POSTs to /api/bans/<guid>/unban, alerts on failure, and
//                    refreshes either way
var bansPage = template.Must(template.Must(baseTemplate.Clone()).New("bans").Parse(`<!doctype html>
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

// handleBansPage serves GET /bans by rendering bansPage. Same rules as
// handlePlayersPage: GET only, plain 500 on template errors.
func handleBansPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := bansPage.Execute(w, pageSettings); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}