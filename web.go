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
	"strconv"
	"strings"
	"time"
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

	// Raw RCON, for schedulers and scripts.
	mux.HandleFunc("/rcon", handleRcon)            // GET ?cmd=... or POST (raw text or {"command": ...})
	mux.HandleFunc("POST /kickall", handleKickAll) // optional {"reason": ...}

	// Players.
	mux.HandleFunc("GET /players", pageHandler(playersPage))
	mux.HandleFunc("GET /api/players", handlePlayersJSON)                 // online + offline players
	mux.HandleFunc("GET /api/players/all", handleAllPlayersJSON)          // every player ever seen (no RCON needed)
	mux.HandleFunc("POST /api/players/{id}/{action}", handlePlayerAction) // message | kick | ban

	// Bans.
	mux.HandleFunc("GET /bans", pageHandler(bansPage))
	mux.HandleFunc("GET /api/bans", handleBansJSON)            // list GUID bans (also syncs to DB)
	mux.HandleFunc("POST /api/bans/{guid}/ban", handleBanGUID) // ban a known player, online or not
	mux.HandleFunc("POST /api/bans/{guid}/unban", handleUnban) // remove every entry for a GUID

	// Flows.
	mux.HandleFunc("GET /flows", pageHandler(flowsPage))
	mux.HandleFunc("GET /api/flows", handleFlowsList)                   // flows + current/last run of each
	mux.HandleFunc("POST /api/flows", handleFlowSave)                   // create/update {id, name, script, trigger, schedule}
	mux.HandleFunc("POST /api/flows/{id}/run", withFlow(handleFlowRun)) // optional {"minutes": N}
	mux.HandleFunc("POST /api/flows/{id}/stop", withFlow(handleFlowStop))
	mux.HandleFunc("POST /api/flows/{id}/enable", withFlow(handleFlowEnable))
	mux.HandleFunc("POST /api/flows/{id}/disable", withFlow(handleFlowDisable))
	mux.HandleFunc("POST /api/flows/{id}/delete", withFlow(handleFlowDelete))
	mux.HandleFunc("POST /api/flows/preview", handleFlowPreview)  // {trigger, schedule, script}
	mux.HandleFunc("POST /api/flows/reorder", handleFlowReorder)  // {ids: [...]}
	mux.HandleFunc("GET /api/flows/export", handleFlowExport)     // download flows.json
	mux.HandleFunc("POST /api/flows/import", handleFlowImport)    // {flows: [...]}
	mux.HandleFunc("POST /api/flows/command", handleQuickCommand) // {command: lock|unlock|shutdown|kickall}

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
	"/api/players": true, "/api/bans": true, "/api/flows": true,
}

// logRequests logs each request (method, path+query, remote address,
// X-Forwarded-For, user agent), except quietPaths GETs.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !(r.Method == http.MethodGet && quietPaths[r.URL.Path]) {
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
func rconUnavailable(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "connected": false, "retrying": true, "error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// decodeBody decodes a JSON request body (capped at 16 KiB) into dst. An
// empty body is not an error: dst keeps its zero values.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst)
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
	rep, err := kickAll(sharedRunner{logAs: "web"}, body.Reason)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
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

// handlePlayersJSON serves GET /api/players: live players plus offline ones from the DB.
func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, ok := livePlayers(w)
	if !ok {
		return
	}
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

// banPlayer bans player p by GUID and writes the JSON response. online says
// whether p is connected right now (p.ID is only meaningful then).
//
//  1. `addBan <guid> <minutes> <reason>` (0 minutes = permanent). Failure -> 502.
//  2. If online, kick with the same reason. A failed kick is only reported ("kicked").
//  3. Record GUID/name/IP/reason in SQLite (the IP is informational, not banned).
func banPlayer(w http.ResponseWriter, p Player, online bool, req actionRequest) {
	banListLock.Lock()
	defer banListLock.Unlock()
	_, guidAck, guidErr := runShared(fmt.Sprintf("addBan %s %d %s", p.GUID, req.Minutes, req.Reason), 1500*time.Millisecond, false, "web")
	if guidErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "guid_ban": false, "guid_error": guidErr.Error()})
		return
	}
	if !guidAck {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "guid_ban": false, "error": "GUID ban was not acknowledged"})
		return
	}
	kicked := false
	if online {
		_, kickAck, kickErr := runShared(fmt.Sprintf("kick %d %s", p.ID, req.Reason), 1500*time.Millisecond, false, "web")
		kicked = kickErr == nil && kickAck
		if !kicked {
			log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
		}
	}
	perm := 0
	if req.Minutes == 0 {
		perm = 1
	}
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

// handleFlowRun: optional body {"minutes": N} puts the 0 point N minutes away.
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

// scriptText is a flow script inside an export/import file: an array of lines
// (what Export writes) or one string with \n line breaks.
type scriptText string

func (s scriptText) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false) // keep & < > readable in the file
	if err := enc.Encode(strings.Split(strings.ReplaceAll(string(s), "\r\n", "\n"), "\n")); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(sb.String())), nil
}

func (s *scriptText) UnmarshalJSON(b []byte) error {
	var lines []string
	if err := json.Unmarshal(b, &lines); err == nil {
		*s = scriptText(strings.Join(lines, "\n"))
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return errors.New("script must be an array of lines or a string")
	}
	*s = scriptText(one)
	return nil
}

type flowFileEntry struct {
	Name     string     `json:"name"`
	Schedule string     `json:"schedule"` // "manual", "on-join" or a cron expression
	Script   scriptText `json:"script"`
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
		out.Flows = append(out.Flows, flowFileEntry{Name: f.Name, Schedule: f.Schedule, Script: scriptText(f.Script)})
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
// and reported. Each entry's schedule says its trigger.
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
		if _, err := validateAndSaveFlow(Flow{Name: name, Script: string(e.Script), Schedule: e.Schedule}); err != nil {
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
	var req struct {
		Trigger  string `json:"trigger"`
		Schedule string `json:"schedule"`
		Script   string `json:"script"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	writeJSON(w, http.StatusOK, previewFlow(req.Trigger, req.Schedule, req.Script))
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
		rep, err := kickAll(sharedRunner{logAs: "web"}, "Server restarting")
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

// pageHandler serves an HTML page rendered with pageSettings.
func pageHandler(page *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, pageSettings); err != nil {
			log.Printf("template %s: %v", page.Name(), err)
		}
	}
}
