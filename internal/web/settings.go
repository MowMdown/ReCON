package web

import (
	"errors"
	"log"
	"net/http"
	"syscall"

	"github.com/MowMdown/ReCON/internal/rcon"
)

// handleRconSettingsGet serves GET /api/rcon-settings: the overrides in use
// ("settings", empty fields use the defaults; memory only), the defaults from
// docker-compose.yaml ("defaults", password included: ReCON runs on the game
// host behind its firewall) and the endpoint ReCON uses now ("address").
func handleRconSettingsGet(w http.ResponseWriter, r *http.Request) {
	d := rcon.DefaultSettings()
	address, _ := rcon.Target()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"settings": rcon.CurrentOverrides(),
		"defaults": map[string]any{"host": d.Host, "port": d.Port, "password": d.Password},
		"address":  address,
		"rcon":     rcon.Status(),
	})
}

// handleRconSettingsConnect serves POST /api/rcon-settings {host, port,
// password}: empty fields use the defaults. It logs in with those settings
// on a new connection first (a new handshake, even when nothing changed);
// only if the server accepts does that become ReCON's RCON session and are
// the settings kept, in memory until ReCON restarts. A wrong address or
// password changes nothing.
func handleRconSettingsConnect(w http.ResponseWriter, r *http.Request) {
	var req rcon.Settings
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
	if err := rcon.Reconnect(address, password); err != nil {
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
	rcon.Polled.Store(true)
	rcon.Fails.Store(0)
	rcon.KeepOverrides(req)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "address": address})
}
