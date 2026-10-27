package web

import (
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// consoleShownLines is the most lines the console page keeps, so it is
// also the most one request returns (the newest ones).
const consoleShownLines = 1000

// handleConsoleEvents serves GET /api/console?after=N: the events newer than
// N (at most consoleShownLines, the newest), plus "last" to send as after=
// next time. The page polls it while the console is open.
func handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, last := rcon.Events.AfterWithLast(after, consoleShownLines)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events, "last": last})
}

// consoleSayRe matches a private message typed in the console (say 3 hi),
// so it can be added to that player's history. say -1 (everyone) is not.
var consoleSayRe = regexp.MustCompile(`(?i)^say\s+(\d+)\s+(.+)$`)

// handleConsoleCommand serves POST /api/console {"command": "..."}: runs one
// command exactly as typed; rcon.Run puts it and its outcome in the
// console log (as rcon.ConsoleSource). kickall is
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
		rep, err := rcon.KickAll(reason, rcon.ConsoleSource)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": true, "output": rep.Summary(reason)})
		return
	}

	out, acked, err := rcon.Run(cmd, 3*time.Second, false, rcon.ConsoleSource)
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
	p, ok := players.ByID(id)
	if !ok || p.GUID == "" {
		return
	}
	if err := database.AddChatMessage(time.Now(), p.GUID, store.ChatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}
