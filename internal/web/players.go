package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
)

// readLivePlayers reads the connected players (retrying read). ok is false when
// RCON can't be reached; the 503 has already been written in that case.
func readLivePlayers(w http.ResponseWriter) ([]battleye.Player, bool) {
	out, acked, err := game.Run("players", 3*time.Second, true, "")
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
		log.Printf("error reading offline players from DB: %v", err)
		offlinePlayers = []battleye.Player{}
	}
	if roles, err := database.AllPlayerRoles(); err == nil {
		for i := range online {
			online[i].Role = roles[strings.ToLower(online[i].GUID)]
		}
	}
	// The Last Message column: the newest line each online player wrote
	// this session, kept in memory by the player tracker (game/players.go).
	last := game.LastChatPerPlayer()
	for i := range online {
		if c, ok := last[online[i].ID]; ok && strings.EqualFold(c.GUID, online[i].GUID) {
			online[i].LastMessage, online[i].LastMessageChannel, online[i].LastMessageUnix = c.Chat.Text, c.Chat.Channel, c.Chat.At.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"connected":      connected,
		"players":        online,
		"count":          len(online),
		"offlinePlayers": offlinePlayers,
		"bans":           game.BanStatuses(),
	})
}

// onlinePlayersForPage is the online list for the Players page: the
// tracker's (package players), kept up to date by the server watch's polls, so
// the page never waits on RCON. Before the first poll since the server last
// answered, it polls once itself. ok is false while the server isn't
// answering.
func onlinePlayersForPage() ([]battleye.Player, bool) {
	if game.Status() == "unavailable" {
		return []battleye.Player{}, false
	}
	online, known := game.OnlinePlayers()
	if known {
		return online, true
	}
	if !game.PollPlayers() {
		return []battleye.Player{}, false
	}
	online, _ = game.OnlinePlayers()
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
		_, acked, err := game.Run(fmt.Sprintf("say %d %s", id, msg), 2*time.Second, false, "web")
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
		_, acked, err := game.Run(fmt.Sprintf("kick %d %s", p.ID, req.Reason), 1500*time.Millisecond, false, "web")
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
	res := game.BanGUID(p, online, req.Minutes, req.Reason, "web")
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
	if p, on := game.PlayerByGUID(guid); on {
		d.Player.Online, d.Player.ID = true, p.ID
		d.Player.Name = battleye.BareName(p.Name)
	}
	d.Ban = game.BanStatusOf(guid)
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

// handleDeletePlayer serves DELETE /api/players/{guid} {"name": "..."}: the
// Danger tab's Delete Player. It removes the player and everything recorded
// about them (store.Database.DeletePlayer), keeping their ban record. name
// must be their name exactly; a player who is online can't be deleted, as
// their open session would add them straight back.
func handleDeletePlayer(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req); err != nil || req.Name == "" {
		fail(w, http.StatusBadRequest, `send {"name": "<the player's name>"}`)
		return
	}
	if _, online := game.PlayerByGUID(guid); online {
		fail(w, http.StatusConflict, "this player is online; a player can only be deleted while offline")
		return
	}
	err := database.DeletePlayer(guid, req.Name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	case errors.Is(err, store.ErrNameMismatch):
		fail(w, http.StatusBadRequest, "the name doesn't match; type the player's name exactly")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("players: deleted %s (%s) and everything recorded about them", req.Name, guid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	_, acked, err := game.Run(cmd, 2*time.Second, false, "web")
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

// handlePlayerSteam serves GET /api/players/{guid}/steam[?refresh=1]:
//
//	{ok, enabled, steamId, pending}                 always (steamId "" and pending true until it's found)
//	{..., search: {state, percent, ...}}            while pending (steam.SearchStatus)
//	{..., profileUrl}                               once the SteamID is known
//	{..., profile, friendsHere: [{guid, name}]}     when STEAM_API_KEY is set
//	{..., stale: "<error>"}                         Steam failed; profile is the last copy
func handlePlayerSteam(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	steamID, known, err := database.SteamIDByGUID(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	} else if !known {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	resp := map[string]any{"ok": true, "enabled": steam.APIKey != "", "steamId": steamID, "pending": steamID == ""}
	if steamID == "" {
		resp["search"] = steam.SearchStatus(guid)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	id, valid := steam.ParseID64(steamID)
	if !valid {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("the stored steam_id %q is not a SteamID64", steamID))
		return
	}
	if steam.BattlEyeGUID(id) != guid {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("the stored steam_id %s belongs to a different GUID (%s)", steamID, steam.BattlEyeGUID(id)))
		return
	}
	resp["profileUrl"] = "https://steamcommunity.com/profiles/" + steamID
	if steam.APIKey == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	p, err := steam.ProfileFor(ctx, steamID, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		log.Printf("steam: %s: %v", steamID, err)
		if p == nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		resp["stale"] = err.Error()
	}
	resp["profile"] = p
	here, err := database.PlayersBySteamIDs(p.FriendIDs)
	if err != nil {
		log.Printf("steam: friends of %s on this server: %v", steamID, err)
		here = []store.SharedPlayer{}
	}
	resp["friendsHere"] = here
	writeJSON(w, http.StatusOK, resp)
}

// handleSteamSearch serves POST /api/players/{guid}/steam/search: starts
// looking for the player's SteamID (see steam.StartSearch) and answers like
// GET /api/players/{guid}/steam, so the page can show the progress. A player
// who already has a SteamID gets 409.
func handleSteamSearch(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	steamID, known, err := database.SteamIDByGUID(guid)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	case !known:
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	case steamID != "":
		fail(w, http.StatusConflict, "this player's SteamID is already known")
		return
	}
	if err := steam.StartSearch(guid); err != nil {
		log.Printf("steam: starting a search for %s: %v", guid, err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": steam.APIKey != "", "steamId": "",
		"pending": true, "search": steam.SearchStatus(guid)})
}
