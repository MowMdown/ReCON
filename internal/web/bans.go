package web

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
)

// handleBansJSON serves GET /api/bans: every ban, the permanent ones from
// the bans table and the timed ones from memory. The server watch keeps both in
// step with BattlEye (bans.SyncWithServer), and ban and unban update them
// straight away, so the page never waits on RCON.
// "connected" is false while the game server isn't answering.
func handleBansJSON(w http.ResponseWriter, r *http.Request) {
	list, err := database.ListAllBans()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": rcon.Status() == "connected", "bans": list, "count": len(list)})
}

// guidParam returns the {guid} path value, lower-cased. ok is false (and a
// 404 has been written) when it isn't a valid GUID.
func guidParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	guid := r.PathValue("guid")
	if !battleye.GUIDPattern.MatchString(guid) {
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
	req.Reason = battleye.CleanReason(req.Reason)
	if req.Reason == "" {
		req.Reason = "No reason specified"
	}

	players, ok := readLivePlayers(w)
	if !ok {
		return
	}
	for _, p := range players {
		if strings.EqualFold(p.GUID, guid) {
			banOnlinePlayer(w, p, true, req)
			return
		}
	}
	p, err := database.PlayerByGUID(guid)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	banOnlinePlayer(w, p, false, req)
}

// handlePlayerUnban serves POST /api/bans/{guid}/unban.
//
// BattlEye removes bans by list index, indexes shift after every removal, and
// one GUID can be listed more than once. So it loops: re-read the list, find
// the first entry for the GUID, removeBan <index>, until the GUID is gone
// (safety cap: 20). Then `writeBans` (best effort, reported as "saved") and
// the DB row is deleted, which is the only way a permanent ban leaves the
// database (bans.SyncWithServer puts back any permanent ban BattlEye loses). Partial
// states are reported via be_removed/db_removed.
func handlePlayerUnban(w http.ResponseWriter, r *http.Request) {
	guid, ok := guidParam(w, r)
	if !ok {
		return
	}
	bans.ListLock.Lock()
	defer bans.ListLock.Unlock()

	removed := 0
	for ; removed < 20; removed++ {
		bans, err := bans.ReadServerList()
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
		_, acked, err := rcon.Run(fmt.Sprintf("removeBan %d", idx), 1500*time.Millisecond, false, "web")
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

	_, saved, _ := rcon.Run("writeBans", 1500*time.Millisecond, false, "web") // persist bans.txt, best effort

	err := database.DeletePlayerBan(guid)
	bans.RefreshExpiry()
	if err != nil {
		log.Printf("error deleting ban from DB: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "be_removed": true, "db_removed": false, "error": "unbanned in BattlEye but database update failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "be_removed": removed > 0, "db_removed": true, "removed": removed, "saved": saved})
}
