// Package web is the HTTP layer: routes, the JSON API and page rendering.
// The pages' HTML and JavaScript live in templates/*.html and are embedded
// into the binary at build time. Every route is listed in buildWebRoutes.
package web

import (
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

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/store"
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
	// The theme's stylesheets and scripts (assets/, see base.html "theme").
	mux.Handle("GET /assets/", http.FileServerFS(assetFiles))
	// For anything that asks for /favicon.ico without reading the page's
	// icon links (browsers' bookmarks, link previews). Browsers take a PNG.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assetFiles, "assets/icons/favicon-32.png")
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// The status bar on every page: {"rcon": "connected" | "retrying" |
	// "unavailable"}, and while not connected either "waiting": true (a
	// login was sent and ReCON is waiting for the server to answer it) or
	// "retryInMs": time until the next check of the game server.
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := map[string]any{"ok": true, "rcon": game.Status()}
		if st["rcon"] != "connected" {
			if game.WaitingForLogin() {
				st["waiting"] = true
			} else {
				st["retryInMs"] = max(time.Until(game.NextCheck()).Milliseconds(), 0)
			}
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
	"/health": true, "/favicon.ico": true, "/players": true, "/bans": true, "/flows": true,
	"/api/players": true, "/api/bans": true, "/api/flows": true, "/api/status": true,
	"/api/console": true,
}

// isQuiet reports whether a request is a routine read: a quietPaths GET, a
// theme file (/assets/...), or the Message and Details dialogs reading one
// player (/api/players/<guid>/chat, /details, /steam).
func isQuiet(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	return quietPaths[p] || strings.HasPrefix(p, "/assets/") || (strings.HasPrefix(p, "/api/players/") &&
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

// templateFiles holds templates/*.html, compiled into the binary.
// base.html defines the shared "style" and "reorder" blocks.
//
//go:embed templates/*.html
var templateFiles embed.FS

// assetFiles are the theme's stylesheets and scripts, served at /assets/.
//
//go:embed assets
var assetFiles embed.FS

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

// handleRconSettingsGet serves GET /api/rcon-settings: the overrides in use
// ("settings", empty fields use the defaults; memory only), the defaults from
// docker-compose.yaml ("defaults", password included: ReCON runs on the game
// host behind its firewall) and the endpoint ReCON uses now ("address").
func handleRconSettingsGet(w http.ResponseWriter, r *http.Request) {
	d := game.DefaultSettings()
	address, _ := game.Target()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"settings": game.CurrentOverrides(),
		"defaults": map[string]any{"host": d.Host, "port": d.Port, "password": d.Password},
		"address":  address,
		"rcon":     game.Status(),
	})
}

// handleRconSettingsConnect serves POST /api/rcon-settings {host, port,
// password}: empty fields use the defaults. It logs in with those settings
// on a new connection first (a new handshake, even when nothing changed);
// only if the server accepts does that become ReCON's RCON session and are
// the settings kept, in memory until ReCON restarts. A wrong address or
// password changes nothing.
func handleRconSettingsConnect(w http.ResponseWriter, r *http.Request) {
	var req game.Settings
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	req, err := req.Clean()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	address, password := req.Effective()
	if err := game.Reconnect(address, password); err != nil {
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
	game.Polled.Store(true)
	game.Fails.Store(0)
	game.KeepOverrides(req)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "address": address})
}

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
	out, acked, err := game.Run(cmd, 2*time.Second, false, "web")
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "command": cmd, "acked": acked, "output": out})
}

// handleKickAll serves POST /kickall: kicks every player and nothing else.
// Optional body {"reason": "..."}; reason defaults to "Server restarting".
// The battleye.KickReport is returned under "result".
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
	rep, err := game.KickAll(body.Reason, "web")
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
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
		_, acked, err := game.Run("#"+name, 2*time.Second, true, "web")
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
		_, acked, err := game.Run("#shutdown", 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := game.KickAll("Server restarting", "web")
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

// consoleShownLines is the most lines the console page keeps, so it is
// also the most one request returns (the newest ones).
const consoleShownLines = 1000

// handleConsoleEvents serves GET /api/console?after=N: the events newer than
// N (at most consoleShownLines, the newest), plus "last" to send as after=
// next time, and "connected": whether the game server is answering (Live or
// Offline on the page). The page polls it while the console is open.
func handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, last := game.Events.AfterWithLast(after, consoleShownLines)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events, "last": last, "connected": game.Status() == "connected"})
}

// consoleSayRe matches a private message typed in the console (say 3 hi),
// so it can be added to that player's history. say -1 (everyone) is not.
var consoleSayRe = regexp.MustCompile(`(?i)^say\s+(\d+)\s+(.+)$`)

// handleConsoleCommand serves POST /api/console {"command": "..."}: runs one
// command exactly as typed; game.Run puts it and its outcome in the
// console log (as game.ConsoleSource). kickall is
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
	cmd := battleye.CleanReason(req.Command)
	if cmd == "" {
		fail(w, http.StatusBadRequest, "no command")
		return
	}
	if low := strings.ToLower(cmd); low == "kickall" || strings.HasPrefix(low, "kickall ") {
		reason := strings.TrimSpace(cmd[len("kickall"):])
		rep, err := game.KickAll(reason, game.ConsoleSource)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": true, "output": rep.Summary(reason)})
		return
	}

	out, acked, err := game.Run(cmd, 3*time.Second, false, game.ConsoleSource)
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
	p, ok := game.PlayerByID(id)
	if !ok || p.GUID == "" {
		return
	}
	if err := database.AddChatMessage(time.Now(), p.GUID, store.ChatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// database is the player database, set at startup (SetDatabase).
var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }
