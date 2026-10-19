package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ============================================================================
// web.go overview
// ----------------------------------------------------------------------------
// The HTTP layer: routes, JSON API and page rendering. The HTML/JS for the
// pages lives in templates/*.html and is embedded into the binary at build
// time. Every route is listed in runWebServer.
//
// Concurrency note: everything that mutates or mirrors the ban list takes
// banListLock (db.go), so a background sync can never act on a stale list.
// ============================================================================

// runWebServer registers every route and serves until the listener fails.
// Patterns use Go 1.22 method and {wildcard} routing.
func runWebServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// The status bar on every page: {"rcon": "connected" | "retrying" | "unavailable"}.
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "rcon": rconStatus()})
	})

	// Raw RCON, for schedulers and scripts.
	mux.HandleFunc("/rcon", handleRcon)            // GET ?cmd=... or POST (raw text or {"command": ...})
	mux.HandleFunc("POST /kickall", handleKickAll) // optional {"reason": ...}

	// Players.
	mux.HandleFunc("GET /players", pageHandler(playersPage))
	mux.HandleFunc("GET /api/players", handlePlayersJSON)                  // online + offline players
	mux.HandleFunc("GET /api/players/all", handleAllPlayersJSON)           // every player ever seen (no RCON needed)
	mux.HandleFunc("POST /api/players/{id}/{action}", handlePlayerAction)  // message | kick | ban
	mux.HandleFunc("GET /api/players/{guid}/chat", handleChatHistory)      // their chat + messages sent to them
	mux.HandleFunc("POST /api/players/{guid}/chat", handleChatSend)        // {"message": ...} -> say <id> <message>
	mux.HandleFunc("GET /api/players/{guid}/details", handlePlayerDetails) // Details dialog: sessions, IPs, ban, note
	mux.HandleFunc("POST /api/players/{guid}/note", handleSaveNote)        // {"note": ...}, up to 500 characters
	mux.HandleFunc("POST /api/players/{guid}/role", handleSetRole)         // {"role": 0|1|2}: Player, Mod, Admin

	// RCON console on the Players page.
	mux.HandleFunc("GET /api/console", handleConsoleEvents)   // ?after=N: server messages and commands since N
	mux.HandleFunc("POST /api/console", handleConsoleCommand) // {"command": ...}, run exactly as typed

	// Bans.
	mux.HandleFunc("GET /bans", pageHandler(bansPage))
	mux.HandleFunc("GET /api/bans", handleBansJSON)            // list GUID bans (also syncs to DB)
	mux.HandleFunc("POST /api/bans/{guid}/ban", handleBanGUID) // ban a known player, online or not
	mux.HandleFunc("POST /api/bans/{guid}/unban", handleUnban) // remove every entry for a GUID

	// Flows.
	mux.HandleFunc("GET /flows", pageHandler(flowsPage))
	mux.HandleFunc("GET /api/flows", handleFlowsList)                   // flows + current/last run of each
	mux.HandleFunc("POST /api/flows", handleFlowSave)                   // create/update {id, name, trigger, schedule, delay, steps, onStop}
	mux.HandleFunc("POST /api/flows/{id}/run", withFlow(handleFlowRun)) // optional {"minutes": N}
	mux.HandleFunc("POST /api/flows/{id}/stop", withFlow(handleFlowStop))
	mux.HandleFunc("POST /api/flows/{id}/enable", withFlow(handleFlowEnable))
	mux.HandleFunc("POST /api/flows/{id}/disable", withFlow(handleFlowDisable))
	mux.HandleFunc("POST /api/flows/{id}/delete", withFlow(handleFlowDelete))
	mux.HandleFunc("POST /api/flows/preview", handleFlowPreview)  // {trigger, schedule, delay}
	mux.HandleFunc("POST /api/flows/reorder", handleFlowReorder)  // {ids: [...]}
	mux.HandleFunc("GET /api/flows/export", handleFlowExport)     // download flows.json
	mux.HandleFunc("POST /api/flows/import", handleFlowImport)    // {flows: [...]}
	mux.HandleFunc("POST /api/flows/command", handleQuickCommand) // {command: lock|unlock|shutdown|kickall}

	// Built-in chat commands (!help, !shout, ...): their settings, edited on the Flows page.
	mux.HandleFunc("GET /api/chat-commands", handleChatCommandsGet)
	mux.HandleFunc("POST /api/chat-commands", handleChatCommandsSave) // {settings: {...}}

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

// quietPaths are the pages' routine GET polls, left out of the request log.
var quietPaths = map[string]bool{
	"/health": true, "/players": true, "/bans": true, "/flows": true,
	"/api/players": true, "/api/bans": true, "/api/flows": true, "/api/status": true,
	"/api/console": true,
}

// isQuiet reports whether a request is a routine read: a quietPaths GET, or
// the Message and Details dialogs reading one player
// (/api/players/<guid>/chat, /api/players/<guid>/details).
func isQuiet(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	return quietPaths[p] || (strings.HasPrefix(p, "/api/players/") &&
		(strings.HasSuffix(p, "/chat") || strings.HasSuffix(p, "/details")))
}

// logRequests logs each request (method, path+query, remote address,
// X-Forwarded-For, user agent), except routine polls (isQuiet).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isQuiet(r) {
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

// fail writes {"ok": false, "error": msg} with status `code`.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// rconUnavailable is the 503 the pages read as "retrying...".
// rconStatus is the game server connection as the 5-second poller last saw
// it: connected, retrying (one or two failed checks, or none done yet), or
// unavailable (3 or more failed checks in a row, the point where the poller
// treats the server as down).
func rconStatus() string {
	switch f := rconFails.Load(); {
	case !rconPolled.Load():
		return "retrying"
	case f == 0:
		return "connected"
	case f >= 3:
		return "unavailable"
	}
	return "retrying"
}

func rconUnavailable(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// decodeBody decodes a JSON request body (capped at 1 MiB, enough for a
// large flow) into dst. An empty body is not an error: dst keeps its zero
// values.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Raw RCON endpoints
// ---------------------------------------------------------------------------

// handleRcon runs any RCON command, given as
//
//	GET  /rcon?cmd=players
//	POST /rcon                  body = raw command text
//	POST /rcon (JSON)           body = {"command": "players"}
//
// JSON is used when the Content-Type contains "json". Bodies are capped at 4 KiB.
// RCON failure -> 502; otherwise 200 with {ok, command, acked, output}.
func handleRcon(w http.ResponseWriter, r *http.Request) {
	var cmd string
	switch r.Method {
	case http.MethodGet:
		cmd = r.URL.Query().Get("cmd")
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			fail(w, 400, "bad body")
			return
		}
		cmd = string(raw)
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
	default:
		fail(w, 405, "method not allowed")
		return
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		fail(w, 400, "no command")
		return
	}
	out, acked, err := runShared(cmd, 2*time.Second, false, "web")
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "command": cmd, "acked": acked, "output": out})
}

// handleKickAll serves POST /kickall: kicks every player and nothing else.
// Optional body {"reason": "..."}; reason defaults to "Server restarting".
// The kickReport is returned under "result".
func handleKickAll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		fail(w, 400, "bad JSON")
		return
	}
	if body.Reason == "" {
		body.Reason = "Server restarting"
	}
	rep, err := kickAll(sharedRunner{}, body.Reason)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	log.Printf("rcon[web] > %s", rep.summary(body.Reason))
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
}

// ---------------------------------------------------------------------------
// Player API
// ---------------------------------------------------------------------------

// livePlayers reads the connected players (retrying read). ok is false when
// RCON can't be reached; the 503 has already been written in that case.
func livePlayers(w http.ResponseWriter) ([]Player, bool) {
	out, acked, err := runShared("players", 3*time.Second, true, "")
	if err != nil || !acked {
		rconUnavailable(w, "RCON unavailable")
		return nil, false
	}
	return parsePlayers(out), true
}

// handlePlayersJSON serves GET /api/players: online players plus offline ones
// from the DB. It answers straight away even while RCON is down
// ("connected": false, no online players), so the offline list always shows. (each with its role, for the Mod/Admin badge), and "bans":
// lower-case GUID -> ban status for everyone banned (the offline table's Ban
// Status column).
func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, connected := onlinePlayersForPage()
	offlinePlayers, err := globalDatabase.OfflinePlayers()
	if err != nil {
		log.Printf("error reading offline players from DB: %v", err)
		offlinePlayers = []Player{}
	}
	if roles, err := globalDatabase.Roles(); err == nil {
		for i := range players {
			players[i].Role = roles[strings.ToLower(players[i].GUID)]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"connected":      connected,
		"players":        players,
		"count":          len(players),
		"offlinePlayers": offlinePlayers,
		"bans":           banStatuses(),
	})
}

// onlinePlayersForPage is the online list for the Players page: the 5-second
// poller's latest list, so the page never waits on RCON. Before the poller's
// first result it reads `players` once. connected is false while the game
// server isn't answering (the poller has missed 3 checks, or that one read
// failed); the online list is then empty, and the page still shows everyone
// from the database.
func onlinePlayersForPage() ([]Player, bool) {
	latest.Lock()
	players, ok := append([]Player{}, latest.players...), latest.ok
	latest.Unlock()
	if rconStatus() == "unavailable" {
		return []Player{}, false
	}
	if ok {
		return players, true
	}
	out, acked, err := runShared("players", 3*time.Second, false, "")
	if err != nil || !acked {
		return []Player{}, false
	}
	return parsePlayers(out), true
}

// handleAllPlayersJSON serves GET /api/players/all: every player ever seen,
// from the database only, so it works while RCON is down.
func handleAllPlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, err := globalDatabase.KnownPlayers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "players": players, "count": len(players)})
}

// actionRequest is the JSON body for player actions and bans:
// kick -> Reason; ban -> Reason, Minutes (0 = permanent); message -> Message.
type actionRequest struct {
	Reason  string `json:"reason"`
	Minutes int    `json:"minutes"`
	Message string `json:"message"`
}

// handlePlayerAction serves POST /api/players/{id}/{message|kick|ban}.
// message is sent straight away. kick and ban first re-read `players` to
// make sure the ID is still the same connected player (and to get the GUID).
func handlePlayerAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		fail(w, http.StatusNotFound, "invalid player action")
		return
	}
	action := r.PathValue("action")
	if action != "message" && action != "kick" && action != "ban" {
		fail(w, http.StatusNotFound, "unknown action")
		return
	}
	var req actionRequest
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}

	if action == "message" {
		msg := cleanReason(req.Message)
		if msg == "" {
			fail(w, http.StatusBadRequest, "message is required")
			return
		}
		_, acked, err := runShared(fmt.Sprintf("say %d %s", id, msg), 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	players, ok := livePlayers(w)
	if !ok {
		return
	}
	var p Player
	found := false
	for _, x := range players {
		if x.ID == id {
			p, found = x, true
			break
		}
	}
	if !found {
		fail(w, http.StatusNotFound, "player is no longer connected")
		return
	}
	req.Reason = cleanReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	if action == "kick" {
		_, acked, err := runShared(fmt.Sprintf("kick %d %s", p.ID, req.Reason), 1500*time.Millisecond, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	if req.Minutes < 0 {
		fail(w, http.StatusBadRequest, "minutes must be 0 or greater")
		return
	}
	banPlayer(w, p, true, req)
}

// banResult is what banByGUID did, as the API reports it.
type banResult struct {
	OK        bool   `json:"ok"`
	GUIDBan   bool   `json:"guid_ban"`
	DBUpdated bool   `json:"db_updated"`
	Online    bool   `json:"online"`
	Kicked    bool   `json:"kicked"`
	Perm      bool   `json:"perm"`
	Error     string `json:"error,omitempty"`
	code      int    // HTTP status for the API
}

// banByGUID bans player p by GUID. online says whether p is connected right
// now (p.ID is only meaningful then). The pages, flows and the !ban chat
// command all ban through it; logAs labels the rcon log lines.
//
//  1. `addBan <guid> <minutes> <reason>` (0 minutes = permanent).
//  2. If online, kick with the same reason. A failed kick is only reported (Kicked).
//  3. Record GUID/name/IP/reason in SQLite (the IP is informational, not banned).
func banByGUID(p Player, online bool, minutes int, reason, logAs string) banResult {
	banListLock.Lock()
	defer banListLock.Unlock()
	res := banResult{Online: online, Perm: minutes == 0, code: http.StatusOK}
	_, guidAck, guidErr := runShared(fmt.Sprintf("addBan %s %d %s", p.GUID, minutes, reason), 1500*time.Millisecond, false, logAs)
	if guidErr != nil {
		res.Error, res.code = guidErr.Error(), http.StatusBadGateway
		return res
	}
	if !guidAck {
		res.Error, res.code = "GUID ban was not acknowledged", http.StatusBadGateway
		return res
	}
	res.GUIDBan = true
	if online {
		_, kickAck, kickErr := runShared(fmt.Sprintf("kick %d %s", p.ID, reason), 1500*time.Millisecond, false, logAs)
		res.Kicked = kickErr == nil && kickAck
		if !res.Kicked {
			log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
		}
	}
	perm := 0
	if res.Perm {
		perm = 1
	}
	if err := globalDatabase.BanUser(p.GUID, bareName(p.Name), p.IP, reason, perm); err != nil {
		log.Printf("error recording ban in DB: %v", err)
		res.Error, res.code = "GUID ban succeeded but database update failed", http.StatusInternalServerError
		return res
	}
	res.DBUpdated, res.OK = true, true
	return res
}

// banPlayer bans player p from the pages and writes the JSON response.
func banPlayer(w http.ResponseWriter, p Player, online bool, req actionRequest) {
	res := banByGUID(p, online, req.Minutes, req.Reason, "web")
	writeJSON(w, res.code, res)
}

// ---------------------------------------------------------------------------
// Player details, notes and chat (the Players page's Details and Message dialogs)
// ---------------------------------------------------------------------------

// maxNoteLen is the longest admin note, in characters.
const maxNoteLen = 500

// chatHistoryLimit is how many lines GET /api/players/{guid}/chat returns:
// the newest ones, oldest first. Everything stays in the table.
const chatHistoryLimit = 300

// playerGUID returns the {guid} path value, lower-cased; false (with a 404
// written) when it isn't a GUID.
func playerGUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	guid := r.PathValue("guid")
	if !guidPattern.MatchString(guid) {
		fail(w, http.StatusNotFound, "invalid player GUID")
		return "", false
	}
	return strings.ToLower(guid), true
}

// handlePlayerDetails serves GET /api/players/{guid}/details: everything
// logged about one player (db.go PlayerDetails), plus whether they are
// online right now and their ban status. Works for offline players and
// while RCON is down.
func handlePlayerDetails(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	d, err := globalDatabase.PlayerDetails(guid)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	} else if err != nil {
		log.Printf("details %s: %v", guid, err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, on := onlineByGUID(guid); on {
		d.Player.Online, d.Player.ID = true, p.ID
		d.Player.Name = bareName(p.Name)
	}
	d.Ban = banStatusOf(guid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "details": d})
}

// handleSaveNote serves POST /api/players/{guid}/note {"note": "..."}: up to
// maxNoteLen characters; an empty note deletes it.
func handleSaveNote(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	note := strings.TrimRight(strings.ReplaceAll(req.Note, "\r\n", "\n"), " \t\n")
	if n := utf8.RuneCountInString(note); n > maxNoteLen {
		fail(w, http.StatusBadRequest, fmt.Sprintf("note is %d characters; the limit is %d", n, maxNoteLen))
		return
	}
	known, err := globalDatabase.PlayerKnown(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	if err := globalDatabase.SaveNote(guid, note); err != nil {
		log.Printf("note %s: %v", guid, err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// handleSetRole serves POST /api/players/{guid}/role {"role": 0|1|2}: the
// player's role for the chat commands (Player, Mod or Admin). The Details
// dialog's Permissions tab saves it as soon as a role is clicked.
func handleSetRole(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Role *int `json:"role"`
	}
	if err := decodeBody(w, r, &req); err != nil || req.Role == nil {
		fail(w, http.StatusBadRequest, `send {"role": 0, 1 or 2}`)
		return
	}
	if *req.Role < rolePlayer || *req.Role > roleAdmin {
		fail(w, http.StatusBadRequest, "role must be 0 (Player), 1 (Mod) or 2 (Admin)")
		return
	}
	found, err := globalDatabase.SetPlayerRole(guid, *req.Role)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	log.Printf("roles: %s is now %s", guid, roleName(*req.Role))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": *req.Role})
}

// handleChatHistory serves GET /api/players/{guid}/chat: the player's chat
// and the private messages sent to them, oldest first. Works while RCON is
// down, and for players who are offline.
func handleChatHistory(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	lines, err := globalDatabase.ChatHistory(guid, chatHistoryLimit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "messages": lines})
}

// handleChatSend serves POST /api/players/{guid}/chat {"message": "..."}:
// sends `say <id> <message>` to the player with that GUID and adds it to
// their history. It reads `players` first because the player's number can
// change between page refreshes (it's reused after someone leaves).
func handleChatSend(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	msg := cleanReason(req.Message)
	if msg == "" {
		fail(w, http.StatusBadRequest, "message is required")
		return
	}
	players, ok := livePlayers(w)
	if !ok {
		return
	}
	var p Player
	found := false
	for _, x := range players {
		if strings.EqualFold(x.GUID, guid) {
			p, found = x, true
			break
		}
	}
	if !found {
		fail(w, http.StatusConflict, "player is no longer connected")
		return
	}
	cmd := fmt.Sprintf("say %d %s", p.ID, msg)
	_, acked, err := runShared(cmd, 2*time.Second, false, "web")
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if !acked {
		fail(w, http.StatusBadGateway, "message was not acknowledged by the server")
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), guid, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// RCON console (the Players page's Console dialog)
// ---------------------------------------------------------------------------
//
// GET /api/console?after=N returns the server messages since N (serverEvents,
// rcon.go), plus the commands typed in the console and their replies. POST
// /api/console runs any RCON command and puts it and its reply in the same
// log. Commands sent by flows and the other pages are left out (the Flows
// page and the container log show those).

// handleConsoleEvents serves GET /api/console?after=N: every kept event
// newer than N, plus "last" to send as after= next time. The page polls it
// while the console is open; after=0 (or none) returns the whole buffer.
func handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, last := serverEvents.afterWithLast(after)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events, "last": last})
}

// consoleSayRe matches a private message typed in the console (say 3 hi),
// so it can be added to that player's history. say -1 (everyone) is not.
var consoleSayRe = regexp.MustCompile(`(?i)^say\s+(\d+)\s+(.+)$`)

// handleConsoleCommand serves POST /api/console {"command": "..."}: runs one
// command exactly as typed and writes the outcome into the console log.
// runShared already logged the command itself (as consoleSource). kickall is
// ReCON's own command, as in the CLI console; everything else goes to
// BattlEye.
func handleConsoleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	cmd := cleanReason(req.Command)
	if cmd == "" {
		fail(w, http.StatusBadRequest, "no command")
		return
	}
	note := func(kind, text string) {
		serverEvents.publish(ServerEvent{Time: time.Now(), Kind: kind, Text: text, PlayerID: -1})
	}

	if low := strings.ToLower(cmd); low == "kickall" || strings.HasPrefix(low, "kickall ") {
		reason := strings.TrimSpace(cmd[len("kickall"):])
		if reason == "" {
			reason = "Server restarting"
		}
		note(eventSent, "> "+cmd)
		rep, err := kickAll(sharedRunner{}, reason)
		if err != nil {
			note(eventError, "kickall failed: "+err.Error())
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("rcon[%s] > %s", consoleSource, rep.summary(reason))
		note(eventReply, rep.summary(reason))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": true, "output": rep.summary(reason)})
		return
	}

	out, acked, err := runShared(cmd, 3*time.Second, false, consoleSource)
	switch {
	case err != nil:
		note(eventError, "error: "+err.Error())
		fail(w, http.StatusBadGateway, err.Error())
		return
	case !acked:
		note(eventError, "(no acknowledgement from server)")
	case strings.TrimSpace(out) != "":
		note(eventReply, strings.TrimRight(out, "\r\n "))
	}
	if acked {
		recordConsoleSay(cmd)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "output": out})
}

// recordConsoleSay adds a `say <id> ...` typed in the console to that
// player's history, if they are online.
func recordConsoleSay(cmd string) {
	m := consoleSayRe.FindStringSubmatch(cmd)
	if m == nil {
		return
	}
	id, _ := strconv.Atoi(m[1])
	p, ok := onlineByID(id)
	if !ok || p.GUID == "" {
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), p.GUID, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Ban API
// ---------------------------------------------------------------------------

// handleBansJSON serves GET /api/bans. It syncs first (so viewing the page
// refreshes the DB too). Names come from the DB since BattlEye only stores
// GUIDs; a GUID listed more than once is shown once.
func handleBansJSON(w http.ResponseWriter, r *http.Request) {
	bans, err := syncBanList()
	if bans == nil {
		rconUnavailable(w, err.Error())
		return
	}
	if err != nil {
		log.Printf("error syncing bans to DB: %v", err)
	}
	names, _ := globalDatabase.BanNames()
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

// guidParam returns the {guid} path value, lower-cased. ok is false (and a
// 404 has been written) when it isn't a valid GUID.
func guidParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	guid := r.PathValue("guid")
	if !guidPattern.MatchString(guid) {
		fail(w, http.StatusNotFound, "invalid ban action")
		return "", false
	}
	return strings.ToLower(guid), true
}

// handleBanGUID serves POST /api/bans/{guid}/ban, the Bans page's "Ban Player"
// dialog. Optional body {"reason", "minutes"}; minutes defaults to 0
// (permanent). The player must be known (online or in the DB) so the ban gets
// a name and IP on record; an online player is kicked right away.
func handleBanGUID(w http.ResponseWriter, r *http.Request) {
	guid, ok := guidParam(w, r)
	if !ok {
		return
	}
	var req actionRequest
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if req.Minutes < 0 {
		fail(w, http.StatusBadRequest, "minutes must be 0 or greater")
		return
	}
	req.Reason = cleanReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	players, ok := livePlayers(w)
	if !ok {
		return
	}
	for _, p := range players {
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

// handleUnban serves POST /api/bans/{guid}/unban.
//
// BattlEye removes bans by list index, indexes shift after every removal, and
// one GUID can be listed more than once. So it loops: re-read the list, find
// the first entry for the GUID, removeBan <index>, until the GUID is gone
// (safety cap: 20). Then `writeBans` (best effort, reported as "saved") and
// the DB row is deleted. Partial states are reported via be_removed/db_removed.
func handleUnban(w http.ResponseWriter, r *http.Request) {
	guid, ok := guidParam(w, r)
	if !ok {
		return
	}
	banListLock.Lock()
	defer banListLock.Unlock()

	removed := 0
	for ; removed < 20; removed++ {
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
		_, acked, err := runShared(fmt.Sprintf("removeBan %d", idx), 1500*time.Millisecond, false, "web")
		if err == nil && !acked {
			err = errors.New("removeBan was not acknowledged")
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": err.Error()})
			return
		}
	}
	if removed >= 20 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "removed": removed, "error": "ban still present after 20 removals"})
		return
	}

	_, saved, _ := runShared("writeBans", 1500*time.Millisecond, false, "web") // persist bans.txt, best effort

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

func handleFlowsList(w http.ResponseWriter, r *http.Request) {
	flows, err := flowsWithNextRun()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": flows, "runs": snapshotRuns()})
}

func handleFlowSave(w http.ResponseWriter, r *http.Request) {
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
}

// withFlow loads the flow named by the {id} path value (404 if it doesn't
// exist) and passes it to h.
func withFlow(h func(http.ResponseWriter, *http.Request, Flow)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
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
		h(w, r, f)
	}
}

// handleFlowRun: optional body {"minutes": N} replaces the flow's delay for
// this run with N minutes.
func handleFlowRun(w http.ResponseWriter, r *http.Request, f Flow) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
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
}

func handleFlowStop(w http.ResponseWriter, r *http.Request, f Flow) {
	if err := stopFlow(f.ID); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowEnable(w http.ResponseWriter, r *http.Request, f Flow) {
	if err := checkFlowCanEnable(f); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	setFlowEnabled(w, f.ID, true)
}

func handleFlowDisable(w http.ResponseWriter, r *http.Request, f Flow) {
	setFlowEnabled(w, f.ID, false)
}

func setFlowEnabled(w http.ResponseWriter, id int64, on bool) {
	if err := globalDatabase.SetFlowScheduleEnabled(id, on); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowDelete(w http.ResponseWriter, r *http.Request, f Flow) {
	if err := deleteFlow(f.ID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errFlowBusy) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// flowFileEntry is one flow in an export file.
type flowFileEntry struct {
	Name     string `json:"name"`
	Trigger  string `json:"trigger"`            // schedule, join, chat or manual
	Schedule string `json:"schedule,omitempty"` // cron, for schedule flows
	Delay    string `json:"delay,omitempty"`    // e.g. "5s"
	Steps    []Step `json:"steps"`
	OnStop   []Step `json:"onStop,omitempty"`
}

type flowFile struct {
	Flows []flowFileEntry `json:"flows"`
}

// handleFlowExport serves every flow, in list order, as a downloadable
// flows.json. Enabled/disabled state is not exported.
func handleFlowExport(w http.ResponseWriter, r *http.Request) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := flowFile{Flows: make([]flowFileEntry, 0, len(flows))}
	for _, f := range flows {
		out.Flows = append(out.Flows, flowFileEntry{Name: f.Name, Trigger: f.Trigger, Schedule: f.Schedule,
			Delay: f.Delay, Steps: f.Steps, OnStop: f.OnStop})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="flows.json"`)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

// handleFlowImport appends every flow in a flows.json body to the bottom of
// the list, disabled. A taken name gets " (2)", " (3)", ...; a name over
// maxFlowNameLen is shortened; flows that fail the usual checks are skipped
// and reported.
func handleFlowImport(w http.ResponseWriter, r *http.Request) {
	var in flowFile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "not a valid flows file: "+err.Error())
		return
	}
	if len(in.Flows) == 0 {
		fail(w, http.StatusBadRequest, "no flows in file")
		return
	}
	existing, err := globalDatabase.ListFlows()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	taken := map[string]bool{}
	for _, f := range existing {
		taken[strings.ToLower(f.Name)] = true
	}
	added := 0
	skipped := []string{}
	for i, e := range in.Flows {
		base := clipName(strings.TrimSpace(e.Name), maxFlowNameLen)
		if base == "" {
			skipped = append(skipped, fmt.Sprintf("flow #%d: name is required", i+1))
			continue
		}
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			suffix := fmt.Sprintf(" (%d)", n)
			name = clipName(base, maxFlowNameLen-len(suffix)) + suffix
		}
		if _, err := validateAndSaveFlow(Flow{Name: name, Trigger: e.Trigger, Schedule: e.Schedule,
			Delay: e.Delay, Steps: e.Steps, OnStop: e.OnStop}); err != nil {
			skipped = append(skipped, base+": "+err.Error())
			continue
		}
		taken[strings.ToLower(name)] = true
		added++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "skipped": skipped})
}

// handleFlowPreview returns the edit dialog's plain-English summary.
func handleFlowPreview(w http.ResponseWriter, r *http.Request) {
	var f Flow // trigger, schedule, delay
	if err := decodeBody(w, r, &f); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	writeJSON(w, http.StatusOK, previewFlow(f))
}

// handleFlowReorder saves the order of the flows list: {"ids": [...]}.
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

// handleQuickCommand serves the Flows page's quick buttons. Only these four
// commands are accepted, not arbitrary RCON text.
func handleQuickCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	switch name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Command), "#")); name {
	case "lock", "unlock":
		_, acked, err := runShared("#"+name, 2*time.Second, true, "web")
		if err == nil && !acked {
			err = errors.New("#" + name + " was not acknowledged")
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "#" + name + " sent"})
	case "shutdown":
		// The server may die before it acknowledges, so only a transport error is a failure.
		_, acked, err := runShared("#shutdown", 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := kickAll(sharedRunner{}, "Server restarting")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("rcon[web] > %s", rep.summary("Server restarting"))
		msg := fmt.Sprintf("Kicked %d player(s)", len(rep.Order))
		if len(rep.Remaining) > 0 {
			msg += fmt.Sprintf(", %d still connected", len(rep.Remaining))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	default:
		fail(w, http.StatusBadRequest, "unknown command")
	}
}

// ---------------------------------------------------------------------------
// Chat commands API (the Flows page's Chat commands dialog)
// ---------------------------------------------------------------------------

// handleChatCommandsGet serves GET /api/chat-commands: every command with
// its labels, defaults and allowed placeholders ("commands"), and the
// current settings.
func handleChatCommandsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commands": chatCommandDefs,
		"settings": currentChatSettings(), "defaults": defaultChatSettings()})
}

// handleChatCommandsSave serves POST /api/chat-commands {"settings": {...}}.
func handleChatCommandsSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings chatSettings `json:"settings"`
	}
	if err := decodeBody(w, r, &req); err != nil || req.Settings == nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	saved, err := saveChatSettings(req.Settings)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("chat commands: settings saved")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": saved})
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

// templateFiles holds templates/*.html, compiled into the binary.
// base.html defines the shared "style" and "reorder" blocks.
//
//go:embed templates/*.html
var templateFiles embed.FS

// loadPage parses one page together with base.html.
func loadPage(name string) *template.Template {
	return template.Must(template.New(name).ParseFS(templateFiles, "templates/base.html", "templates/"+name))
}

var (
	playersPage = loadPage("players.html")
	bansPage    = loadPage("bans.html")
	flowsPage   = loadPage("flows.html")
)

// pageSettings holds the tweakable layout values (widths, paddings, font
// sizes) and the browser polling intervals; it is every page's template data.
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
	PlayerWidth:         "190px",
	IPWidth:             "150px",
	GUIDWidth:           "300px", // the widest column: a 32-character GUID
	PingWidth:           "56px",  // Bans/other pages; on the Players page Ping takes the spare width
	ActionsWidth:        "290px", // online players: Message, Kick, Ban and Details (~255px) plus padding and font slack
	ModalWidth:          "420px",
	ModalPadding:        "20px",
	InputPadding:        "9px",
	ButtonPadding:       "6px 10px",
	RefreshInterval:     5 * time.Second,
	BansRefreshInterval: 10 * time.Second,
}

// pageHandler serves an HTML page rendered with pageSettings.
func pageHandler(page *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, pageSettings); err != nil {
			log.Printf("template %s: %v", page.Name(), err)
		}
	}
}
