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
	"syscall"
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

// runWebServer serves every route (routes) until the listener fails.
func runWebServer(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           logWebRequests(buildWebRoutes()),
		ReadHeaderTimeout: 5 * time.Second, // guards against slowloris-style header stalls
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      120 * time.Second, // generous: kickall can take a while
	}
	return srv.ListenAndServe()
}

// buildWebRoutes registers every route. Patterns use Go 1.22 method and {wildcard}
// routing.
func buildWebRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	// The bare address opens the Players page. {$} matches "/" only, so a
	// mistyped path still gets a 404. 302, not 301: browsers keep a 301, and
	// a page at / later would never be seen.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/players", http.StatusFound)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// The status bar on every page: {"rcon": "connected" | "retrying" |
	// "unavailable"}, and while not connected "retryInMs": time until the next
	// check of the game server.
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := map[string]any{"ok": true, "rcon": currentRconStatus()}
		if st["rcon"] != "connected" {
			st["retryInMs"] = max(time.Until(rconNextCheck()).Milliseconds(), 0)
		}
		writeJSON(w, 200, st)
	})

	// Raw RCON, for schedulers and scripts.
	mux.HandleFunc("/rcon", handleRconCommand)     // GET ?cmd=... or POST (raw text or {"command": ...})
	mux.HandleFunc("POST /kickall", handleKickAll) // optional {"reason": ...}

	// Players.
	mux.HandleFunc("GET /players", servePageHandler(playersPage))
	mux.HandleFunc("GET /api/players", handlePlayersJSON)                      // online + offline players
	mux.HandleFunc("GET /api/players/all", handleAllPlayersJSON)               // every player ever seen (no RCON needed)
	mux.HandleFunc("POST /api/players/{id}/{action}", handlePlayerAction)      // message | kick | ban
	mux.HandleFunc("GET /api/players/{guid}/chat", handleChatHistory)          // their chat + messages sent to them
	mux.HandleFunc("POST /api/players/{guid}/chat", handleChatSend)            // {"message": ...} -> say <id> <message>
	mux.HandleFunc("GET /api/players/{guid}/details", handlePlayerDetails)     // Details dialog: sessions, IPs, ban, note
	mux.HandleFunc("POST /api/players/{guid}/note", handleSaveNote)            // {"note": ...}, up to 500 characters
	mux.HandleFunc("POST /api/players/{guid}/role", handleSetRole)             // {"role": 0|1|2}: Player, Mod, Admin
	mux.HandleFunc("GET /api/players/{guid}/steam", handlePlayerSteam)         // Steam Profile tab (steam.go); ?refresh=1 fetches now
	mux.HandleFunc("POST /api/players/{guid}/steam/search", handleSteamSearch) // find a player's SteamID from their GUID (steam.go)

	// RCON Settings on the Players page: override the docker-compose RCON settings.
	mux.HandleFunc("GET /api/rcon-settings", handleRconSettingsGet)
	mux.HandleFunc("POST /api/rcon-settings", handleRconSettingsConnect) // {host, port, password}: connect, then save

	// RCON console on the Players page.
	mux.HandleFunc("GET /api/console", handleConsoleEvents)   // ?after=N: server messages and commands since N
	mux.HandleFunc("POST /api/console", handleConsoleCommand) // {"command": ...}, run exactly as typed

	// Bans.
	mux.HandleFunc("GET /bans", servePageHandler(bansPage))
	mux.HandleFunc("GET /api/bans", handleBansJSON)                  // list GUID bans (from the DB, kept in step with BattlEye)
	mux.HandleFunc("POST /api/bans/{guid}/ban", handleBanGUID)       // ban a known player, online or not
	mux.HandleFunc("POST /api/bans/{guid}/unban", handlePlayerUnban) // remove every entry for a GUID

	// Flows.
	mux.HandleFunc("GET /flows", servePageHandler(flowsPage))
	mux.HandleFunc("GET /api/flows", handleFlowsList)                   // flows + current/last run of each
	mux.HandleFunc("POST /api/flows", handleFlowSave)                   // update {id, name, trigger, schedule, delay, steps, onStop}; no id creates
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

	return mux
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
// (/api/players/<guid>/chat, /details, /steam).
func isQuiet(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	return quietPaths[p] || (strings.HasPrefix(p, "/api/players/") &&
		(strings.HasSuffix(p, "/chat") || strings.HasSuffix(p, "/details") || strings.HasSuffix(p, "/steam")))
}

// logWebRequests logs each request (method, path+query, remote address,
// X-Forwarded-For, user agent), except routine polls (isQuiet).
func logWebRequests(next http.Handler) http.Handler {
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

// currentRconStatus is the game server connection as the server watch (rcon.go)
// last saw it: connected, retrying (one or two failed checks, or none done
// yet), or unavailable (3 or more failed checks in a row, the point where the
// server watch treats the server as down).
func currentRconStatus() string {
	switch f := rconFails.Load(); {
	case !rconPolled.Load():
		return "retrying"
	case f == 0:
		return "connected"
	case f >= downAfterMisses:
		return "unavailable"
	}
	return "retrying"
}

// rconNextCheck is when ReCON next tries the game server: the server watch's
// next check (its next heartbeat while the server is down).
func rconNextCheck() time.Time {
	return time.UnixMilli(rconNextPoll.Load())
}

// isRconUnavailable is the 503 the pages read as "retrying...".
func isRconUnavailable(w http.ResponseWriter, msg string) {
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

// handleRconCommand runs any RCON command, given as
//
//	GET  /rcon?cmd=players
//	POST /rcon                  body = raw command text
//	POST /rcon (JSON)           body = {"command": "players"}
//
// JSON is used when the Content-Type contains "json". Bodies are capped at 4 KiB.
// RCON failure -> 502; otherwise 200 with {ok, command, acked, output}.
func handleRconCommand(w http.ResponseWriter, r *http.Request) {
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
	out, acked, err := runWebCommand(cmd, 2*time.Second, false, "web")
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
	rep, err := kickAllWeb(body.Reason, "web")
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
}

// ---------------------------------------------------------------------------
// Player API
// ---------------------------------------------------------------------------

// readLivePlayers reads the connected players (retrying read). ok is false when
// RCON can't be reached; the 503 has already been written in that case.
func readLivePlayers(w http.ResponseWriter) ([]Player, bool) {
	out, acked, err := runWebCommand("players", 3*time.Second, true, "")
	if err != nil || !acked {
		isRconUnavailable(w, "RCON unavailable")
		return nil, false
	}
	return parsePlayersReply(out), true
}

// handlePlayersJSON serves GET /api/players: online players (each with their
// role, for the Mod/Admin badge, and the last line they wrote in chat), the
// offline ones from the DB, and "bans": lower-case GUID -> ban status for
// everyone banned (the offline table's Ban Status column). It answers
// straight away even while RCON is down ("connected": false, no online
// players), so the offline list always shows.
func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, connected := onlinePlayersForPage()
	offlinePlayers, err := globalDatabase.ListOfflinePlayers()
	if err != nil {
		log.Printf("error reading offline players from DB: %v", err)
		offlinePlayers = []Player{}
	}
	if roles, err := globalDatabase.AllPlayerRoles(); err == nil {
		for i := range players {
			players[i].Role = roles[strings.ToLower(players[i].GUID)]
		}
	}
	// The Last Message column: the newest line each online player wrote
	// this session, kept in memory by the player tracker (players.go).
	last := lastChatPerPlayer()
	for i := range players {
		if c, ok := last[players[i].ID]; ok && strings.EqualFold(c.guid, players[i].GUID) {
			players[i].LastMessage, players[i].LastMessageChannel, players[i].LastMessageUnix = c.chat.Text, c.chat.Channel, c.chat.At.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"connected":      connected,
		"players":        players,
		"count":          len(players),
		"offlinePlayers": offlinePlayers,
		"bans":           allBanStatuses(),
	})
}

// onlinePlayersForPage is the online list for the Players page: the
// tracker's (players.go), kept up to date by the server watch's polls, so
// the page never waits on RCON. Before the first poll since the server last
// answered, it polls once itself. ok is false while the server isn't
// answering.
func onlinePlayersForPage() ([]Player, bool) {
	if currentRconStatus() == "unavailable" {
		return []Player{}, false
	}
	players, known := listOnlinePlayers()
	if known {
		return players, true
	}
	if !pollPlayersNow() {
		return []Player{}, false
	}
	players, _ = listOnlinePlayers()
	return players, true
}

// handleAllPlayersJSON serves GET /api/players/all: every player ever seen,
// from the database only, so it works while RCON is down.
func handleAllPlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, err := globalDatabase.ListKnownPlayers()
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
		_, acked, err := runWebCommand(fmt.Sprintf("say %d %s", id, msg), 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": acked, "acked": acked})
		return
	}

	players, ok := readLivePlayers(w)
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
		_, acked, err := runWebCommand(fmt.Sprintf("kick %d %s", p.ID, req.Reason), 1500*time.Millisecond, false, "web")
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
	banOnlinePlayer(w, p, true, req)
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
//  2. Record GUID/name/IP/reason and when it runs out in SQLite (the IP is
//     informational, not banned). This comes before the kick so the player's
//     session is closed as "banned", not "kicked".
//  3. If online, kick with the same reason. A failed kick is only reported (Kicked).
func banByGUID(p Player, online bool, minutes int, reason, logAs string) banResult {
	banListLock.Lock()
	defer banListLock.Unlock()
	res := banResult{Online: online, Perm: minutes == 0, code: http.StatusOK}
	placed := time.Now()
	_, guidAck, guidErr := runWebCommand(fmt.Sprintf("addBan %s %d %s", p.GUID, minutes, reason), 1500*time.Millisecond, false, logAs)
	if guidErr != nil {
		res.Error, res.code = guidErr.Error(), http.StatusBadGateway
		return res
	}
	if !guidAck {
		res.Error, res.code = "GUID ban was not acknowledged", http.StatusBadGateway
		return res
	}
	res.GUIDBan = true
	dbErr := globalDatabase.RecordPlayerBan(p.GUID, bareName(p.Name), p.IP, reason, res.Perm, placed.Add(time.Duration(minutes)*time.Minute))
	refreshBanExpiry()
	if online {
		_, kickAck, kickErr := runWebCommand(fmt.Sprintf("kick %d %s", p.ID, reason), 1500*time.Millisecond, false, logAs)
		res.Kicked = kickErr == nil && kickAck
		if !res.Kicked {
			log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
		}
	}
	if dbErr != nil {
		log.Printf("error recording ban in DB: %v", dbErr)
		res.Error, res.code = "GUID ban succeeded but database update failed", http.StatusInternalServerError
		return res
	}
	res.DBUpdated, res.OK = true, true
	return res
}

// banOnlinePlayer bans player p from the pages and writes the JSON response.
func banOnlinePlayer(w http.ResponseWriter, p Player, online bool, req actionRequest) {
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
// logged about one player (db.go GetPlayerDetails), plus whether they are
// online right now and their ban status. Works for offline players and
// while RCON is down.
func handlePlayerDetails(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	d, err := globalDatabase.GetPlayerDetails(guid)
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
	known, err := globalDatabase.IsPlayerKnown(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	if err := globalDatabase.SavePlayerNote(guid, note); err != nil {
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
	lines, err := globalDatabase.GetChatHistory(guid, chatHistoryLimit)
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
	players, ok := readLivePlayers(w)
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
	_, acked, err := runWebCommand(cmd, 2*time.Second, false, "web")
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
// GET /api/console?after=N returns the master console log since N
// (serverEvents, rcon.go): every server message, and every command ReCON
// sent for the pages, the console, flows and chat commands, with its reply.
// Polls and heartbeats aren't in it, nor commands from the CLI (its own
// session). POST /api/console runs any RCON command.

// consoleShownLines is the most lines the console page keeps, so it is
// also the most one request returns (the newest ones).
const consoleShownLines = 1000

// handleConsoleEvents serves GET /api/console?after=N: the events newer than
// N (at most consoleShownLines, the newest), plus "last" to send as after=
// next time. The page polls it while the console is open.
func handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, last := serverEvents.afterWithLast(after, consoleShownLines)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events, "last": last})
}

// consoleSayRe matches a private message typed in the console (say 3 hi),
// so it can be added to that player's history. say -1 (everyone) is not.
var consoleSayRe = regexp.MustCompile(`(?i)^say\s+(\d+)\s+(.+)$`)

// handleConsoleCommand serves POST /api/console {"command": "..."}: runs one
// command exactly as typed; runWebCommand puts it and its outcome in the
// console log (as consoleSource). kickall is
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
	if low := strings.ToLower(cmd); low == "kickall" || strings.HasPrefix(low, "kickall ") {
		reason := strings.TrimSpace(cmd[len("kickall"):])
		rep, err := kickAllWeb(reason, consoleSource)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": true, "output": rep.summary(reason)})
		return
	}

	out, acked, err := runWebCommand(cmd, 3*time.Second, false, consoleSource)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
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
// RCON Settings (the Players page's RCON Settings dialog)
// ---------------------------------------------------------------------------

// handleRconSettingsGet serves GET /api/rcon-settings: the overrides in use
// ("settings", empty fields use the defaults; memory only), the defaults from
// docker-compose.yaml ("defaults", password included: ReCON runs on the game
// host behind its firewall) and the endpoint ReCON uses now ("address").
func handleRconSettingsGet(w http.ResponseWriter, r *http.Request) {
	d := rconDefaultSettings()
	address, _ := currentRconTarget()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"settings": currentRconOverrides(),
		"defaults": map[string]any{"host": d.Host, "port": d.Port, "password": d.Password},
		"address":  address,
		"rcon":     currentRconStatus(),
	})
}

// handleRconSettingsConnect serves POST /api/rcon-settings {host, port,
// password}: empty fields use the defaults. It logs in with those settings
// on a new connection first (a new handshake, even when nothing changed);
// only if the server accepts does that become ReCON's RCON session and are
// the settings kept, in memory until ReCON restarts. A wrong address or
// password changes nothing.
func handleRconSettingsConnect(w http.ResponseWriter, r *http.Request) {
	var req rconSettings
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	req, err := req.clean()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	address, password := req.effective()
	if err := reconnectWebSession(address, password); err != nil {
		log.Printf("rcon: RCON Settings: connecting to %s failed: %v", address, err)
		why := err.Error()
		if errors.Is(err, syscall.ECONNREFUSED) {
			why = "nothing is listening on that port (is the server running, and is the port right?)"
		}
		fail(w, http.StatusBadGateway, "Could not log in to "+address+": "+why+". Nothing was changed.")
		return
	}
	// The server just answered the login: show it as connected now rather
	// than after the server watch's next check.
	rconPolled.Store(true)
	rconFails.Store(0)
	keepRconOverrides(req)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "address": address})
}

// ---------------------------------------------------------------------------
// Ban API
// ---------------------------------------------------------------------------

// handleBansJSON serves GET /api/bans: every ban, the permanent ones from
// the bans table and the timed ones from memory. The server watch keeps both in
// step with BattlEye (syncBanList, db.go), and ban and unban update them
// straight away, so the page never waits on RCON.
// "connected" is false while the game server isn't answering.
func handleBansJSON(w http.ResponseWriter, r *http.Request) {
	list, err := globalDatabase.ListAllBans()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": currentRconStatus() == "connected", "bans": list, "count": len(list)})
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

	players, ok := readLivePlayers(w)
	if !ok {
		return
	}
	for _, p := range players {
		if strings.EqualFold(p.GUID, guid) {
			banOnlinePlayer(w, p, true, req)
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
	banOnlinePlayer(w, p, false, req)
}

// handlePlayerUnban serves POST /api/bans/{guid}/unban.
//
// BattlEye removes bans by list index, indexes shift after every removal, and
// one GUID can be listed more than once. So it loops: re-read the list, find
// the first entry for the GUID, removeBan <index>, until the GUID is gone
// (safety cap: 20). Then `writeBans` (best effort, reported as "saved") and
// the DB row is deleted, which is the only way a permanent ban leaves the
// database (syncBanList puts back any permanent ban BattlEye loses). Partial
// states are reported via be_removed/db_removed.
func handlePlayerUnban(w http.ResponseWriter, r *http.Request) {
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
		_, acked, err := runWebCommand(fmt.Sprintf("removeBan %d", idx), 1500*time.Millisecond, false, "web")
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

	_, saved, _ := runWebCommand("writeBans", 1500*time.Millisecond, false, "web") // persist bans.txt, best effort

	err := globalDatabase.DeletePlayerBan(guid)
	refreshBanExpiry()
	if err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": flows, "runs": snapshotFlowRuns()})
}

// handleFlowSave serves POST /api/flows: with "id" it updates that flow;
// without one (or with "id": null) it creates a new flow, which gets the
// lowest free number from 0. 0 is an ordinary flow number.
func handleFlowSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Flow
		ID *int64 `json:"id"` // nil: a new flow (shadows Flow.ID when decoding)
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	f := req.Flow
	if req.ID != nil {
		if *req.ID < 0 {
			fail(w, http.StatusBadRequest, "invalid flow id")
			return
		}
		f.ID = *req.ID
	}
	id, err := validateAndSaveFlow(f, req.ID == nil)
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
		if err != nil || id < 0 {
			fail(w, http.StatusNotFound, "invalid flow id")
			return
		}
		f, err := globalDatabase.GetStoredFlow(id)
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
	if err := startFlowNow(f, body.Minutes); err != nil {
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
	if err := stopFlowRun(f.ID); err != nil {
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
	wakeFlowScheduler() // flows_schedule.go
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowDelete(w http.ResponseWriter, r *http.Request, f Flow) {
	if err := deleteFlowAndRuns(f.ID); err != nil {
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
	// CounterThreshold is the flow's {counter_threshold}; 0 (an older file) means the default.
	CounterThreshold int `json:"counterThreshold,omitempty"`
}

type flowFile struct {
	Flows []flowFileEntry `json:"flows"`
}

// handleFlowExport serves every flow, in list order, as a downloadable
// flows.json. Enabled/disabled state is not exported.
func handleFlowExport(w http.ResponseWriter, r *http.Request) {
	flows, err := globalDatabase.ListStoredFlows()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := flowFile{Flows: make([]flowFileEntry, 0, len(flows))}
	for _, f := range flows {
		out.Flows = append(out.Flows, flowFileEntry{Name: f.Name, Trigger: f.Trigger, Schedule: f.Schedule,
			Delay: f.Delay, Steps: f.Steps, OnStop: f.OnStop, CounterThreshold: f.CounterThreshold})
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
	existing, err := globalDatabase.ListStoredFlows()
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
			Delay: e.Delay, Steps: e.Steps, OnStop: e.OnStop, CounterThreshold: e.CounterThreshold}, true); err != nil {
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
	writeJSON(w, http.StatusOK, previewFlowSteps(f))
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
	if err := globalDatabase.ReorderStoredFlows(req.IDs); err != nil {
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
		_, acked, err := runWebCommand("#"+name, 2*time.Second, true, "web")
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
		_, acked, err := runWebCommand("#shutdown", 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := kickAllWeb("Server restarting", "web")
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

// loadPageTemplate parses one page together with base.html.
func loadPageTemplate(name string) *template.Template {
	return template.Must(template.New(name).ParseFS(templateFiles, "templates/base.html", "templates/"+name))
}

var (
	playersPage = loadPageTemplate("players.html")
	bansPage    = loadPageTemplate("bans.html")
	flowsPage   = loadPageTemplate("flows.html")
)

// servePageHandler serves an HTML page. Its layout values (widths, paddings, font
// sizes) and polling intervals are written in the page's own HTML.
func servePageHandler(page *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, nil); err != nil {
			log.Printf("template %s: %v", page.Name(), err)
		}
	}
}
