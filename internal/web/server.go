package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/rcon"
)

// Serve serves every route (routes) until the listener fails.
func Serve(addr string) error {
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
		st := map[string]any{"ok": true, "rcon": rcon.Status()}
		if st["rcon"] != "connected" {
			st["retryInMs"] = max(time.Until(rcon.NextCheck()).Milliseconds(), 0)
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
	mux.HandleFunc("GET /api/players/{guid}/steam", handlePlayerSteam)         // Steam Profile tab; ?refresh=1 fetches now
	mux.HandleFunc("POST /api/players/{guid}/steam/search", handleSteamSearch) // find a player's SteamID from their GUID

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
	mux.HandleFunc("GET /api/bans/export", handleBansExport)         // download the permanent bans as bans.json; optional ?guids=a,b
	mux.HandleFunc("POST /api/bans/import", handleBansImport)        // add permanent bans from a bans.json body

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
	mux.HandleFunc("GET /api/flows/export", handleFlowExport)     // download flows.json; optional ?ids=0,3,5
	mux.HandleFunc("POST /api/flows/import", handleFlowImport)    // {flows: [...]}
	mux.HandleFunc("POST /api/flows/command", handleQuickCommand) // {command: lock|unlock|shutdown|kickall}

	// Built-in chat commands (!help, !shout, ...): their settings, edited on the Flows page.
	mux.HandleFunc("GET /api/chat-commands", handleChatCommandsGet)
	mux.HandleFunc("POST /api/chat-commands", handleChatCommandsSave) // {settings: {...}}

	return mux
}

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
