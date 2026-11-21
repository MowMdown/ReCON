package livemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The API: the mod's syncs, and the Live Map page's view of the live
// player data and the actions on players and the world.

// handleModSync serves POST /api/mod/sync: the mod's player data in, the
// commands to run out. A refusal is still a JSON reply ({ok: false, error}),
// so the mod can log why.
func handleModSync(w http.ResponseWriter, r *http.Request) {
	var req SyncRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		// Logged (once per different error), as the sync isn't otherwise:
		// the start of what the mod sent shows what ReCON couldn't read.
		start := string(body)
		if len(start) > 300 {
			start = start[:300] + "..."
		}
		logRefusalKeyed("read: "+err.Error(), fmt.Sprintf("couldn't read what %s sent (%v): %s", r.RemoteAddr, err, start))
		writeJSON(w, http.StatusBadRequest, SyncReply{Error: "ReCON couldn't read the data: " + err.Error(), Commands: []Command{}})
		return
	}
	reply, err := Sync(req, time.Now())
	if reply.Commands == nil {
		reply.Commands = []Command{}
	}
	code := http.StatusOK
	if errors.Is(err, ErrRefused) {
		code = http.StatusForbidden
	} else if err != nil {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, reply)
}

// handleModPlayers serves GET /api/mod/players: whether the mod is
// connected, every online player's live state, and the recent commands.
func handleModPlayers(w http.ResponseWriter, r *http.Request) {
	st, players := Current(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mod": st, "players": players, "commands": Commands("")})
}

// handleModItems serves GET /api/mod/items: every item the server can
// spawn, sorted by name (complete: false while the mod is still sending them).
func handleModItems(w http.ResponseWriter, r *http.Request) {
	items, complete := Catalog(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items, "complete": complete})
}

// handleModVehicles serves GET /api/mod/vehicles: every vehicle on the
// server, as the mod last sent them (sent: false when it hasn't, as a mod
// before 0.0.1i doesn't).
func handleModVehicles(w http.ResponseWriter, r *http.Request) {
	vs, at, sent := Vehicles(time.Now())
	out := map[string]any{"ok": true, "vehicles": vs, "sent": sent}
	if sent {
		out["at"] = at
	}
	writeJSON(w, http.StatusOK, out)
}

// handleModWorldEvents serves GET /api/mod/worldevents: the map's dynamic
// events (helicopter crashes, toxic zones) as the mod last sent them.
func handleModWorldEvents(w http.ResponseWriter, r *http.Request) {
	es, sent := WorldEvents(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": es, "sent": sent})
}

// handleModPredators serves GET /api/mod/predators: the bears, wolves and
// guard dogs as the mod last sent them.
func handleModPredators(w http.ResponseWriter, r *http.Request) {
	ps, sent := Predators(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "predators": ps, "sent": sent})
}

// handleModPlaces serves GET /api/mod/places: the map's place names.
func handleModPlaces(w http.ResponseWriter, r *http.Request) {
	ps, world, sent := Places(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "places": ps, "world": world, "sent": sent})
}

// handleModDeaths serves GET /api/mod/deaths?hours=24: every death in the
// last hours (1 to 720; at most 1000), newest first, for the map.
func handleModDeaths(w http.ResponseWriter, r *http.Request) {
	hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || hours < 1 || hours > 720 {
		hours = 24
	}
	ks, err := deathsSince(time.Now().Add(-time.Duration(hours)*time.Hour), 1000)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deaths": ks})
}

// handleModVehicle serves GET /api/mod/vehicles/{id}: one vehicle, and its
// damage zones, parts and cargo once the mod has sent them. ?ask=1 asks the
// mod for them afresh (they come a sync or so later); without it, it only
// reads what came in.
func handleModVehicle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	if r.URL.Query().Get("ask") == "1" {
		AskVehicle(id, now)
	}
	v, d, haveDetail, ok := VehicleOf(id, now)
	if !ok {
		fail(w, http.StatusNotFound, "that vehicle isn't on the server any more")
		return
	}
	out := map[string]any{"ok": true, "vehicle": v}
	if haveDetail {
		out["detail"] = d
	}
	writeJSON(w, http.StatusOK, out)
}

// handleModPlayer serves GET /api/mod/players/{guid}: one player's live
// state (live: false when they aren't online or the mod isn't connected),
// their kills and deaths, and the commands sent to them.
func handleModPlayer(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	now := time.Now()
	st, _ := Current(now)
	p, live := PlayerOf(guid, now)
	out := map[string]any{"ok": true, "mod": st, "live": live, "commands": Commands(guid)}
	if live {
		out["player"] = p
		// A page is looking at them: have the mod send their inventory.
		Watch(guid, now)
		if inv, ok := InventoryOf(guid, now); ok {
			out["inventory"] = inv
		}
	}
	if prev, ok := Previous(guid); ok {
		out["previous"] = prev
	}
	if kills, deaths, err := killCounts(guid); err == nil {
		out["kills"], out["deaths"] = kills, deaths
	}
	if recent, err := killsOf(guid, 100); err == nil {
		out["recentKills"] = recent
	}
	writeJSON(w, http.StatusOK, out)
}

// handleModAction serves POST /api/mod/players/{guid}/{action}: queue an
// action (heal, kill, teleport, teleportTo, back, set, message) for the mod.
// Body: teleport {x, y, z}; teleportTo {target: GUID}; back {} (to where
// they were before their last teleport); set {stat, value}; message {text,
// title, seconds}; spawn {text: class name, value: how many}.
func handleModAction(w http.ResponseWriter, r *http.Request) {
	var c Command
	if err := decodeBody(w, r, &c, 1<<16); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	c.Action = r.PathValue("action")
	cs, err := Queue(r.PathValue("guid"), c, time.Now())
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
	var c Command
	if err := decodeBody(w, r, &c, 1<<16); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	c.Action = r.PathValue("action")
	cs, err := QueueWorld(c, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command": cs})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// decodeBody decodes a JSON body of at most limit bytes into dst; an empty
// body leaves dst as it is.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
