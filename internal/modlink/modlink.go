// Package modlink is ReCON's side of recon-mod, the DayZ server-side mod
// (github.com/MowMdown/ReCON-Mod). The mod posts every online player's
// state to /api/mod/sync every second or so (Sync), with the deaths since
// the last sync and how the last commands went; the reply carries the
// commands queued here since (Queue). The mod proves it is the game server
// with the RCON password, which ReCON already has.
//
// Players come by SteamID64; their BattlEye GUID (ReCON's key for a
// player) is worked out from it.
package modlink

import (
	"crypto/subtle"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
)

// Protocol is the sync format version this ReCON speaks.
const Protocol = 1

var (
	// staleAfter is how long after the last sync the mod counts as
	// disconnected and its data as old.
	staleAfter = 5 * time.Second
	// commandTimeout is how long a queued command waits for the mod to
	// pick it up and answer before it is given up.
	commandTimeout = 30 * time.Second
	// keepResults is how many finished commands are kept for the pages.
	keepResults = 50
)

// SyncRequest is what the mod posts (ReCON_Sync in the mod).
type SyncRequest struct {
	V       int           `json:"v"`
	Mod     string        `json:"mod"`
	Key     string        `json:"key"`
	World   string        `json:"world"`
	Year    int           `json:"year"`
	Month   int           `json:"month"`
	Day     int           `json:"day"`
	Hour    int           `json:"hour"`
	Minute  int           `json:"minute"`
	Players []PlayerState `json:"players"`
	Events  []Event       `json:"events"`
	Results []Result      `json:"results"`
}

// PlayerState is one player as the mod reads them (ReCON_PlayerState).
type PlayerState struct {
	SteamID     string  `json:"steamId"`
	Name        string  `json:"name"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Z           float64 `json:"z"`
	Heading     float64 `json:"heading"`
	Alive       bool    `json:"alive"`
	Health      float64 `json:"health"`
	Blood       float64 `json:"blood"`
	Shock       float64 `json:"shock"`
	Water       float64 `json:"water"`
	Energy      float64 `json:"energy"`
	Heat        float64 `json:"heat"`
	Wet         int     `json:"wet"`
	Stamina     float64 `json:"stamina"`
	Bleeding    int     `json:"bleeding"`
	Agents      int     `json:"agents"`
	BrokenLegs  int     `json:"brokenLegs"`
	Unconscious bool    `json:"unconscious"`
	Restrained  bool    `json:"restrained"`
	Hands       string  `json:"hands"`
	Vehicle     string  `json:"vehicle"`
	Kills       int     `json:"kills"`
	Deaths      int     `json:"deaths"`
}

// Event is something that happened between syncs (ReCON_Event).
type Event struct {
	Type       string  `json:"type"`
	Victim     string  `json:"victim"`
	VictimName string  `json:"victimName"`
	Killer     string  `json:"killer"`
	KillerName string  `json:"killerName"`
	Weapon     string  `json:"weapon"`
	Distance   float64 `json:"distance"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Z          float64 `json:"z"`
	At         int     `json:"at"`
}

// Result is how a command went (ReCON_Result).
type Result struct {
	ID    int    `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Command is an action for the mod to run (ReCON_Command). Every field is
// always sent: the mod's JSON reader fills its class from them.
type Command struct {
	ID      int     `json:"id"`
	Action  string  `json:"action"`
	SteamID string  `json:"steamId"`
	Target  string  `json:"target"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Z       float64 `json:"z"`
	Stat    string  `json:"stat"`
	Value   float64 `json:"value"`
	Text    string  `json:"text"`
	Title   string  `json:"title"`
	Seconds float64 `json:"seconds"`
}

// SyncReply is ReCON's answer (ReCON_Reply).
type SyncReply struct {
	OK       bool      `json:"ok"`
	Error    string    `json:"error"`
	Interval int       `json:"interval"`
	Commands []Command `json:"commands"`
}

// Player is a player's live state as the pages get it.
type Player struct {
	GUID string `json:"guid"`
	PlayerState
	Illnesses []string `json:"illnesses"`
}

// CommandStatus is a queued command and what became of it.
type CommandStatus struct {
	Command
	GUID     string    `json:"guid"`
	Status   string    `json:"status"` // queued, sent, done, failed, expired
	Error    string    `json:"error,omitempty"`
	Queued   time.Time `json:"queued"`
	Finished time.Time `json:"finished,omitempty"`
}

var state struct {
	sync.Mutex
	lastSync time.Time
	mod      string
	world    string
	date     [5]int            // year, month, day, hour, minute
	players  map[string]Player // by GUID
	nextID   int
	commands []*CommandStatus // oldest first: queued and sent ones, then the last keepResults finished
	// SteamIDs saved to the players table this run (one write per player).
	steamSaved map[string]bool
	refused    string // the last refusal logged, so it isn't logged every second
}

var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }

// keySource is the RCON password the mod must send.
var keySource = func() string { return "" }

// SetKeySource sets where the RCON password comes from (read on every
// sync, so RCON Settings changing it applies at once).
func SetKeySource(f func() string) { keySource = f }

// ErrRefused is a sync with the wrong password.
var ErrRefused = errors.New("wrong RCON password: set the mod's \"password\" to the RCON password ReCON uses")

// Sync takes in one sync from the mod and returns the commands to run.
func Sync(req SyncRequest, now time.Time) (SyncReply, error) {
	want := keySource()
	if want == "" || subtle.ConstantTimeCompare([]byte(req.Key), []byte(want)) != 1 {
		logRefusal(ErrRefused.Error())
		return SyncReply{Error: ErrRefused.Error()}, ErrRefused
	}
	if req.V != Protocol {
		msg := "the mod speaks protocol " + strconv.Itoa(req.V) + ", this ReCON " + strconv.Itoa(Protocol) + ": update both to the same release"
		logRefusal(msg)
		return SyncReply{Error: msg}, errors.New(msg)
	}
	players := make(map[string]Player, len(req.Players))
	var newSteam [][2]string
	state.Lock()
	if state.lastSync.IsZero() || now.Sub(state.lastSync) > staleAfter {
		log.Printf("modlink: recon-mod %s connected (%s, %d players)", req.Mod, req.World, len(req.Players))
	}
	state.refused = ""
	if state.steamSaved == nil {
		state.steamSaved = map[string]bool{}
	}
	for _, p := range req.Players {
		id, err := strconv.ParseUint(p.SteamID, 10, 64)
		if err != nil {
			continue
		}
		guid := steam.BattlEyeGUID(id)
		players[guid] = Player{GUID: guid, PlayerState: p, Illnesses: Illnesses(p.Agents)}
		if !state.steamSaved[guid] {
			state.steamSaved[guid] = true
			newSteam = append(newSteam, [2]string{guid, p.SteamID})
		}
	}
	state.lastSync, state.mod, state.world, state.players = now, req.Mod, req.World, players
	state.date = [5]int{req.Year, req.Month, req.Day, req.Hour, req.Minute}

	// Results of what was sent.
	byID := map[int]*CommandStatus{}
	for _, c := range state.commands {
		byID[c.ID] = c
	}
	for _, r := range req.Results {
		c := byID[r.ID]
		if c == nil || (c.Status != "sent" && c.Status != "expired") {
			continue
		}
		c.Status, c.Error, c.Finished = "done", "", now
		if !r.OK {
			c.Status, c.Error = "failed", r.Error
		}
	}
	// What to send: everything queued, unless it waited too long.
	reply := SyncReply{OK: true, Commands: []Command{}}
	for _, c := range state.commands {
		switch {
		case c.Status == "queued" && now.Sub(c.Queued) > commandTimeout:
			c.Status, c.Error, c.Finished = "expired", "the mod didn't pick it up in time", now
		case c.Status == "queued":
			c.Status = "sent"
			reply.Commands = append(reply.Commands, c.Command)
		case c.Status == "sent" && now.Sub(c.Queued) > commandTimeout:
			c.Status, c.Error, c.Finished = "expired", "no answer from the mod", now
		}
	}
	trimCommands()
	state.Unlock()

	if database != nil {
		for _, g := range newSteam {
			if err := database.SetSteamID(g[0], g[1]); err != nil {
				log.Printf("modlink: saving %s's SteamID: %v", g[0], err)
			}
		}
		for _, e := range req.Events {
			if e.Type != "kill" {
				continue
			}
			k := store.Kill{At: now, VictimName: e.VictimName, KillerName: e.KillerName,
				Weapon: e.Weapon, Distance: e.Distance, X: e.X, Z: e.Z}
			k.VictimGUID = guidOf(e.Victim)
			k.KillerGUID = guidOf(e.Killer)
			if err := database.AddKill(k); err != nil {
				log.Printf("modlink: saving a kill: %v", err)
			}
		}
	}
	return reply, nil
}

func logRefusal(msg string) {
	state.Lock()
	defer state.Unlock()
	if state.refused != msg {
		state.refused = msg
		log.Printf("modlink: refused a sync from recon-mod: %s", msg)
	}
}

// guidOf is the GUID of a SteamID64 ("" for none).
func guidOf(steamID string) string {
	id, err := strconv.ParseUint(steamID, 10, 64)
	if err != nil || id == 0 {
		return ""
	}
	return steam.BattlEyeGUID(id)
}

// trimCommands keeps the open commands and the last keepResults finished.
func trimCommands() {
	finished := 0
	for _, c := range state.commands {
		if c.Status != "queued" && c.Status != "sent" {
			finished++
		}
	}
	if finished <= keepResults {
		return
	}
	drop := finished - keepResults
	kept := state.commands[:0]
	for _, c := range state.commands {
		if drop > 0 && c.Status != "queued" && c.Status != "sent" {
			drop--
			continue
		}
		kept = append(kept, c)
	}
	state.commands = kept
}

// illnessNames are eAgents' bits.
var illnessNames = []string{"cholera", "influenza", "salmonella", "brain disease", "food poisoning",
	"chemical poisoning", "wound infection", "nerve agent", "heavy metal poisoning", "silicosis", "eye irritation"}

// Illnesses names the eAgents bits in agents.
func Illnesses(agents int) []string {
	out := []string{}
	for i, n := range illnessNames {
		if agents&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// Status is the mod's connection, as the pages show it.
type Status struct {
	Connected bool      `json:"connected"`
	LastSync  time.Time `json:"lastSync,omitempty"`
	Mod       string    `json:"mod,omitempty"`
	World     string    `json:"world,omitempty"`
	Date      [5]int    `json:"date"` // in-game year, month, day, hour, minute
}

// Current is the mod's connection and every online player's state (none
// once the mod stopped syncing), sorted by name.
func Current(now time.Time) (Status, []Player) {
	state.Lock()
	defer state.Unlock()
	st := Status{LastSync: state.lastSync, Mod: state.mod, World: state.world, Date: state.date}
	st.Connected = !state.lastSync.IsZero() && now.Sub(state.lastSync) <= staleAfter
	out := []Player{}
	if st.Connected {
		for _, p := range state.players {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return st, out
}

// PlayerOf is one online player's live state.
func PlayerOf(guid string, now time.Time) (Player, bool) {
	state.Lock()
	defer state.Unlock()
	if state.lastSync.IsZero() || now.Sub(state.lastSync) > staleAfter {
		return Player{}, false
	}
	p, ok := state.players[strings.ToLower(guid)]
	return p, ok
}

// Commands is the open and recent commands, newest first; for one player
// when guid isn't "".
func Commands(guid string) []CommandStatus {
	state.Lock()
	defer state.Unlock()
	out := []CommandStatus{}
	for i := len(state.commands) - 1; i >= 0; i-- {
		c := state.commands[i]
		if guid == "" || c.GUID == strings.ToLower(guid) {
			out = append(out, *c)
		}
	}
	return out
}

// Actions are the commands the mod runs, and the stats "set" can change
// with their largest values.
var (
	Actions  = map[string]bool{"heal": true, "kill": true, "teleport": true, "teleportTo": true, "set": true, "message": true}
	StatMaxs = map[string]float64{"health": 100, "blood": 5000, "shock": 100, "water": 5000, "energy": 20000}
)

// Queue adds a command for an online player (guid); c.SteamID and, for
// teleportTo, c.Target (a GUID here) are filled from the live state.
// Errors are user-facing.
func Queue(guid string, c Command, now time.Time) (CommandStatus, error) {
	if !Actions[c.Action] {
		return CommandStatus{}, errors.New("unknown action")
	}
	switch c.Action {
	case "set":
		max, ok := StatMaxs[c.Stat]
		if !ok {
			return CommandStatus{}, errors.New("stat is health, blood, shock, water or energy")
		}
		if c.Value < 0 || c.Value > max {
			return CommandStatus{}, errors.New(c.Stat + " is from 0 to " + strconv.FormatFloat(max, 'f', -1, 64))
		}
	case "teleport":
		if c.X <= 0 || c.Z <= 0 {
			return CommandStatus{}, errors.New("pick where to teleport them")
		}
	case "message":
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			return CommandStatus{}, errors.New("type a message")
		}
		if len(c.Text) > 500 {
			return CommandStatus{}, errors.New("the message is longer than 500 characters")
		}
	}
	guid = strings.ToLower(guid)
	state.Lock()
	defer state.Unlock()
	if state.lastSync.IsZero() || now.Sub(state.lastSync) > staleAfter {
		return CommandStatus{}, errors.New("recon-mod isn't connected")
	}
	p, ok := state.players[guid]
	if !ok {
		return CommandStatus{}, errors.New("the player isn't online")
	}
	c.SteamID = p.SteamID
	if c.Action == "teleportTo" {
		t, ok := state.players[strings.ToLower(c.Target)]
		if !ok {
			return CommandStatus{}, errors.New("the player to go to isn't online")
		}
		c.Target = t.SteamID
	}
	state.nextID++
	c.ID = state.nextID
	cs := &CommandStatus{Command: c, GUID: guid, Status: "queued", Queued: now}
	state.commands = append(state.commands, cs)
	return *cs, nil
}

// reset forgets everything (tests).
func reset() {
	state.Lock()
	defer state.Unlock()
	state.lastSync, state.mod, state.world, state.players = time.Time{}, "", "", nil
	state.nextID, state.commands, state.steamSaved, state.refused = 0, nil, nil, ""
}
