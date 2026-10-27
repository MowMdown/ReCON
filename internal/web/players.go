package web

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// readLivePlayers reads the connected players (retrying read). ok is false when
// RCON can't be reached; the 503 has already been written in that case.
func readLivePlayers(w http.ResponseWriter) ([]battleye.Player, bool) {
	out, acked, err := rcon.Run("players", 3*time.Second, true, "")
	if err != nil || !acked {
		isRconUnavailable(w, "RCON unavailable")
		return nil, false
	}
	return battleye.ParsePlayersReply(out), true
}

// handlePlayersJSON serves GET /api/players: online players (each with their
// role, for the Mod/Admin badge, and the last line they wrote in chat), the
// offline ones from the DB, and "bans": lower-case GUID -> ban status for
// everyone banned (the offline table's Ban Status column). It answers
// straight away even while RCON is down ("connected": false, no online
// players), so the offline list always shows.
func handlePlayersJSON(w http.ResponseWriter, r *http.Request) {
	online, connected := onlinePlayersForPage()
	offlinePlayers, err := database.ListOfflinePlayers()
	if err != nil {
		log.Printf("error reading offline online from DB: %v", err)
		offlinePlayers = []battleye.Player{}
	}
	if roles, err := database.AllPlayerRoles(); err == nil {
		for i := range online {
			online[i].Role = roles[strings.ToLower(online[i].GUID)]
		}
	}
	// The Last Message column: the newest line each online player wrote
	// this session, kept in memory by the player tracker (online.go).
	last := players.LastChatPerPlayer()
	for i := range online {
		if c, ok := last[online[i].ID]; ok && strings.EqualFold(c.GUID, online[i].GUID) {
			online[i].LastMessage, online[i].LastMessageChannel, online[i].LastMessageUnix = c.Chat.Text, c.Chat.Channel, c.Chat.At.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"connected":      connected,
		"online":         online,
		"count":          len(online),
		"offlinePlayers": offlinePlayers,
		"bans":           bans.AllStatuses(),
	})
}

// onlinePlayersForPage is the online list for the Players page: the
// tracker's (package players), kept up to date by the server watch's polls, so
// the page never waits on RCON. Before the first poll since the server last
// answered, it polls once itself. ok is false while the server isn't
// answering.
func onlinePlayersForPage() ([]battleye.Player, bool) {
	if rcon.Status() == "unavailable" {
		return []battleye.Player{}, false
	}
	online, known := players.Online()
	if known {
		return online, true
	}
	if !players.PollNow() {
		return []battleye.Player{}, false
	}
	online, _ = players.Online()
	return online, true
}

// handleAllPlayersJSON serves GET /api/players/all: every player ever seen,
// from the database only, so it works while RCON is down.
func handleAllPlayersJSON(w http.ResponseWriter, r *http.Request) {
	players, err := database.ListKnownPlayers()
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
		msg := battleye.CleanReason(req.Message)
		if msg == "" {
			fail(w, http.StatusBadRequest, "message is required")
			return
		}
		_, acked, err := rcon.Run(fmt.Sprintf("say %d %s", id, msg), 2*time.Second, false, "web")
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
	var p battleye.Player
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
	req.Reason = battleye.CleanReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	if action == "kick" {
		_, acked, err := rcon.Run(fmt.Sprintf("kick %d %s", p.ID, req.Reason), 1500*time.Millisecond, false, "web")
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

// banOnlinePlayer bans player p from the pages and writes the JSON response.
func banOnlinePlayer(w http.ResponseWriter, p battleye.Player, online bool, req actionRequest) {
	res := bans.BanGUID(p, online, req.Minutes, req.Reason, "web")
	writeJSON(w, res.Code, res)
}

// maxNoteLen is the longest admin note, in characters.
const maxNoteLen = 500

// chatHistoryLimit is how many lines GET /api/players/{guid}/chat returns:
// the newest ones, oldest first. Everything stays in the table.
const chatHistoryLimit = 300

// playerGUID returns the {guid} path value, lower-cased; false (with a 404
// written) when it isn't a GUID.
func playerGUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	guid := r.PathValue("guid")
	if !battleye.GUIDPattern.MatchString(guid) {
		fail(w, http.StatusNotFound, "invalid player GUID")
		return "", false
	}
	return strings.ToLower(guid), true
}

// handlePlayerDetails serves GET /api/players/{guid}/details: everything
// logged about one player (store.Database.GetPlayerDetails), plus whether they are
// online right now and their ban status. Works for offline players and
// while RCON is down.
func handlePlayerDetails(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	d, err := database.GetPlayerDetails(guid)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	} else if err != nil {
		log.Printf("details %s: %v", guid, err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, on := players.ByGUID(guid); on {
		d.Player.Online, d.Player.ID = true, p.ID
		d.Player.Name = battleye.BareName(p.Name)
	}
	d.Ban = bans.StatusOf(guid)
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
	known, err := database.IsPlayerKnown(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	if err := database.SavePlayerNote(guid, note); err != nil {
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
	if *req.Role < flows.RolePlayer || *req.Role > flows.RoleAdmin {
		fail(w, http.StatusBadRequest, "role must be 0 (Player), 1 (Mod) or 2 (Admin)")
		return
	}
	found, err := database.SetPlayerRole(guid, *req.Role)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	log.Printf("roles: %s is now %s", guid, flows.RoleName(*req.Role))
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
	lines, err := database.GetChatHistory(guid, chatHistoryLimit)
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
	msg := battleye.CleanReason(req.Message)
	if msg == "" {
		fail(w, http.StatusBadRequest, "message is required")
		return
	}
	players, ok := readLivePlayers(w)
	if !ok {
		return
	}
	var p battleye.Player
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
	_, acked, err := rcon.Run(cmd, 2*time.Second, false, "web")
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if !acked {
		fail(w, http.StatusBadGateway, "message was not acknowledged by the server")
		return
	}
	if err := database.AddChatMessage(time.Now(), guid, store.ChatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
