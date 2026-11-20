package web

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/MowMdown/ReCON/internal/modlink"
	"github.com/MowMdown/ReCON/internal/serverinfo"
)

// The Server Info page (/server, the default page): what the game server
// tells Steam's server query, its uptime, population and busiest times
// from the session history, and its mods with Steam's updates.

// serverMod is a mod as the page shows it.
type serverMod struct {
	WorkshopID   string `json:"workshopId"`
	Name         string `json:"name"`
	SizeBytes    int64  `json:"sizeBytes"`
	SteamUpdated int64  `json:"steamUpdated"` // Unix seconds, 0 until Steam was asked
	Updated      bool   `json:"updated"`      // updated on Steam since it was acknowledged
}

// handleServerInfo serves GET /api/server.
func handleServerInfo(w http.ResponseWriter, r *http.Request) {
	snap, cfg := serverinfo.Current()
	now := time.Now()
	// recon-mod knows the in-game date: better than SERVER_TIME's.
	if st, _ := modlink.Current(now); st.Connected && st.Date[0] > 0 {
		cfg.ServerTime = fmt.Sprintf("%d/%d/%d/%02d/%02d", st.Date[0], st.Date[1], st.Date[2], st.Date[3], st.Date[4])
	}
	out := map[string]any{"ok": true, "server": snap,
		"sun":       serverinfo.SunFor(cfg, snap.Info.Map, now),
		"modsKnown": snap.ModsKnown,
	}
	// Uptime over the last 7 days, of the time ReCON was running.
	week := now.Add(-7 * 24 * time.Hour)
	serverinfo.FlushUptime(now)
	if up, down, err := database.Uptime(week, now); err == nil && up+down > 0 {
		out["uptime"] = map[string]any{"percent": 100 * float64(up) / float64(up+down), "downSeconds": int(down.Seconds()), "seenSeconds": int((up + down).Seconds())}
	}
	if spans, err := database.SessionSpans(week, now); err == nil {
		out["week"] = serverinfo.SumUp(spans, week, now)
	}
	// Mods, with Steam's sizes and updates.
	rows, _ := database.ModUpdates()
	mods := []serverMod{}
	var total int64
	for _, m := range snap.Mods {
		if m.DLC {
			continue
		}
		id := strconv.FormatUint(m.WorkshopID, 10)
		sm := serverMod{WorkshopID: id, Name: m.Name}
		if row, ok := rows[id]; ok {
			if row.Name != "" {
				sm.Name = row.Name // Steam's title reads better than the folder name
			}
			sm.SizeBytes, sm.SteamUpdated = row.SizeBytes, row.SteamUpdated
			sm.Updated = row.SteamUpdated > row.AckedUpdated
			total += row.SizeBytes
		}
		mods = append(mods, sm)
	}
	out["mods"], out["modsBytes"] = mods, total
	checked, checkErr := serverinfo.ModsChecked()
	if !checked.IsZero() {
		out["modsChecked"] = checked
	}
	if checkErr != "" {
		out["modsCheckError"] = checkErr
	}
	writeJSON(w, http.StatusOK, out)
}

// handleServerRefresh serves POST /api/server/refresh: ask the server now.
func handleServerRefresh(w http.ResponseWriter, r *http.Request) {
	serverinfo.Refresh()
	handleServerInfo(w, r)
}

// handleServerPopulation serves GET /api/server/population?range=12h|1d|7d|30d|all.
func handleServerPopulation(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	now := time.Now()
	var since time.Time
	if d, ok := serverinfo.Ranges[rng]; ok {
		since = now.Add(-d)
	} else if rng == "all" {
		first, err := database.FirstSessionStart()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		since = first // from the first session ReCON recorded
		if since.IsZero() {
			since = now.Add(-24 * time.Hour) // none yet: an empty day
		} else if now.Sub(since) < time.Minute {
			since = now.Add(-time.Minute)
		}
	} else {
		fail(w, http.StatusBadRequest, "range is 12h, 1d, 7d, 30d or all")
		return
	}
	spans, err := database.SessionSpans(since, now)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": since.Unix(), "to": now.Unix(),
		"points": serverinfo.Population(spans, since, now)})
}

// handleServerBusiest serves GET /api/server/busiest?tz=America/New_York:
// the average players per weekday and hour over the last 4 weeks, in the
// viewer's time zone.
func handleServerBusiest(w http.ResponseWriter, r *http.Request) {
	loc, err := time.LoadLocation(r.URL.Query().Get("tz"))
	if err != nil {
		loc = time.UTC
	}
	now := time.Now()
	since := now.AddDate(0, 0, -28)
	if first, err := database.FirstSessionStart(); err == nil && first.After(since) {
		since = first // average only over what has been recorded
	}
	spans, err := database.SessionSpans(since, now)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	b := serverinfo.Busiest(spans, since, now, loc)
	peak := 0.0
	for _, d := range b {
		for _, v := range d {
			peak = max(peak, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tz": loc.String(), "days": b, "peak": peak})
}

// handleServerModAck serves POST /api/server/mods/{id}/ack: the mod's
// latest Steam update has been seen.
func handleServerModAck(w http.ResponseWriter, r *http.Request) {
	found, err := database.AckModUpdate(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		fail(w, http.StatusNotFound, "unknown mod")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// serverSettingsView is GET /api/server/settings: what was saved on the
// page, and the container's values the empty fields use.
func serverSettingsView() map[string]any {
	def := serverinfo.Defaults()
	lat := "the map's (45 unless it is Chernarus)"
	if !math.IsNaN(def.Latitude) {
		lat = strconv.FormatFloat(def.Latitude, 'f', -1, 64)
	}
	snap, _ := serverinfo.Current()
	return map[string]any{"ok": true, "settings": serverinfo.SavedSettings(),
		"defaults": map[string]any{"queryPort": strconv.Itoa(def.QueryPort),
			"serverTime": def.ServerTime, "latitude": lat},
		"address": serverinfo.QueryAddress(), "answered": snap.Answered, "error": snap.Error}
}

// handleServerSettingsGet serves GET /api/server/settings.
func handleServerSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, serverSettingsView())
}

// handleServerSettingsSave serves POST /api/server/settings {queryPort,
// serverTime, latitude}: empty fields use the
// container's values. It saves them (nothing changes if one is wrong) and
// asks the server at once, so the answer says whether it replies.
func handleServerSettingsSave(w http.ResponseWriter, r *http.Request) {
	var s serverinfo.Settings
	if err := decodeBody(w, r, &s); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if _, err := serverinfo.SaveSettings(s); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	serverinfo.Refresh()
	writeJSON(w, http.StatusOK, serverSettingsView())
}
