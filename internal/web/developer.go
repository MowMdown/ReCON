package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/geoip"
	"github.com/MowMdown/ReCON/internal/steam"
)

// The Developer page (/developer) shows every player and everything ReCON
// holds about them, raw: the tracker and flow runs in memory, every row of
// every table in players.db, the Steam cache and GeoIP, and what each flow
// {variable} would be. Nothing links to it; it only reads.

// devPlayerRow is one player in the Developer page's list.
type devPlayerRow struct {
	GUID     string   `json:"guid"`
	Name     string   `json:"name"`
	SteamID  string   `json:"steamId"`
	IPs      []string `json:"ips"`
	OnlineID *int     `json:"onlineId"` // their number when the tracker has them online
	Banned   bool     `json:"banned"`
}

// handleDevPlayers serves GET /api/developer/players: every player in the
// players table, with whether they are online (the tracker) and banned.
func handleDevPlayers(w http.ResponseWriter, r *http.Request) {
	list, err := database.DevPlayers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ids := game.DevOnlineIDs()
	bans := game.BanStatuses()
	out := make([]devPlayerRow, 0, len(list))
	for _, p := range list {
		g := strings.ToLower(p.GUID)
		row := devPlayerRow{GUID: p.GUID, Name: p.Name, SteamID: p.SteamID, IPs: p.IPs, Banned: bans[g].Banned}
		if id, ok := ids[g]; ok {
			row.OnlineID = &id
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "players": out})
}

// devSource is one section of the Developer page: where the data is held
// (memory, db, ext), its name, a short note and the data as it is.
type devSource struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Note  string `json:"note"`
	Data  any    `json:"data"`
}

// handleDevPlayer serves GET /api/developer/players/{guid}.
func handleDevPlayer(w http.ResponseWriter, r *http.Request) {
	guid := strings.ToLower(r.PathValue("guid"))
	if !battleye.GUIDPattern.MatchString(guid) {
		fail(w, http.StatusBadRequest, "not a GUID")
		return
	}
	tables, err := database.DevPlayerRows(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var sources []devSource

	tr := game.DevTrackerOf(guid)
	note := "not online"
	if tr.Online != nil {
		note = "online now: what the console log and the polls have told ReCON"
	}
	sources = append(sources, devSource{"memory", "Player tracker (game tracker)", note, tr})
	var listed any
	if p, ok := game.PlayerByGUID(guid); ok {
		listed = p
	}
	sources = append(sources, devSource{"memory", "Online list (game.OnlinePlayers)", "the player as the Players page and flows see them", listed})
	runs := flows.DevRuns(guid)
	sources = append(sources, devSource{"memory", "Flow runs (flows.flowRuns)", "each flow's current or last run since ReCON started", runs})
	var timed any
	timedNote := "none held"
	if tb, ok := database.DevTimedBanOf(guid); ok {
		timed, timedNote = tb, "held in memory until it runs out"
	}
	sources = append(sources, devSource{"memory", "Timed ban (store, memory only)", timedNote, timed})
	sources = append(sources, devSource{"memory", "Ban list (game)", "", game.DevBanState(guid)})
	for _, t := range tables {
		n := len(t.Rows)
		note := "no row"
		switch {
		case n == 1:
			note = "1 row"
		case n > 1:
			note = strconv.Itoa(n) + " rows"
		}
		var data any = t.Rows
		if n == 0 {
			data = nil
		} else if t.Table == "players" || t.Table == "active_sessions" || t.Table == "player_notes" || t.Table == "bans" {
			data = t.Rows[0]
		}
		sources = append(sources, devSource{"db", t.Table, note, data})
	}
	sources = append(sources, devSource{"ext", "Steam (cache)", "memory only: nothing is fetched from Steam here", steam.Dev(guid)})
	ips := map[string]string{}
	for _, t := range tables {
		for _, row := range t.Rows {
			for _, col := range []string{"ip", "last_ip"} {
				if ip, ok := row[col].(string); ok && ip != "" {
					ips[ip] = geoip.Country(ip)
				}
			}
		}
	}
	if tr.Online != nil {
		ips[tr.Online.IP] = geoip.Country(tr.Online.IP)
	}
	geoNote := "no database loaded"
	info := geoip.Info()
	if info != nil {
		geoNote = "country of each IP"
	}
	sources = append(sources, devSource{"ext", "GeoIP", geoNote, map[string]any{"database": info, "countries": ips}})

	name := ""
	steamID := ""
	for _, t := range tables {
		if t.Table == "players" && len(t.Rows) > 0 {
			name, _ = t.Rows[0]["username"].(string)
			steamID, _ = t.Rows[0]["steam_id"].(string)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "guid": guid, "name": name, "steamId": steamID,
		"variables": flows.DevVariables(guid), "sources": sources,
	})
}
