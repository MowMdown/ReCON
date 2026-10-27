package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/rcon"
)

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
	out, acked, err := rcon.Run(cmd, 2*time.Second, false, "web")
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
	rep, err := rcon.KickAll(body.Reason, "web")
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
		_, acked, err := rcon.Run("#"+name, 2*time.Second, true, "web")
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
		_, acked, err := rcon.Run("#shutdown", 2*time.Second, false, "web")
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := rcon.KickAll("Server restarting", "web")
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
