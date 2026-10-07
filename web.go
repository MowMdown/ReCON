package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	//"regexp"
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

var webRCON = struct {
	sync.Mutex
	s *live
}{}

// Temporary: old regex-based web player parser.
// var webPlayerRow = regexp.MustCompile(`^\s*(\d+)\s+(\S+):(\d+)\s+(-?\d+)\s+(\S+)\s*(.*)$`)

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

	s, err := dialLive()
	if err != nil {
		return nil, err
	}

	webRCON.s = s
	return s, nil
}

// webRun executes commands with retry control based on action type.
// allowRetry is true for read-only calls (e.g. players), false for state-mutating actions.
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

		// Only throw the shared session away if it is really dead. A single
		// lost packet must not kill it for every other request using it.
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

		// Expected:
		// ID IP:PORT PING GUID NAME...
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

		// Do not validate or otherwise restrict the GUID contents.
		// It may contain '-' or other characters.
		guid := fields[3]

		// Remove BattlEye's status suffix, e.g. "(OK)" or "(?)".
		if idx := strings.IndexByte(guid, '('); idx != -1 {
			guid = guid[:idx]
		}

		name := strings.Join(fields[4:], " ")

		players = append(players, webPlayer{
			ID:   id,
			IP:   host,
			Port: port,
			Ping: ping,
			GUID: guid,
			Name: name,
		})
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

func webCommandAllowed(command string) bool {
	return permitted(command)
}

func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"ok":    false,
			"error": "use GET",
		})
		return
	}

	out, acked, err := webRun("players", 3*time.Second, true)
	if err != nil || !acked {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":        false,
			"connected": false,
			"retrying":  true,
			"error":     "RCON unavailable",
		})
		return
	}

	players := parseWebPlayers(out)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"connected": true,
		"players":   players,
		"count":     len(players),
	})
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

	return json.NewDecoder(
		http.MaxBytesReader(w, r.Body, 4096),
	).Decode(dst)
}

func handlePlayerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"ok":    false,
			"error": "use POST",
		})
		return
	}

	id, ok := parsePlayerIDPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "invalid player action",
		})
		return
	}

	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	action := parts[len(parts)-1]

	var req webActionRequest

	if err := readWebAction(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "bad JSON",
		})
		return
	}

	if action == "message" {
		msg := webReason(req.Message)

		if msg == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "message is required",
			})
			return
		}

		cmd := fmt.Sprintf("say %d %s", id, msg)

		if !webCommandAllowed("say") {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"ok":    false,
				"error": "command not allowed",
			})
			return
		}

		_, acked, err := webRun(cmd, 2*time.Second, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    acked,
			"acked": acked,
		})
		return
	}

	out, _, err := webRun("players", 3*time.Second, true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":        false,
			"connected": false,
			"retrying":  true,
			"error":     "RCON unavailable",
		})
		return
	}

	p, found := findWebPlayer(out, id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "player is no longer connected",
		})
		return
	}

	req.Reason = webReason(req.Reason)

	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	if action == "kick" {
		if !webCommandAllowed("kick") {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"ok":    false,
				"error": "command not allowed",
			})
			return
		}

		cmd := fmt.Sprintf("kick %d %s", p.ID, req.Reason)

		_, acked, err := webRun(cmd, 1500*time.Millisecond, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    acked,
			"acked": acked,
		})
		return
	}

	if action == "ban" {
		if req.Minutes <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "minutes must be a whole integer greater than 0",
			})
			return
		}

		if !webCommandAllowed("addBan") {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"ok":    false,
				"error": "command not allowed",
			})
			return
		}

		guidCmd := fmt.Sprintf(
			"addBan %s %d %s",
			p.GUID,
			req.Minutes,
			req.Reason,
		)

		ipCmd := fmt.Sprintf(
			"addBan %s %d %s",
			p.IP,
			req.Minutes,
			req.Reason,
		)

		_, guidAck, guidErr := webRun(
			guidCmd,
			1500*time.Millisecond,
			false,
		)

		_, ipAck, ipErr := webRun(
			ipCmd,
			1500*time.Millisecond,
			false,
		)

		ok := guidErr == nil &&
			ipErr == nil &&
			guidAck &&
			ipAck

		resp := map[string]any{
			"ok":       ok,
			"guid_ban": guidAck && guidErr == nil,
			"ip_ban":   ipAck && ipErr == nil,
		}

		if guidErr != nil {
			resp["guid_error"] = guidErr.Error()
		}

		if ipErr != nil {
			resp["ip_error"] = ipErr.Error()
		}

		code := http.StatusOK
		if !ok {
			code = http.StatusBadGateway
		}

		writeJSON(w, code, resp)
		return
	}

	writeJSON(w, http.StatusNotFound, map[string]any{
		"ok":    false,
		"error": "unknown action",
	})
}

var playersPage = template.Must(template.New("players").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>DayZ Players</title>
<style>body{font-family:system-ui,sans-serif;margin:24px;background:#111;color:#eee}table{width:100%;border-collapse:collapse}th,td{padding:9px;border-bottom:1px solid #333;text-align:left}button{margin-right:5px;padding:6px 10px}#status{margin-bottom:15px;color:#aaa}.modal{display:none;position:fixed;inset:0;background:#0009;align-items:center;justify-content:center}.box{background:#222;padding:20px;border-radius:8px;width:min(420px,90vw)}input,textarea{box-sizing:border-box;width:100%;margin:6px 0 14px;padding:9px;background:#111;color:#eee;border:1px solid #555}.buttons{text-align:right}</style></head>
<body><h1>DayZ Players</h1><div id="status">Connecting...</div><table><thead><tr><th>ID</th><th>Player</th><th>Ping</th><th>Actions</th></tr></thead><tbody id="players"></tbody></table>
<div id="modal" class="modal"><div class="box"><h2 id="title"></h2><div id="durationWrap"><label>Duration (minutes)</label><input id="minutes" type="number" min="1" step="1"></div><label id="textLabel"></label><textarea id="text" rows="4"></textarea><div class="buttons"><button onclick="closeModal()">Cancel</button><button id="submit">Submit</button></div></div></div>
<script>
let current=null, action=null;
function esc(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
async function api(url,opt={}){return fetch(url,opt)}
async function refresh(){try{let r=await api('/api/players');let d=await r.json();if(!d.ok){document.getElementById('status').textContent='Server unavailable — retrying...';return}document.getElementById('status').textContent='Connected — '+d.count+' player(s)';document.getElementById('players').innerHTML=d.players.map(function(p){let n=esc(p.name);return '<tr><td>'+p.id+'</td><td>'+n+'</td><td>'+p.ping+'</td><td><button data-id="'+p.id+'" data-action="message" data-name="'+n+'" onclick="handleActionClick(this)">Message</button><button data-id="'+p.id+'" data-action="kick" data-name="'+n+'" onclick="handleActionClick(this)">Kick</button><button data-id="'+p.id+'" data-action="ban" data-name="'+n+'" onclick="handleActionClick(this)">Ban</button></td></tr>';}).join('')}catch(e){document.getElementById('status').textContent='Server unavailable — retrying...'}}
function handleActionClick(btn){openModal(parseInt(btn.getAttribute('data-id'),10),btn.getAttribute('data-action'),btn.getAttribute('data-name'))}
function openModal(id,a,name){current=id;action=a;document.getElementById('title').textContent=a[0].toUpperCase()+a.slice(1)+' '+name;document.getElementById('durationWrap').style.display=a==='ban'?'block':'none';document.getElementById('textLabel').textContent=a==='message'?'Message':'Reason';document.getElementById('text').value='';document.getElementById('minutes').value='';document.getElementById('modal').style.display='flex';}
function closeModal(){document.getElementById('modal').style.display='none'}
document.getElementById('submit').onclick=async()=>{let body=action==='message'?{message:document.getElementById('text').value}:{reason:document.getElementById('text').value,minutes:parseInt(document.getElementById('minutes').value,10)};if(action==='ban'&&(!Number.isInteger(body.minutes)||body.minutes<=0)){alert('Duration must be a whole number of minutes greater than 0.');return}let r=await api('/api/players/'+current+'/'+action,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});let d=await r.json();if(!d.ok){alert(d.error||'Action failed');return}closeModal();refresh()};
refresh();setInterval(refresh,5000);
</script></body></html>`))

func handlePlayersPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	playersPage.Execute(w, nil)
}