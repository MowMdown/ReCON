package flows

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/steam"
)

// DevVariable is what one {placeholder} would be for a player right now, and
// where it is read from (the Developer page).
type DevVariable struct {
	Name   string `json:"name"`
	Flow   string `json:"flow,omitempty"` // {counter} and {threshold}: the flow they belong to
	Value  string `json:"value"`
	Known  bool   `json:"known"` // false: a step would leave it as typed (or "?"), an If on it is skipped
	Source string `json:"source"`
}

// devVariableSources says where each placeholder's value comes from.
var devVariableSources = map[string]string{
	"player_name":    "player tracker: name, without (Lobby)",
	"player_id":      "player tracker: player number",
	"player_ping":    "player tracker: ping from the last poll",
	"player_ip":      "player tracker: IP",
	"player_guid":    "player tracker: GUID",
	"player_role":    "players.role; an If compares the number (0 Player, 1 Mod, 2 Admin)",
	"players_online": "player tracker: players online",
	"message":        "chat flows only: the line that started the run",
}

// DevVariables is what every flow placeholder would be for the player with
// this GUID right now: online, from the tracker, else from the database as
// when they were last seen. The Steam ones come from the cache only, so
// opening the page never asks Steam.
func DevVariables(guid string) []DevVariable {
	g := strings.ToLower(guid)
	p, online := game.PlayerByGUID(g)
	if !online && database != nil {
		stored, err := database.PlayerByGUID(g)
		if err != nil {
			return []DevVariable{}
		}
		p = stored
		p.ID = -1
	}
	fp := &flowPlayer{Player: p, role: lookupPlayerRole(g)}
	msgSource := devVariableSources["message"]
	if c, ok := game.LatestChatOf(g); ok {
		msgSource += "; their last chat line is \"" + c.Text + "\""
	}
	x := &job{run: &flowRun{counters: map[string]int{}}, ctx: context.Background(), p: fp}
	where := "player tracker"
	if !online {
		where = "players table (offline: as last seen)"
	}
	out := []DevVariable{}
	for _, name := range []string{"player_name", "player_id", "player_ping", "player_ip", "player_guid", "player_role", "players_online", "message"} {
		v, ok := x.placeholderValue(name)
		src := devVariableSources[name]
		switch {
		case name == "message":
			v, ok, src = "", false, msgSource
		case !online && strings.HasPrefix(src, "player tracker:") && name != "players_online":
			src = strings.Replace(src, "player tracker", where, 1)
			if name == "player_id" || name == "player_ping" {
				v, ok = "", false
				src += ": not online"
			}
		}
		out = append(out, DevVariable{Name: name, Value: v, Known: ok, Source: src})
	}
	b, ok := steam.CachedBans(g)
	from := "Steam cache (" + b.From + ")"
	if !ok {
		from = "Steam cache: nothing held (no SteamID, no STEAM_API_KEY, or not fetched in the last 30 minutes)"
	}
	add := func(name string, v int, known bool, src string) {
		val := ""
		if known {
			val = strconv.Itoa(v)
		}
		out = append(out, DevVariable{Name: name, Value: val, Known: known, Source: src})
	}
	add("player_vac_bans", b.VAC, ok, from)
	add("player_game_bans", b.Game, ok, from)
	if ok && b.VAC+b.Game == 0 {
		add("player_days_since_last_ban", 0, false, from+": no bans, so no last ban")
	} else {
		add("player_days_since_last_ban", b.DaysSinceLast, ok, from)
	}

	// {counter} and {threshold} belong to each flow.
	list, _ := List()
	flowRuns.Lock()
	runs := make(map[int64]*flowRun, len(flowRuns.byFlow))
	for id, r := range flowRuns.byFlow {
		runs[id] = r
	}
	flowRuns.Unlock()
	for _, f := range list {
		src := "no run since ReCON started: 0"
		n := 0
		if r := runs[f.ID]; r != nil {
			n = r.counterOf(g)
			src = "this flow's current or last run (" + r.status + ")"
		}
		out = append(out, DevVariable{Name: "counter", Flow: f.Name, Value: strconv.Itoa(n), Known: true, Source: src})
		th := DevVariable{Name: "threshold", Flow: f.Name, Source: "the flow's Counter threshold"}
		if f.CounterThreshold > 0 {
			th.Value, th.Known = strconv.Itoa(f.CounterThreshold), true
		} else {
			th.Source += ": none set"
		}
		out = append(out, th)
	}
	return out
}

// DevRun is a flow's current or last run as it concerns one player.
type DevRun struct {
	FlowID    int64     `json:"flowId"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Active    bool      `json:"active"`
	Anchor    time.Time `json:"anchor"`
	LastRun   time.Time `json:"lastRun"`
	Jobs      int       `json:"jobs"`
	Counter   int       `json:"counter"`
	Threshold int       `json:"threshold"`
	Counters  int       `json:"playersCounted"` // players with a counter in this run
	LogLines  []string  `json:"logLinesAboutPlayer"`
	ChatFrom  *int64    `json:"chatWindowFrom"`
	ChatTo    *int64    `json:"chatWindowTo"`
}

// DevRuns is every flow's current or last run (memory only), with the
// player's counter and the log lines that name them.
func DevRuns(guid string) []DevRun {
	g := strings.ToLower(guid)
	name := ""
	if p, ok := game.PlayerByGUID(g); ok {
		name = battleye.BareName(p.Name)
	} else if database != nil {
		if p, err := database.PlayerByGUID(g); err == nil {
			name = battleye.BareName(p.Name)
		}
	}
	flowRuns.Lock()
	list := make([]*flowRun, 0, len(flowRuns.byFlow))
	out := make([]DevRun, 0, len(flowRuns.byFlow))
	for _, r := range flowRuns.byFlow {
		list = append(list, r)
		d := DevRun{FlowID: r.flowID, Name: r.name, Status: r.status, Active: r.active, Anchor: r.anchor,
			LastRun: r.lastRun, Jobs: r.jobs, Threshold: r.counterThreshold, LogLines: []string{}}
		if r.chatWindow.set {
			from, to := r.chatWindow.from, r.chatWindow.to
			d.ChatFrom, d.ChatTo = &from, &to
		}
		for _, l := range r.log {
			if name != "" && strings.Contains(l, name) || strings.Contains(strings.ToLower(l), g) {
				d.LogLines = append(d.LogLines, l)
			}
		}
		out = append(out, d)
	}
	flowRuns.Unlock()
	for i, r := range list {
		r.counterMu.Lock()
		out[i].Counter = r.counters[g]
		out[i].Counters = len(r.counters)
		r.counterMu.Unlock()
	}
	sort.Slice(out, func(a, b int) bool { return out[a].FlowID < out[b].FlowID })
	return out
}
