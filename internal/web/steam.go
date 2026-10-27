package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
)

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
