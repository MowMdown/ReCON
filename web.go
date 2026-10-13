package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// web.go overview
// ----------------------------------------------------------------------------
// The HTTP layer. It turns requests into calls on the logic (db.go, flows.go)
// and on the game server (rcon.go), and turns the results into JSON or pages.
//
//   Raw RCON (for schedulers / scripts)
//       GET/POST /rcon                       run any RCON command
//       POST     /kickall                    kick everyone (optional lock/shutdown)
//   Players
//       GET  /players                        page
//       GET  /api/players                    online + offline players
//       GET  /api/players/all                every player ever seen (no RCON needed)
//       POST /api/players/<id>/message       private message to a player
//       POST /api/players/<id>/kick          kick a player
//       POST /api/players/<id>/ban           ban a player by GUID (+kick)
//   Bans
//       GET  /bans                           page
//       GET  /api/bans                       list GUID bans (also syncs to DB)
//       POST /api/bans/<guid>/ban            ban a known player by GUID, online or not
//       POST /api/bans/<guid>/unban          remove every ban entry for a GUID
//   Flows
//       GET  /flows                          page
//       GET  /api/flows                      flows + current/last run of each
//       POST /api/flows                      create/update {id, name, script, schedule, scheduleEnabled}
//       POST /api/flows/<id>/run             optional body {"minutes": N} = minutes until the cron time
//       POST /api/flows/<id>/stop
//       POST /api/flows/<id>/enable          (needs a schedule)   /disable
//       POST /api/flows/<id>/delete
//       POST /api/flows/preview              {schedule, script} -> plain-English description
//       POST /api/flows/reorder              {ids: [...]} saves the order shown on the page
//       POST /api/flows/command              {command: lock|unlock|shutdown|kickall} the page's quick buttons
//   GET /health
//
// Concurrency note: everything that mutates or mirrors the ban list takes
// banListLock (db.go), so a background sync can never act on a stale list.
// ============================================================================

// runWebServer registers every route and serves until the listener fails.
func runWebServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/rcon", handleRcon)                 // run any RCON command
	mux.HandleFunc("/kickall", handleKickAll)           // kick everyone (optionally lock/shutdown)
	mux.HandleFunc("/players", handlePlayersPage)       // players web page
	mux.HandleFunc("/api/players", handlePlayersJSON)   // players JSON list
	mux.HandleFunc("/api/players/all", handleAllPlayersJSON) // every known player, for the Ban Player picker
	mux.HandleFunc("/api/players/", handlePlayerAction)      // per-player actions (trailing slash = subtree)
	mux.HandleFunc("/bans", handleBansPage)             // bans web page
	mux.HandleFunc("/api/bans", handleBansJSON)         // bans JSON list
	mux.HandleFunc("/api/bans/", handleBanAction)       // per-GUID ban / unban
	mux.HandleFunc("/flows", handleFlowsPage)           // flows web page
	mux.HandleFunc("/api/flows", handleFlowsAPI)        // list / save flows
	mux.HandleFunc("/api/flows/", handleFlowAction)     // per-flow actions, preview, reorder, quick commands

	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second, // guards against slowloris-style header stalls
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      120 * time.Second, // generous: kickall can take a while
	}
	return srv.ListenAndServe()
}

// ---------------------------------------------------------------------------
// Shared HTTP helpers
// ---------------------------------------------------------------------------

// logRequests is HTTP middleware that logs each request (method, path+query,
// remote address, X-Forwarded-For if behind a proxy, user agent) before
// passing it on. Health checks and the pages' routine GET polling are skipped
// to keep the log readable.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quiet := r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/players" ||
			r.URL.Path == "/bans" || r.URL.Path == "/api/players" || r.URL.Path == "/api/bans" ||
			r.URL.Path == "/flows" || r.URL.Path == "/api/flows")
		if !quiet {
			from := r.RemoteAddr
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				from += " (xff " + xff + ")"
			}
			target := r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			log.Printf("http %s %s from %s ua=%q", r.Method, target, from, r.UserAgent())
		}
		next.ServeHTTP(w, r)
	})
}

// fail writes a JSON error body: {"ok": false, "error": msg} with status `code`.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// writeJSON sets the JSON content type, writes the status code, then encodes
// `v` as the response body. (Header must be set before WriteHeader.)
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// decodeBody decodes a JSON request body (capped at 16 KiB) into dst.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst)
}

// ---------------------------------------------------------------------------
// Raw RCON endpoints
// ---------------------------------------------------------------------------

// runCommand executes a single RCON command for an HTTP caller and writes the
// JSON result.
//   - empty command -> 400
//   - RCON failure  -> 502 (the upstream game server is the problem, not the request)
//   - success       -> 200 with {ok, command, acked, output}
func runCommand(w http.ResponseWriter, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		fail(w, 400, "no command")
		return
	}
	out, acked, err := sendOneShot("http", command, 2*time.Second)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "command": command, "acked": acked, "output": out})
}

// handleRcon serves /rcon and accepts the command in three ways, which makes it
// easy to call from simple tools (e.g. a scheduler's "POST to a URI" action):
//
//	GET  /rcon?cmd=players
//	POST /rcon                  body = raw command text
//	POST /rcon (JSON)           body = {"command": "players"}
//
// JSON parsing kicks in when the Content-Type header contains "json".
// Request bodies are capped at 4 KiB.
func handleRcon(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runCommand(w, r.URL.Query().Get("cmd"))
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			fail(w, 400, "bad body")
			return
		}
		cmd := string(raw)
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			var body struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				fail(w, 400, "bad JSON")
				return
			}
			cmd = body.Command
		}
		runCommand(w, cmd)
	default:
		fail(w, 405, "method not allowed")
	}
}

// handleKickAll serves POST /kickall. The optional JSON body is
//
//	{"reason": "...", "lock": true, "shutdown": true}
//
// An empty body is allowed (all defaults). `reason` defaults to
// "Server restarting". The kickReport is returned under "result".
func handleKickAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "use POST")
		return
	}
	var body struct {
		Reason   string `json:"reason"`
		Lock     bool   `json:"lock"`
		Shutdown bool   `json:"shutdown"`
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			fail(w, 400, "bad JSON")
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "Server restarting"
	}
	rep, err := kickAll("http-kickall", body.Reason, body.Lock, body.Shutdown)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
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

	offlinePlayers, err := globalDatabase.OfflinePlayers()
	if err != nil {
		log.Printf("error reading offline players from DB: %v", err)
		offlinePlayers = []Player{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"connected":      true,
		"players":        players,
		"count":          len(players),
		"offlinePlayers": offlinePlayers,
	})
}

// handleAllPlayersJSON serves GET /api/players/all: every player ever seen,
// from the database only, so it works while RCON is down. "offline" is false
// for players who were online at the last poll.
func handleAllPlayersJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	players, err := globalDatabase.KnownPlayers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "players": players, "count": len(players)})
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
		banPlayer(w, p, true, req)
		return
	}

	fail(w, http.StatusNotFound, "unknown action")
}

// banPlayer bans player p and writes the JSON response itself. online says
// whether p is connected right now (p.ID is only meaningful then).
//
// Steps:
//  1. Lock the ban list.
//  2. `addBan <guid> <minutes> <reason>` (GUID-only ban). Failure or no ack
//     -> 502, nothing else happens.
//  3. If online, kick the player immediately with the same reason. The ban
//     already exists, so a failed kick is only logged and reported via "kicked".
//  4. Record GUID/name/IP/reason/perm in SQLite. If this fails the response is
//     500 with guid_ban=true, so the UI knows the in-game ban did succeed.
//
// BattlEye treats duration 0 as permanent. The IP is stored for reference
// only and is NOT banned.
func banPlayer(w http.ResponseWriter, p Player, online bool, req actionRequest) {
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
	if online {
		kickCmd := fmt.Sprintf("kick %d %s", p.ID, req.Reason)
		_, kickAck, kickErr := runShared(kickCmd, 1500*time.Millisecond, false)
		kicked = kickErr == nil && kickAck
		if !kicked {
			log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
		}
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
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "guid_ban": true, "db_updated": false, "online": online, "kicked": kicked, "error": "GUID ban succeeded but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guid_ban": true, "db_updated": true, "online": online, "kicked": kicked, "perm": perm == 1})
}

// ---------------------------------------------------------------------------
// Ban API
// ---------------------------------------------------------------------------

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

// handleBanGUID serves POST /api/bans/<guid>/ban (routed by handleBanAction,
// which has already validated the GUID): the Bans page's "Ban Player" dialog.
// Optional body: {"reason": "text", "minutes": 0}. minutes defaults to 0,
// which is permanent; an empty body is a permanent ban with no reason given.
//
// The player can be online or offline, but must be known (in the live
// player list or the database), so the ban gets a name and IP on record.
// An online player is kicked right away, the same as the Players page ban.
func handleBanGUID(w http.ResponseWriter, r *http.Request, guid string) {
	var body struct {
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	if err := decodeBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if body.Minutes < 0 {
		fail(w, http.StatusBadRequest, "minutes must be 0 or greater")
		return
	}
	req := actionRequest{Reason: cleanReason(body.Reason), Minutes: body.Minutes}
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	// Online? Then we have the player number for the kick (retrying read).
	out, _, err := runShared("players", 3*time.Second, true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": "RCON unavailable"})
		return
	}
	for _, p := range parsePlayers(out) {
		if strings.EqualFold(p.GUID, guid) {
			banPlayer(w, p, true, req)
			return
		}
	}
	p, err := globalDatabase.PlayerByGUID(guid)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	banPlayer(w, p, false, req)
}

// handleBanAction serves POST /api/bans/<guid>/ban (see handleBanGUID) and
// POST /api/bans/<guid>/unban.
//
// Unban:
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
	if len(parts) != 2 || (parts[1] != "ban" && parts[1] != "unban") || !guidPattern.MatchString(parts[0]) {
		fail(w, http.StatusNotFound, "invalid ban action")
		return
	}
	guid := strings.ToLower(parts[0])
	if parts[1] == "ban" {
		handleBanGUID(w, r, guid)
		return
	}

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
// Flow API
// ---------------------------------------------------------------------------

// handleFlowsAPI serves GET (list) and POST (create/update) on /api/flows.
func handleFlowsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		flows, err := flowsWithNextRun()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": flows, "runs": snapshotRuns()})
	case http.MethodPost:
		var f Flow
		if err := decodeBody(w, r, &f); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON")
			return
		}
		id, err := validateAndSaveFlow(f)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		fail(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleFlowPreview serves POST /api/flows/preview: a plain-English summary of
// a schedule (and how the script's offsets fit around it) for the edit dialog.
func handleFlowPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule string `json:"schedule"`
		Script   string `json:"script"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	writeJSON(w, http.StatusOK, previewFlow(req.Schedule, req.Script))
}

// handleFlowReorder serves POST /api/flows/reorder: the body lists flow ids in
// the order they should appear.
func handleFlowReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeBody(w, r, &req); err != nil || len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if err := globalDatabase.ReorderFlows(req.IDs); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleQuickCommand serves POST /api/flows/command for the page's static
// buttons. Only these four commands are accepted (not arbitrary RCON text).
func handleQuickCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Command), "#"))
	switch name {
	case "lock", "unlock":
		_, acked, err := runShared("#"+name, 2*time.Second, true)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		if !acked {
			fail(w, http.StatusBadGateway, "#"+name+" was not acknowledged")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "#" + name + " sent"})
	case "shutdown":
		// The server may die before it acknowledges, so only a transport error is a failure.
		_, acked, err := runShared("#shutdown", 2*time.Second, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := kickAllUsing(sharedRunner{}, "Server restarting", false, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		msg := fmt.Sprintf("Kicked %d player(s)", len(rep.Order))
		if len(rep.Remaining) > 0 {
			msg += fmt.Sprintf(", %d still connected", len(rep.Remaining))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	default:
		fail(w, http.StatusBadRequest, "unknown command")
	}
}

// handleFlowAction serves POST /api/flows/<id>/<run|stop|enable|disable|delete>
// and the /api/flows/preview, /reorder and /command endpoints.
func handleFlowAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/preview" {
		handleFlowPreview(w, r)
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/command" {
		handleQuickCommand(w, r)
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/reorder" {
		handleFlowReorder(w, r)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/flows/"), "/"), "/")
	if len(parts) != 2 {
		fail(w, http.StatusNotFound, "invalid flow action")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusNotFound, "invalid flow id")
		return
	}
	f, err := globalDatabase.GetFlow(id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "flow not found")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch parts[1] {
	case "run":
		var body struct {
			Minutes int `json:"minutes"`
		}
		if r.ContentLength != 0 {
			if err := decodeBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
				fail(w, http.StatusBadRequest, "bad JSON")
				return
			}
		}
		if body.Minutes < 0 || body.Minutes > 10080 {
			fail(w, http.StatusBadRequest, "minutes must be 0-10080")
			return
		}
		if err := startFlow(f, body.Minutes); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errFlowRunning) {
				code = http.StatusConflict
			}
			fail(w, code, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "stop":
		if err := stopFlow(id); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "enable":
		if err := checkFlowCanEnable(f); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := globalDatabase.SetFlowScheduleEnabled(id, true); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "disable":
		if err := globalDatabase.SetFlowScheduleEnabled(id, false); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "delete":
		if err := deleteFlow(id); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, errFlowBusy) {
				code = http.StatusConflict
			}
			fail(w, code, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusNotFound, "unknown action")
	}
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

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
	RefreshInterval     time.Duration // how often the players and flows pages poll
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

// renderPage serves a GET-only HTML page from a template with the layout
// settings. Rendering errors become a plain 500. (If Execute fails midway,
// part of the page may already have been sent.)
func renderPage(w http.ResponseWriter, r *http.Request, page *template.Template) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, pageSettings); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

func handlePlayersPage(w http.ResponseWriter, r *http.Request) { renderPage(w, r, playersPage) }
func handleBansPage(w http.ResponseWriter, r *http.Request)    { renderPage(w, r, bansPage) }
func handleFlowsPage(w http.ResponseWriter, r *http.Request)   { renderPage(w, r, flowsPage) }

// baseTemplate defines a reusable "style" block (the shared CSS) that every
// page includes with {{template "style" .}}, plus the "reorder" script for
// drag-to-reorder table columns. Layout values come from pageSettings. Each
// page template below is created from a Clone() of this base so they share
// these definitions without affecting each other.
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
nav a#resetCols { float: right; margin-right: 0; color: #888; font-size: 13px; }
th[draggable="true"] { cursor: grab; user-select: none; }
th.dragging { opacity: 0.4; }
th.drag-over-left { box-shadow: inset 3px 0 0 #8ab4f8; }
th.drag-over-right { box-shadow: inset -3px 0 0 #8ab4f8; }
</style>{{end}}

{{define "reorder"}}<script>
// Drag-and-drop column reordering for every table on the page that has a
// <thead> and a <tbody id="...">. Drag a header onto another header to move
// the column. The order is remembered per table in localStorage, and is
// re-applied each time the table body is re-rendered by the polling code.
(function () {
	var resets = [];

	function readOrder(storeKey, keys) {
		try {
			var saved = JSON.parse(localStorage.getItem(storeKey) || 'null');
			if (Array.isArray(saved) && saved.length === keys.length &&
				keys.every(function (k) { return saved.indexOf(k) !== -1; })) {
				return saved;
			}
		} catch (e) {}
		return keys.slice();
	}

	function saveOrder(storeKey, order, isDefault) {
		try {
			if (isDefault) { localStorage.removeItem(storeKey); }
			else { localStorage.setItem(storeKey, JSON.stringify(order)); }
		} catch (e) {}
	}

	function setup(table) {
		var headRow = table.tHead && table.tHead.rows[0];
		var body = table.tBodies[0];
		if (!headRow || !body || !body.id) { return; }

		var ths = Array.prototype.slice.call(headRow.cells);
		var keys = ths.map(function (th) { return th.textContent.trim(); });
		var storeKey = 'cols:' + location.pathname + ':' + body.id;
		var order = readOrder(storeKey, keys);

		var thByKey = {};
		ths.forEach(function (th, i) { thByKey[keys[i]] = th; });

		// <col> elements follow their columns so the widths move too.
		var colgroup = table.querySelector('colgroup');
		var cols = colgroup ? Array.prototype.slice.call(colgroup.children) : [];
		var colByKey = null;
		if (cols.length === keys.length) {
			colByKey = {};
			cols.forEach(function (c, i) { colByKey[keys[i]] = c; });
		}

		// Body rows are rebuilt in the default order by the page script, so each
		// cell is tagged with its default column key the first time it is seen.
		function reorderRow(row) {
			var cells = Array.prototype.slice.call(row.children);
			if (cells.length !== keys.length) { return; } // e.g. a one-cell message row
			var map = {};
			cells.forEach(function (c, i) {
				if (!c.dataset.ck) { c.dataset.ck = keys[i]; }
				map[c.dataset.ck] = c;
			});
			order.forEach(function (k) { if (map[k]) { row.appendChild(map[k]); } });
		}

		function applyBody() { Array.prototype.forEach.call(body.rows, reorderRow); }

		function applyAll() {
			order.forEach(function (k) { headRow.appendChild(thByKey[k]); });
			if (colByKey) { order.forEach(function (k) { colgroup.appendChild(colByKey[k]); }); }
			applyBody();
		}

		// Re-apply whenever the page script replaces the rows. Moving cells inside
		// a row doesn't change the tbody's own children, so this can't loop.
		new MutationObserver(applyBody).observe(body, { childList: true });

		var dragKey = null;
		function clearMarks() {
			ths.forEach(function (t) { t.classList.remove('dragging', 'drag-over-left', 'drag-over-right'); });
		}

		ths.forEach(function (th, i) {
			var key = keys[i];
			th.draggable = true;
			th.title = 'Drag to reorder';

			th.addEventListener('dragstart', function (e) {
				dragKey = key;
				e.dataTransfer.effectAllowed = 'move';
				e.dataTransfer.setData('text/plain', key); // Firefox needs data to start a drag
				th.classList.add('dragging');
			});
			th.addEventListener('dragend', function () { dragKey = null; clearMarks(); });
			th.addEventListener('dragover', function (e) {
				if (dragKey === null || dragKey === key) { return; }
				e.preventDefault();
				e.dataTransfer.dropEffect = 'move';
				var movingRight = order.indexOf(dragKey) < order.indexOf(key);
				th.classList.toggle('drag-over-right', movingRight);
				th.classList.toggle('drag-over-left', !movingRight);
			});
			th.addEventListener('dragleave', function () {
				th.classList.remove('drag-over-left', 'drag-over-right');
			});
			th.addEventListener('drop', function (e) {
				e.preventDefault();
				if (dragKey === null || dragKey === key) { clearMarks(); return; }
				var from = order.indexOf(dragKey);
				var to = order.indexOf(key);
				order.splice(from, 1);
				order.splice(to, 0, dragKey);
				clearMarks();
				dragKey = null;
				saveOrder(storeKey, order, order.join('\u0000') === keys.join('\u0000'));
				applyAll();
			});
		});

		resets.push(function () {
			order = keys.slice();
			saveOrder(storeKey, order, true);
			applyAll();
		});
		applyAll();
	}

	Array.prototype.forEach.call(document.querySelectorAll('table'), setup);

	var link = document.getElementById('resetCols');
	if (link) {
		link.addEventListener('click', function (e) {
			e.preventDefault();
			resets.forEach(function (fn) { fn(); });
		});
	}
})();
</script>{{end}}`))

// playersPage is the /players page: a table of connected players that polls
// /api/players every RefreshInterval, with Message / Kick / Ban buttons that
// open one shared modal dialog, plus a table of offline players from the DB.
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
.offline-section { margin-top: 42px; }
</style>
</head>
<body>
<nav><a class="active" href="/players">Players</a><a href="/bans">Server Bans</a><a href="/flows">Flows</a><a href="#" id="resetCols">Reset columns</a></nav>
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

<div class="offline-section">
	<h2>Offline Players</h2>
	<table>
		<colgroup>
			<col style="width: {{.PlayerWidth}}">
			<col style="width: {{.IPWidth}}">
			<col style="width: {{.GUIDWidth}}">
			<col style="width: 130px">
			<col style="width: 210px">
		</colgroup>
		<thead>
			<tr><th>Player</th><th>Last IP</th><th>GUID</th><th>Time On Server</th><th>Last Seen</th></tr>
		</thead>
		<tbody id="offlinePlayers"></tbody>
	</table>
</div>

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

		// Render offline players returned by the database.
		document.getElementById('offlinePlayers').innerHTML = (d.offlinePlayers || []).map(function(p) {
			let seconds = Math.max(0, Number(p.totalPlaytimeSeconds) || 0);
			let days = Math.floor(seconds / 86400);
			let hours = Math.floor((seconds % 86400) / 3600);
			let minutes = Math.floor((seconds % 3600) / 60);
			let playtime = (days ? days + 'd ' : '') + (hours ? hours + 'h ' : '') + minutes + 'm';

			return '<tr>' +
				'<td>' + esc(p.name) + '</td>' +
				'<td class="mono">' + esc(p.ip) + '</td>' +
				'<td class="mono">' + esc(p.guid) + '</td>' +
				'<td>' + playtime + '</td>' +
				'<td>' + fmtLastSeen(p.lastSeenUnix) + '</td>' +
			'</tr>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable — retrying...';
	}
}

// fmtLastSeen formats Unix seconds as MM-DD-YYYY - HH:MM am|pm in the
// viewer's local time zone. Returns a dash when there is no timestamp.
function fmtLastSeen(unix) {
	if (!unix || unix <= 0) return '—';
	let d = new Date(unix * 1000);
	let pad = function(n) { return String(n).padStart(2, '0'); };
	let h = d.getHours();
	let ampm = h >= 12 ? 'pm' : 'am';
	h = h % 12 || 12;
	return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + '-' + d.getFullYear() +
		' - ' + pad(h) + ':' + pad(d.getMinutes()) + ' ' + ampm;
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
{{template "reorder" .}}
</body>
</html>`))

// bansPage is the /bans page: a table of GUID bans that polls /api/bans every
// BansRefreshInterval, with an Unban button per row and a "Ban Player" button
// that opens a dialog to ban any known player (online or not) by GUID.
//
// Client-side script outline:
//   - esc():         HTML-escapes server data before it's inserted
//   - fmtMinutes():  turns minutes-left into "Permanent" or "Xd Yh Zm"
//   - refresh():     fetches /api/bans and rebuilds the table; shows the
//                    server's error text if unavailable
//   - unban():       confirms, disables the button to prevent double clicks,
//                    POSTs to /api/bans/<guid>/unban, alerts on failure, and
//                    refreshes either way
//   - openBan():     loads every known player from /api/players/all into the
//                    picker, leaving out GUIDs that are already banned
//   - ban submit:    POSTs {reason, minutes} to /api/bans/<guid>/ban
var bansPage = template.Must(template.Must(baseTemplate.Clone()).New("bans").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Server Bans</title>
{{template "style" .}}
<style>
td:nth-child(2), td:nth-child(3), td:nth-child(5) { white-space: nowrap; text-overflow: ellipsis; }
.top { display: flex; justify-content: space-between; align-items: center; gap: 16px; flex-wrap: wrap; }
.top button { margin-right: 0; }
select { box-sizing: border-box; width: 100%; margin: 6px 0 14px; padding: {{.InputPadding}}; background: #111; color: #eee; border: 1px solid #555; }
</style>
</head>
<body>
<nav><a href="/players">Players</a><a class="active" href="/bans">Server Bans</a><a href="/flows">Flows</a><a href="#" id="resetCols">Reset columns</a></nav>
<div class="top">
	<h1>Server Bans</h1>
	<button onclick="openBan()">Ban Player</button>
</div>
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

<div id="banModal" class="modal" onclick="if (event.target === this) closeBan()">
	<div class="box">
		<h2>Ban Player</h2>
		<label>Player</label>
		<select id="banPlayer"></select>
		<label>Duration (minutes, 0 = permanent)</label>
		<input id="banMinutes" type="number" min="0" step="1" value="0">
		<label>Reason</label>
		<input id="banReason" type="text" maxlength="200" placeholder="Short message shown to the player">
		<div class="buttons">
			<button onclick="closeBan()">Cancel</button>
			<button id="banSubmit">Ban</button>
		</div>
	</div>
</div>
<script>
let banned = {}; // GUIDs on the current ban list, so the picker can skip them

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
		banned = {};
		d.bans.forEach(function(b) { banned[b.guid] = true; });
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

async function openBan() {
	let sel = document.getElementById('banPlayer');
	sel.innerHTML = '<option value="">Loading players...</option>';
	document.getElementById('banMinutes').value = '0';
	document.getElementById('banReason').value = '';
	document.getElementById('banModal').style.display = 'flex';
	try {
		let r = await fetch('/api/players/all');
		let d = await r.json();
		if (!d.ok) throw new Error(d.error);
		let list = d.players.filter(function(p) { return !banned[String(p.guid).toLowerCase()]; });
		if (!list.length) {
			sel.innerHTML = '<option value="">No players left to ban</option>';
			return;
		}
		sel.innerHTML = '<option value="">Select a player...</option>' + list.map(function(p) {
			return '<option value="' + esc(p.guid) + '">' + esc(p.name) + (p.offline ? '' : ' (online)') +
				' — ' + esc(String(p.guid).slice(0, 8)) + '</option>';
		}).join('');
	} catch(e) {
		sel.innerHTML = '<option value="">Could not load players</option>';
	}
}

function closeBan() { document.getElementById('banModal').style.display = 'none'; }

document.getElementById('banSubmit').onclick = async function() {
	let btn = this;
	let guid = document.getElementById('banPlayer').value;
	let minutes = parseInt(document.getElementById('banMinutes').value, 10);
	if (!guid) { alert('Select a player first.'); return; }
	if (!Number.isInteger(minutes) || minutes < 0) {
		alert('Duration must be a whole number of minutes (0 = permanent).');
		return;
	}
	btn.disabled = true;
	try {
		let r = await fetch('/api/bans/' + encodeURIComponent(guid) + '/ban', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ reason: document.getElementById('banReason').value, minutes: minutes })
		});
		let d = await r.json();
		if (!d.ok) {
			alert(d.error || d.guid_error || 'Ban failed');
			return;
		}
		closeBan();
		if (d.online && !d.kicked) alert('Player was banned, but the kick was not confirmed.');
	} catch(e) {
		alert('Request failed');
	} finally {
		btn.disabled = false;
		refresh();
	}
};

refresh();
setInterval(refresh, {{.BansRefreshInterval.Milliseconds}});
</script>
{{template "reorder" .}}
</body>
</html>`))

// flowsPage is the /flows page: the list of flows with Enable/Run/Edit/Delete
// buttons, drag-to-reorder rows, the run logs, an edit dialog with a live
// schedule preview, a help dialog, and quick #lock/#unlock/#shutdown/#kickall
// buttons. It polls /api/flows every RefreshInterval.
var flowsPage = template.Must(template.Must(baseTemplate.Clone()).New("flows").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Flows</title>
{{template "style" .}}
<style>
.box.wide { width: min(760px, 95vw); }
.top { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; flex-wrap: wrap; }
.top h1 { margin: 0 0 8px; }
.quick { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.quick button { margin-right: 0; }
#quickMsg { color: #aaa; font-size: 13px; margin-right: 8px; }
.box.helpbox { width: min(820px, 95vw); max-height: 85vh; overflow-y: auto; }
.helpbox h3 { margin: 18px 0 6px; font-size: 15px; }
.helpbox p, .helpbox li { font-size: 13px; line-height: 1.6; color: #ccc; }
.helpbox ul { margin: 4px 0; padding-left: 20px; }
.helpbox code { font-family: ui-monospace, Consolas, monospace; font-size: 12px; background: #111; padding: 1px 5px; border-radius: 3px; }
.helpbox pre { background: #111; padding: 10px; border-radius: 6px; font-size: 12px; overflow-x: auto; margin: 6px 0; }
textarea.script, #sched { font-family: ui-monospace, Consolas, monospace; font-size: 13px; }
textarea.script { white-space: pre; }
.help { color: #aaa; font-size: 13px; line-height: 1.6; margin-bottom: 10px; }
.help.bad { color: #f28b82; }
.run { margin-top: 24px; }
pre.log { background: #1a1a1a; padding: 10px; border-radius: 6px; font-size: 12px; white-space: pre-wrap; margin: 6px 0 0; }
td.actions { white-space: nowrap; }
button:disabled { opacity: 0.4; cursor: default; }
#flows tr[draggable="true"] { cursor: grab; }
#flows tr.dragging { opacity: 0.4; }
#flows tr.drop-above td { box-shadow: inset 0 3px 0 #8ab4f8; }
#flows tr.drop-below td { box-shadow: inset 0 -3px 0 #8ab4f8; }
</style>
</head>
<body>
<nav><a href="/players">Players</a><a href="/bans">Server Bans</a><a class="active" href="/flows">Flows</a></nav>
<div class="top">
	<h1>Flows</h1>
	<div class="quick">
		<span id="quickMsg"></span>
		<button onclick="quick('lock')">#Lock</button>
		<button onclick="quick('unlock')">#Unlock</button>
		<button onclick="quick('shutdown')">#Shutdown</button>
		<button onclick="quick('kickall')">#Kick All</button>
	</div>
</div>
<div id="status">Connecting...</div>
<p><button onclick="newFlow()">New flow</button><button onclick="openHelp()">Help</button></p>
<table>
	<colgroup>
		<col style="width: 200px">
		<col style="width: 140px">
		<col>
		<col style="width: 190px">
		<col style="width: 390px">
	</colgroup>
	<thead>
		<tr><th>Flow</th><th>Schedule</th><th>Status</th><th>Next run</th><th>Actions</th></tr>
	</thead>
	<tbody id="flows"></tbody>
</table>
<div id="runs"></div>

<div id="modal" class="modal">
	<div class="box wide">
		<h2 id="title"></h2>
		<label>Name</label>
		<input id="name" type="text">
		<textarea id="script" class="script" rows="14" spellcheck="false"></textarea>
		<label>Schedule (cron: minute hour day-of-month month day-of-week; blank or * * * * * = run when a player joins)</label>
		<input id="sched" type="text" placeholder="0 6 * * *" spellcheck="false">
		<div id="schedHelp" class="help"></div>
		<div class="buttons">
			<button onclick="closeModal()">Cancel</button>
			<button id="save">Save</button>
		</div>
	</div>
</div>
<div id="confirmModal" class="modal" onclick="if (event.target === this) closeConfirm()">
	<div class="box">
		<h2 id="confirmTitle"></h2>
		<p id="confirmText"></p>
		<div class="buttons">
			<button onclick="closeConfirm()">Cancel</button>
			<button id="confirmOk">Confirm</button>
		</div>
	</div>
</div>
<div id="helpModal" class="modal" onclick="if (event.target === this) closeHelp()">
	<div class="box helpbox">
		<h2>Flow help</h2>

		<h3>What a flow is</h3>
		<p>A flow is a saved list of steps that run around a schedule. When the schedule (a cron expression) fires, each step runs at its own offset from that time: before it, on it, or after it. Flows only run on their schedule while they are <b>Enabled</b>; <b>Run</b> starts one by hand.</p>

		<h3>Writing steps</h3>
		<p>One step per line: <code>offset | command</code>. Lines starting with <code>//</code> are comments.</p>
<pre>-60m | say -1 Server is going to restart in $minutes minutes!
-3m  | #lock
-1m  | #kickall Restarting now
0m   | #shutdown
+90  | #unlock
cancel | say -1 Server restart cancelled.</pre>
		<ul>
			<li><code>-60m</code> runs 60 minutes <b>before</b> the schedule time, <code>0m</code> runs <b>on</b> it, <code>+90</code> runs 90 seconds <b>after</b> it.</li>
			<li>Units are <code>s</code>, <code>m</code> and <code>h</code>. No unit means seconds, so <code>-90</code> is 90 seconds before.</li>
			<li>Any offset other than 0 needs a <code>+</code> or <code>-</code> sign.</li>
			<li><code>$minutes</code> and <code>$seconds</code> in a command become how long before the schedule time that step runs (0 for steps on or after it).</li>
			<li><code>cancel | command</code> lines run only if you press Stop before the schedule time arrives. Use them to undo a countdown, for example a "restart cancelled" message.</li>
		</ul>

		<h3>Player-join flows</h3>
		<p>Leave the schedule blank (or use <code>* * * * *</code>) and <b>Enable</b> the flow, and it runs once for every player who joins instead of on the clock. Joins are checked every 5 seconds.</p>
<pre>0s   | say $player Welcome to our server, $name!
+60  | say $player Type /help for the rules.</pre>
		<ul>
			<li><code>$player</code> becomes the joining player's number (what <code>say</code> and <code>kick</code> need) and <code>$name</code> their name.</li>
			<li>Offsets count from when the join was seen, so use <code>0s</code> or <code>+</code> offsets only.</li>
			<li>A player who is still loading in may miss a <code>0s</code> message; <code>+30</code> or so is more reliable.</li>
			<li>Players already online when the app starts are not greeted. After the server restarts, everyone who reconnects is.</li>
			<li>Flows using <code>$player</code> or <code>$name</code> can't be started with Run or given a clock schedule.</li>
		</ul>

		<h3>Commands you can use</h3>
		<ul>
			<li>Any BattlEye RCON command, such as <code>say -1 text</code>, <code>#lock</code>, <code>#unlock</code>, <code>#shutdown</code> or <code>kick 3 reason</code>.</li>
			<li><code>kickall [reason]</code> or <code>#kickall [reason]</code> kicks every connected player. With no reason it uses "Server restarting".</li>
		</ul>

		<h3>Schedule (cron)</h3>
		<p>Five fields: <code>minute hour day-of-month month day-of-week</code>, in the server's time zone. The text under the field explains in plain English what you typed and shows the next runs. The schedule time is the <code>0m</code> point, so a flow starting at <code>-60m</code> begins its countdown an hour earlier.</p>
		<ul>
			<li><code>0 6 * * *</code> every day at 06:00</li>
			<li><code>0 */6 * * *</code> every 6 hours (00:00, 06:00, 12:00, 18:00)</li>
			<li><code>30 4 * * 1,4</code> Mondays and Thursdays at 04:30</li>
			<li><code>0 5 * * 1-5</code> weekdays at 05:00</li>
			<li><code>@daily</code>, <code>@hourly</code>, <code>@weekly</code>, <code>@monthly</code> shortcuts also work.</li>
		</ul>

		<h3>Buttons</h3>
		<ul>
			<li><b>Enable / Disable</b> turns the flow on or off. An enabled flow with a schedule runs on the clock; one without runs when a player joins.</li>
			<li><b>Run / Stop</b> switches with the flow's state. Run starts the flow immediately: the earliest step fires right away and the rest follow at their offsets. Stop halts a running flow.</li>
			<li><b>Edit</b> changes the steps or schedule. <b>Delete</b> removes the flow (not while it is running).</li>
			<li>Drag a row up or down to reorder the list.</li>
			<li>The <b>#Lock</b>, <b>#Unlock</b>, <b>#Shutdown</b> and <b>#Kick All</b> buttons at the top right send that command to the server immediately. Shutdown and Kick All open a confirm/cancel dialog first.</li>
		</ul>

		<h3>Tips</h3>
		<ul>
			<li>Put <code>#kickall</code> at least a minute before <code>0m | #shutdown</code>, since the sweep takes a few seconds.</li>
			<li>Set <code>+90</code> (or more) for <code>#unlock</code> to leave time for the server to restart and RCON to log in again.</li>
			<li>Use <code>say -1</code> for messages to everyone; <code>say &lt;id&gt;</code> messages one player.</li>
			<li>If the app restarts during a scheduled countdown it rejoins it and skips steps whose time has passed. Manually started runs are not resumed.</li>
			<li>A flow runs one copy at a time. To start one from a task scheduler, POST to <code>/api/flows/&lt;id&gt;/run</code>.</li>
		</ul>

		<div class="buttons"><button onclick="closeHelp()">Close</button></div>
	</div>
</div>
<script>
let flows = [];
let editing = null;
let previewTimer = null;
let dragging = false;
let dragId = null;

function esc(s) {
	return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

async function post(url, body) {
	let r = await fetch(url, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body || {})
	});
	return r.json();
}

function fmtRemaining(unix) {
	let s = Math.round(unix - Date.now() / 1000);
	if (s <= 0) return 'now';
	let h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
	return (h ? h + 'h ' : '') + m + 'm ' + String(s % 60).padStart(2, '0') + 's';
}

function statusText(run) {
	if (!run) return 'Idle';
	if (run.active) {
		if (run.anchorUnix > Date.now() / 1000) return 'Running - schedule time in ' + fmtRemaining(run.anchorUnix);
		return 'Running - finishing up';
	}
	return 'Last run: ' + run.status;
}

function btn(label, onclick, disabled) {
	return '<button' + (disabled ? ' disabled' : '') + ' onclick="' + onclick + '">' + label + '</button>';
}

async function refresh() {
	if (dragging) return; // do not rebuild rows under a drag in progress
	try {
		let r = await fetch('/api/flows');
		let d = await r.json();
		if (!d.ok) {
			document.getElementById('status').textContent = (d.error || 'Server unavailable') + ' — retrying...';
			return;
		}
		flows = d.flows || [];
		let runs = {};
		(d.runs || []).forEach(function(x) { runs[x.flowId] = x; });
		document.getElementById('status').textContent = 'Connected — ' + flows.length + ' flow(s)';
		document.getElementById('flows').innerHTML = flows.map(function(f) {
			let run = runs[f.id];
			let active = !!(run && run.active);
			let hasCron = !!(f.schedule && f.schedule.trim());
			let next = f.scheduleEnabled ? (f.nextRun || '-') : (hasCron ? 'Disabled' : '-');
			let buttons =
				btn(f.scheduleEnabled ? 'Disable' : 'Enable', 'toggleFlow(' + f.id + ')', false) +
				(active ? btn('Stop', 'stopFlow(' + f.id + ')', false) : btn('Run', 'runFlow(' + f.id + ')', false)) +
				btn('Edit', 'editFlow(' + f.id + ')', false) +
				btn('Delete', 'deleteFlow(' + f.id + ')', active);
			return '<tr draggable="true" data-id="' + f.id + '"><td>' + esc(f.name) + '</td>' +
				'<td class="mono">' + (hasCron ? esc(f.schedule) : '-') + '</td>' +
				'<td>' + esc(statusText(run)) + '</td>' +
				'<td>' + esc(next) + '</td>' +
				'<td class="actions">' + buttons + '</td></tr>';
		}).join('');
		document.getElementById('runs').innerHTML = flows.filter(function(f) { return runs[f.id]; }).map(function(f) {
			let x = runs[f.id];
			return '<div class="run"><b>' + esc(f.name) + '</b> - ' + esc(statusText(x)) +
				'<pre class="log">' + esc((x.log || []).join('\n')) + '</pre></div>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable — retrying...';
	}
}

function find(id) { return flows.find(function(x) { return x.id === id; }); }

function isJoinFlow(f) {
	let s = (f.schedule || '').trim().split(/\s+/).join(' ');
	return s === '' || s === '* * * * *';
}

async function toggleFlow(id) {
	let f = find(id);
	if (!f.scheduleEnabled && isJoinFlow(f) &&
		!confirm('"' + f.name + '" has no clock schedule, so it will run every time a player joins. Enable it?')) return;
	let d = await post('/api/flows/' + id + '/' + (f.scheduleEnabled ? 'disable' : 'enable'));
	if (!d.ok) alert(d.error || 'Failed');
	refresh();
}

async function runFlow(id) {
	let d = await post('/api/flows/' + id + '/run');
	if (!d.ok) alert(d.error || 'Run failed');
	refresh();
}

async function stopFlow(id) {
	if (!confirm('Stop the running flow?')) return;
	let d = await post('/api/flows/' + id + '/stop');
	if (!d.ok) alert(d.error || 'Stop failed');
	refresh();
}

async function deleteFlow(id) {
	let f = find(id);
	if (!confirm('Delete flow "' + f.name + '"?')) return;
	let d = await post('/api/flows/' + id + '/delete');
	if (!d.ok) alert(d.error || 'Delete failed');
	refresh();
}

let quickTimer = null;

const quickConfirm = {
	shutdown: { title: 'Shut down server?', text: 'This sends #shutdown to the server immediately.', ok: 'Shut down' },
	kickall: { title: 'Kick all players?', text: 'This kicks every connected player immediately.', ok: 'Kick all' }
};

function quick(cmd) {
	let c = quickConfirm[cmd];
	if (!c) { sendQuick(cmd); return; }
	document.getElementById('confirmTitle').textContent = c.title;
	document.getElementById('confirmText').textContent = c.text;
	let ok = document.getElementById('confirmOk');
	ok.textContent = c.ok;
	ok.onclick = function() { closeConfirm(); sendQuick(cmd); };
	document.getElementById('confirmModal').style.display = 'flex';
}

function closeConfirm() { document.getElementById('confirmModal').style.display = 'none'; }

async function sendQuick(cmd) {
	let msg = document.getElementById('quickMsg');
	msg.textContent = 'Sending...';
	try {
		let d = await post('/api/flows/command', { command: cmd });
		msg.textContent = d.ok ? (d.message || 'Sent') : (d.error || 'Failed');
	} catch(e) {
		msg.textContent = 'Request failed';
	}
	clearTimeout(quickTimer);
	quickTimer = setTimeout(function() { msg.textContent = ''; }, 8000);
	refresh();
}

function openHelp() { document.getElementById('helpModal').style.display = 'flex'; }
function closeHelp() { document.getElementById('helpModal').style.display = 'none'; }

function openModal(f, title) {
	editing = f;
	document.getElementById('title').textContent = title;
	document.getElementById('name').value = f.name;
	document.getElementById('script').value = f.script;
	document.getElementById('sched').value = f.schedule || '';
	document.getElementById('modal').style.display = 'flex';
	updatePreview();
}

function newFlow() {
	openModal({ id: 0, name: '', script: '-5m | say -1 Restarting in $minutes minutes!\n0m | #shutdown\n', schedule: '', scheduleEnabled: false }, 'New flow');
}

function editFlow(id) { openModal(find(id), 'Edit flow'); }

function closeModal() { document.getElementById('modal').style.display = 'none'; }

function queuePreview() {
	clearTimeout(previewTimer);
	previewTimer = setTimeout(updatePreview, 300);
}

async function updatePreview() {
	let help = document.getElementById('schedHelp');
	try {
		let d = await post('/api/flows/preview', {
			schedule: document.getElementById('sched').value,
			script: document.getElementById('script').value
		});
		if (!d.ok) {
			help.className = 'help bad';
			help.textContent = d.error || 'Invalid schedule';
			return;
		}
		help.className = 'help';
		let html = '<b>' + esc(d.description) + '</b>';
		if (d.next && d.next.length) html += '<br>Next runs: ' + d.next.map(esc).join(' &middot; ');
		if (d.starts) html += '<br>Next countdown starts: ' + esc(d.starts);
		if (d.span) html += '<br>' + esc(d.span);
		help.innerHTML = html;
	} catch(e) {
		help.textContent = '';
	}
}

document.getElementById('sched').addEventListener('input', queuePreview);
document.getElementById('script').addEventListener('input', queuePreview);

document.getElementById('save').onclick = async () => {
	let d = await post('/api/flows', {
		id: editing.id,
		name: document.getElementById('name').value,
		script: document.getElementById('script').value,
		schedule: document.getElementById('sched').value,
		scheduleEnabled: !!editing.scheduleEnabled
	});
	if (!d.ok) { alert(d.error || 'Save failed'); return; }
	closeModal();
	refresh();
};

function clearDragMarks() {
	Array.prototype.forEach.call(document.querySelectorAll('#flows tr'), function(tr) {
		tr.classList.remove('dragging', 'drop-above', 'drop-below');
	});
}

function endDrag() {
	dragging = false;
	dragId = null;
	clearDragMarks();
}

function dropBefore(e, tr) {
	let rect = tr.getBoundingClientRect();
	return e.clientY < rect.top + rect.height / 2;
}

let flowsBody = document.getElementById('flows');

flowsBody.addEventListener('dragstart', function(e) {
	let tr = e.target.closest('tr');
	if (!tr) return;
	dragging = true;
	dragId = parseInt(tr.getAttribute('data-id'), 10);
	e.dataTransfer.effectAllowed = 'move';
	e.dataTransfer.setData('text/plain', String(dragId)); // Firefox needs data to start a drag
	tr.classList.add('dragging');
});

flowsBody.addEventListener('dragover', function(e) {
	if (!dragging) return;
	let tr = e.target.closest('tr');
	if (!tr) return;
	e.preventDefault();
	e.dataTransfer.dropEffect = 'move';
	tr.classList.remove('drop-above', 'drop-below');
	if (parseInt(tr.getAttribute('data-id'), 10) !== dragId) {
		tr.classList.add(dropBefore(e, tr) ? 'drop-above' : 'drop-below');
	}
});

flowsBody.addEventListener('dragleave', function(e) {
	let tr = e.target.closest('tr');
	if (tr) tr.classList.remove('drop-above', 'drop-below');
});

flowsBody.addEventListener('drop', async function(e) {
	e.preventDefault();
	let tr = e.target.closest('tr');
	if (!tr || !dragging) { endDrag(); return; }
	let targetId = parseInt(tr.getAttribute('data-id'), 10);
	let before = dropBefore(e, tr);
	let moved = dragId;
	endDrag();
	if (targetId === moved) return;
	let ids = flows.map(function(f) { return f.id; }).filter(function(id) { return id !== moved; });
	let at = ids.indexOf(targetId);
	if (!before) at++;
	ids.splice(at, 0, moved);
	let d = await post('/api/flows/reorder', { ids: ids });
	if (!d.ok) alert(d.error || 'Reorder failed');
	refresh();
});

flowsBody.addEventListener('dragend', endDrag);

refresh();
setInterval(refresh, {{.RefreshInterval.Milliseconds}});
</script>
</body>
</html>`))
