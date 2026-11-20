package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/MowMdown/ReCON/internal/modlink"
)

// recon-mod: the DayZ server-side mod's syncs, and the pages' view of the
// live player data and the actions on players.

// handleModSync serves POST /api/mod/sync: the mod's player data in, the
// commands to run out. A refusal is still a JSON reply ({ok: false, error}),
// so the mod can log why.
func handleModSync(w http.ResponseWriter, r *http.Request) {
	var req modlink.SyncRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, modlink.SyncReply{Error: "bad JSON", Commands: []modlink.Command{}})
		return
	}
	reply, err := modlink.Sync(req, time.Now())
	if reply.Commands == nil {
		reply.Commands = []modlink.Command{}
	}
	code := http.StatusOK
	if errors.Is(err, modlink.ErrRefused) {
		code = http.StatusForbidden
	} else if err != nil {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, reply)
}

// handleModPlayers serves GET /api/mod/players: whether the mod is
// connected, every online player's live state, and the recent commands.
func handleModPlayers(w http.ResponseWriter, r *http.Request) {
	st, players := modlink.Current(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mod": st, "players": players, "commands": modlink.Commands("")})
}

// handleModPlayer serves GET /api/mod/players/{guid}: one player's live
// state (live: false when they aren't online or the mod isn't connected),
// their kills and deaths, and the commands sent to them.
func handleModPlayer(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	now := time.Now()
	st, _ := modlink.Current(now)
	p, live := modlink.PlayerOf(guid, now)
	out := map[string]any{"ok": true, "mod": st, "live": live, "commands": modlink.Commands(guid)}
	if live {
		out["player"] = p
	}
	if kills, deaths, err := database.KillCounts(guid); err == nil {
		out["kills"], out["deaths"] = kills, deaths
	}
	if recent, err := database.KillsOf(guid, 20); err == nil {
		out["recentKills"] = recent
	}
	writeJSON(w, http.StatusOK, out)
}

// handleModAction serves POST /api/mod/players/{guid}/{action}: queue an
// action (heal, kill, teleport, teleportTo, set, message) for the mod.
// Body: teleport {x, y, z}; teleportTo {target: GUID}; set {stat, value};
// message {text, title, seconds}.
func handleModAction(w http.ResponseWriter, r *http.Request) {
	var c modlink.Command
	if err := decodeBody(w, r, &c); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	c.Action = r.PathValue("action")
	cs, err := modlink.Queue(r.PathValue("guid"), c, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command": cs})
}

// handleModWorld serves POST /api/mod/world/{action}: queue a weather or
// time change for the mod. Body: weather {stat, value, transition, hold};
// weatherMode {text}; storm {value, min, hold}; rainThresholds {min, max,
// transition}; date {year, month, day, hour, minute}; freezeTime {value}.
func handleModWorld(w http.ResponseWriter, r *http.Request) {
	var c modlink.Command
	if err := decodeBody(w, r, &c); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	c.Action = r.PathValue("action")
	cs, err := modlink.QueueWorld(c, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command": cs})
}
