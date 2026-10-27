package web

import (
	"log"
	"net/http"

	"github.com/MowMdown/ReCON/internal/flows"
)

// handleChatCommandsGet serves GET /api/chat-commands: every command with
// its labels, defaults and allowed placeholders ("commands"), and the
// current settings.
func handleChatCommandsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commands": flows.ChatCommandDefs,
		"settings": flows.CurrentChatSettings(), "defaults": flows.DefaultChatSettings()})
}

// handleChatCommandsSave serves POST /api/chat-commands {"settings": {...}}.
func handleChatCommandsSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings flows.ChatSettings `json:"settings"`
	}
	if err := decodeBody(w, r, &req); err != nil || req.Settings == nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	saved, err := flows.SaveChatSettings(req.Settings)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("chat commands: settings saved")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": saved})
}
