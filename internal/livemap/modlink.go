// Package livemap is ReCON's side of recon-mod, the DayZ server-side mod
// (github.com/MowMdown/ReCON-Mod): it adds the Live Map page and the API
// the mod talks to (see Start). The mod posts every online player's
// state to /api/mod/sync every second or so (Sync), with the deaths since
// the last sync and how the last commands went; the reply carries the
// commands queued here since (Queue). The mod proves it is the game server
// with the RCON password, which ReCON already has.
//
// Players come by SteamID64; their BattlEye GUID (ReCON's key for a
// player) is worked out from it.
package livemap

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Protocol is the sync format version this ReCON speaks.
const Protocol = 1

var (
	// staleAfter is how long after the last sync the mod counts as
	// disconnected and its data as old.
	staleAfter = 5 * time.Second
	// watchFor is how long a player stays watched (their inventory sent)
	// after a page last asked for them.
	watchFor = 15 * time.Second
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
	Weather World         `json:"weather"`
	Players []PlayerState `json:"players"`
	Events  []Event       `json:"events"`
	Results []Result      `json:"results"`
	// The items the server can spawn: Catalog is entries CatalogAt onwards
	// of CatalogTotal, sent while the reply's CatalogNext asks for them.
	CatalogTotal int           `json:"catalogTotal"`
	CatalogAt    int           `json:"catalogAt"`
	Catalog      []CatalogItem `json:"catalog"`
	// Every vehicle on the server (mod 0.0.1i on), in the syncs that have
	// them (every few seconds: VehiclesSent).
	VehiclesSent Flag      `json:"vehiclesSent"`
	Vehicles     []Vehicle `json:"vehicles"`
	// The vehicles the reply's WatchVehicles asked to see inside.
	VehicleDetails []VehicleDetail `json:"vehicleDetails"`
	// The map's dynamic events, sent with the vehicles.
	WorldEvents []WorldEvent `json:"worldEvents"`
	// The bears, wolves and guard dogs alive on the server, every sync
	// (mod 0.0.1l on: PredatorsSent).
	PredatorsSent Flag       `json:"predatorsSent"`
	Predators     []Predator `json:"predators"`
	// The map's place names, when the reply asked for them (WantPlaces).
	PlacesSent Flag    `json:"placesSent"`
	Places     []Place `json:"places"`
}

// WorldEvent is one of the map's dynamic events (ReCON_WorldEvent).
type WorldEvent struct {
	ID     int     `json:"id"`
	Type   string  `json:"type"`
	Kind   string  `json:"kind"` // crash, gas or area
	X      float64 `json:"x"`
	Z      float64 `json:"z"`
	Radius float64 `json:"radius"` // an area's, in metres
}

// Predator is a bear, wolf or guard dog (ReCON_Predator).
type Predator struct {
	ID      int     `json:"id"`
	Type    string  `json:"type"`
	Kind    string  `json:"kind"` // bear, wolf or dog
	X       float64 `json:"x"`
	Z       float64 `json:"z"`
	Heading float64 `json:"heading"` // degrees, 0 = north, clockwise
}

// Place is one of the map's place names (ReCON_Place).
type Place struct {
	Name string  `json:"name"`
	Type string  `json:"type"` // Capital, City, Village, Local, Marine, Hill, ViewPoint...
	X    float64 `json:"x"`
	Z    float64 `json:"z"`
}

// VehicleDetail is what is in a vehicle (ReCON_VehicleDetail): its damage
// zones' health, and its parts (attachments) and cargo.
type VehicleDetail struct {
	ID    int       `json:"id"`
	Zones []Zone    `json:"zones"`
	Items []Item    `json:"items"`
	At    time.Time `json:"at"` // when it came in (ReCON's)
}

// Zone is one of a vehicle's damage zones.
type Zone struct {
	Name   string  `json:"name"`
	Health float64 `json:"health"` // 0-1
}

// Vehicle is a car, motorbike or boat on the server (ReCON_Vehicle).
type Vehicle struct {
	ID      int     `json:"id"` // its object id, for deleteVehicle
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	X       float64 `json:"x"`
	Z       float64 `json:"z"`
	Heading float64 `json:"heading"`
	Health  float64 `json:"health"` // 0-1
	Fuel    float64 `json:"fuel"`   // 0-1, -1 when it isn't a car
	Crew    int     `json:"crew"`
	Engine  Flag    `json:"engine"`
}

// CatalogItem is an item the server can spawn (ReCON_CatalogItem).
type CatalogItem struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Kind string `json:"kind"` // weapon, magazine, ammo, clothing, vehicle, animal or item
}

// PlayerState is one player as the mod reads them (ReCON_PlayerState).
type PlayerState struct {
	SteamID string  `json:"steamId"`
	Name    string  `json:"name"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Z       float64 `json:"z"`
	Heading float64 `json:"heading"`
	Alive   Flag    `json:"alive"`
	Health  float64 `json:"health"`
	Blood   float64 `json:"blood"`
	Shock   float64 `json:"shock"`
	Water   float64 `json:"water"`
	Energy  float64 `json:"energy"`
	Heat    float64 `json:"heat"`
	// The heat buff, shown as "comfort": 0 (none) to HeatBufferMax.
	HeatBuffer      float64 `json:"heatBuffer"`
	HeatBufferMax   float64 `json:"heatBufferMax"`
	HeatBufferCap   float64 `json:"heatBufferCap"`   // the share their clothes allow, 0.35 to 1
	HeatBufferStage int     `json:"heatBufferStage"` // the HUD's buff icon: 0 none, 1 to 3
	Wet             int     `json:"wet"`
	Stamina         float64 `json:"stamina"`
	StaminaMax      float64 `json:"staminaMax"` // DayZ's maximum (100 by default)
	StaminaCap      float64 `json:"staminaCap"` // the most they can have now: carried weight lowers it
	Bleeding        int     `json:"bleeding"`
	Agents          int     `json:"agents"`
	BrokenLegs      int     `json:"brokenLegs"`
	Unconscious     Flag    `json:"unconscious"`
	Restrained      Flag    `json:"restrained"`
	Hands           string  `json:"hands"`
	Vehicle         string  `json:"vehicle"`
	HandsName       string  `json:"handsName"` // as the game shows them ("Wood Axe")
	VehicleName     string  `json:"vehicleName"`
	Kills           int     `json:"kills"`
	Deaths          int     `json:"deaths"`
	// Inv: Inventory was read (only for watched players). Kept apart from
	// the live state (see InventoryOf), so the players list stays small.
	Inv       Flag   `json:"inv"`
	Inventory []Item `json:"inventory,omitempty"`
}

// Item is one item a player carries, with its attachments and cargo
// (ReCON_Item).
type Item struct {
	ID          int     `json:"id"`
	Type        string  `json:"type"`
	Name        string  `json:"name"`
	Slot        string  `json:"slot"` // "" in cargo; "Hands" for the item in hand
	Row         int     `json:"row"`  // in cargo; -1 when attached
	Col         int     `json:"col"`
	Quantity    float64 `json:"quantity"`
	QuantityMax float64 `json:"quantityMax"` // 0: it has no quantity
	Unit        string  `json:"unit"`        // "rounds", "ml" or ""
	Health      float64 `json:"health"`
	HealthMax   float64 `json:"healthMax"`
	HealthLevel int     `json:"healthLevel"` // 0 pristine .. 4 ruined
	Wet         float64 `json:"wet"`
	Temperature float64 `json:"temperature"`
	Attachments []Item  `json:"attachments"`
	Cargo       []Item  `json:"cargo"`
}

// Inventory is what a player carried at the last sync that read it.
type Inventory struct {
	Items []Item    `json:"items"`
	At    time.Time `json:"at"`
}

// World is the weather now and where each part is heading (ReCON_World).
type World struct {
	Overcast        float64 `json:"overcast"` // 0-1
	Rain            float64 `json:"rain"`
	Fog             float64 `json:"fog"`
	Snow            float64 `json:"snow"`
	WindSpeed       float64 `json:"windSpeed"` // m/s
	WindDir         float64 `json:"windDir"`   // degrees
	OvercastTarget  float64 `json:"overcastTarget"`
	RainTarget      float64 `json:"rainTarget"`
	FogTarget       float64 `json:"fogTarget"`
	SnowTarget      float64 `json:"snowTarget"`
	WindSpeedTarget float64 `json:"windSpeedTarget"`
	WindDirTarget   float64 `json:"windDirTarget"`
	Mode            string  `json:"mode"` // world, forecast or static
	TimeFrozen      Flag    `json:"timeFrozen"`
	// Today's sunrise and sunset on the map, in hours (0: not sent).
	Sunrise float64 `json:"sunrise"`
	Sunset  float64 `json:"sunset"`
}

// Event is something that happened between syncs (ReCON_Event).
type Event struct {
	Type       string  `json:"type"`
	Victim     string  `json:"victim"`
	VictimName string  `json:"victimName"`
	Killer     string  `json:"killer"`
	KillerName string  `json:"killerName"`
	Weapon     string  `json:"weapon"`
	Cause      string  `json:"cause"`  // what killed them (from mod 0.0.1h; see Kill.Cause)
	Agents     int     `json:"agents"` // the illnesses they had (eAgents bits)
	Distance   float64 `json:"distance"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Z          float64 `json:"z"`
	At         int     `json:"at"`
}

// Result is how a command went (ReCON_Result).
type Result struct {
	ID    int    `json:"id"`
	OK    Flag   `json:"ok"`
	Error string `json:"error"`
	// A vehicle spawn's new vehicle, by persistent ID (b1:b2:b3:b4), from
	// mod 0.0.1m; "" otherwise.
	Entity string `json:"entity"`
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
	// spawn: the vehicle to delete once the new one exists (persistent
	// ID), for a chat car (chatcars.go); mod 0.0.1m on.
	Replace string `json:"replace,omitempty"`
	// ...and its class: the mod deletes it only if both match, so an ID
	// the game ever gave to another vehicle can't take that one with it.
	ReplaceType string  `json:"replaceType,omitempty"`
	Title       string  `json:"title"`
	Seconds     float64 `json:"seconds"`
	// World actions (QueueWorld).
	Transition float64 `json:"transition"`
	Hold       float64 `json:"hold"`
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	Year       int     `json:"year"`
	Month      int     `json:"month"`
	Day        int     `json:"day"`
	Hour       int     `json:"hour"`
	Minute     int     `json:"minute"`
}

// SyncReply is ReCON's answer (ReCON_Reply).
type SyncReply struct {
	OK       bool      `json:"ok"`
	Error    string    `json:"error"`
	Interval int       `json:"interval"`
	Commands []Command `json:"commands"`
	Watch    []string  `json:"watch"` // SteamID64s whose inventory to send
	// Vehicle ids whose parts and cargo to send (left out when none, for a
	// mod before 0.0.1i).
	WatchVehicles []int `json:"watchVehicles,omitempty"`
	// WantPlaces asks for the map's place names (once a connection; left
	// out when false, for a mod before 0.0.1i).
	WantPlaces bool `json:"wantPlaces,omitempty"`
	// CatalogNext is 1 + the first catalog entry wanted next (0: none).
	CatalogNext int `json:"catalogNext"`
}

// Player is a player's live state as the pages get it.
type Player struct {
	GUID string `json:"guid"`
	PlayerState
	Illnesses []string `json:"illnesses"`
}

// Position is a spot in the world (y is the height).
type Position struct {
	X  float64   `json:"x"`
	Y  float64   `json:"y"`
	Z  float64   `json:"z"`
	At time.Time `json:"at"` // when they were teleported away from it
}

// CommandStatus is a queued command and what became of it.
type CommandStatus struct {
	Command
	GUID string `json:"guid"`
	// Teleports: where the player was when it was queued (it becomes their
	// previous spot once the mod has run it), and whether it was "back".
	From     *Position `json:"from,omitempty"`
	Back     bool      `json:"back,omitempty"`
	ChatCar  bool      `json:"chatCar,omitempty"` // a vehicle chat command's spawn (chatcars.go)
	Status   string    `json:"status"`            // queued, sent, done, failed, expired
	Error    string    `json:"error,omitempty"`
	Queued   time.Time `json:"queued"`
	Finished time.Time `json:"finished,omitempty"`
}

var state struct {
	sync.Mutex
	lastSync time.Time
	mod      string
	world    string
	date     [5]int // year, month, day, hour, minute
	weather  World
	players  map[string]Player // by GUID
	// Watched players (by GUID: until when), and their last inventories.
	watch     map[string]time.Time
	inventory map[string]Inventory
	// Where each player was before their last teleport (by GUID), for back.
	previous map[string]Position
	// The items the server can spawn, as far as they have come in, and how
	// many there are; catalogTypes is filled once all are in.
	catalog      []CatalogItem
	catalogTotal int
	catalogTypes map[string]CatalogItem
	// The vehicles as last sent, and when (zero: never, so a mod before 0.0.1i).
	vehicles   []Vehicle
	vehiclesAt time.Time
	// Vehicles asked about (by id: until when to keep asking), and what is
	// in them. An ask is answered once: the mod sends it in one sync.
	vehWatch  map[int]time.Time
	vehDetail map[int]VehicleDetail
	// The map's dynamic events (with the vehicles), and its place names
	// (placesFor: the world they are of; asked for once a connection).
	worldEvents []WorldEvent
	// The map whose vehicles.json was looked for this connection.
	vehFileFor string
	// The map the mod last reported, kept in the database (mapstate.go), so
	// the Live Map can show it while the mod isn't connected.
	lastWorld string
	// The predators as last sent (nil: never, so a mod before 0.0.1l).
	predators []Predator
	places    []Place
	placesFor string
	nextID    int
	commands  []*CommandStatus // oldest first: queued and sent ones, then the last keepResults finished
	// SteamIDs saved to the players table this run (one write per player).
	steamSaved map[string]bool
	refused    string // the last refusal logged, so it isn't logged every second
}

// keySource is the RCON password the mod must send (read on every sync,
// so RCON Settings changing it applies at once).
var keySource = func() string { return "" }

// saveSteamID stores a player's SteamID64 in ReCON (nil: not stored).
var saveSteamID func(guid, steamID string) error

// ErrRefused is a sync with the wrong password.
var ErrRefused = errors.New("wrong RCON password: set the mod's \"password\" to the RCON password ReCON uses")

// Sync takes in one sync from the mod and returns the commands to run.
func Sync(req SyncRequest, now time.Time) (SyncReply, error) {
	want := keySource()
	if want == "" || subtle.ConstantTimeCompare([]byte(req.Key), []byte(want)) != 1 {
		// The lengths (never the passwords) tell a typo from a different password.
		logRefusalKeyed(ErrRefused.Error(), fmt.Sprintf("%s (the mod sent a password of %d characters; ReCON's has %d)",
			ErrRefused.Error(), len(req.Key), len(want)))
		return SyncReply{Error: ErrRefused.Error()}, ErrRefused
	}
	if req.V != Protocol {
		msg := "the mod speaks protocol " + strconv.Itoa(req.V) + ", this ReCON " + strconv.Itoa(Protocol) + ": update both to the same release"
		logRefusal(msg)
		return SyncReply{Error: msg}, errors.New(msg)
	}
	players := make(map[string]Player, len(req.Players))
	var newSteam [][2]string
	var chatCars []chatCarResult // chat cars the results reported, kept after the lock
	untracked := false
	saveWorld := ""
	state.Lock()
	if w := strings.ToLower(req.World); w != "" && w != state.lastWorld {
		state.lastWorld, saveWorld = w, w
	}
	if state.lastSync.IsZero() || now.Sub(state.lastSync) > staleAfter {
		log.Printf("recon-mod: the mod (version %s) connected (%s, %d players)", req.Mod, req.World, len(req.Players))
		// The server may have restarted with other mods: fetch the item list again.
		state.catalog, state.catalogTotal, state.catalogTypes = nil, 0, nil
		state.places, state.placesFor = nil, ""
		state.predators, state.vehFileFor = nil, ""
	}
	takeCatalog(req)
	if req.VehiclesSent {
		state.vehicles, state.vehiclesAt = req.Vehicles, now
		if state.vehicles == nil {
			state.vehicles = []Vehicle{}
		}
		state.worldEvents = req.WorldEvents
		if state.worldEvents == nil {
			state.worldEvents = []WorldEvent{}
		}
	}
	if req.PredatorsSent {
		state.predators = req.Predators
		if state.predators == nil {
			state.predators = []Predator{}
		}
	}
	if req.PlacesSent {
		state.places, state.placesFor = req.Places, strings.ToLower(req.World)
		if state.places == nil {
			state.places = []Place{}
		}
	}
	for _, d := range req.VehicleDetails {
		if state.vehDetail == nil {
			state.vehDetail = map[int]VehicleDetail{}
		}
		if d.Zones == nil {
			d.Zones = []Zone{}
		}
		if d.Items == nil {
			d.Items = []Item{}
		}
		d.At = now
		state.vehDetail[d.ID] = d
		delete(state.vehWatch, d.ID) // answered: don't ask again
	}
	for id, d := range state.vehDetail {
		if now.Sub(d.At) > 5*time.Minute {
			delete(state.vehDetail, id)
		}
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
		guid := battlEyeGUID(id)
		if p.Inv {
			if state.inventory == nil {
				state.inventory = map[string]Inventory{}
			}
			items := p.Inventory
			if items == nil {
				items = []Item{}
			}
			state.inventory[guid] = Inventory{Items: items, At: now}
		}
		p.Inv, p.Inventory = false, nil
		players[guid] = Player{GUID: guid, PlayerState: p, Illnesses: Illnesses(p.Agents)}
		if !state.steamSaved[guid] {
			state.steamSaved[guid] = true
			newSteam = append(newSteam, [2]string{guid, p.SteamID})
		}
	}
	state.lastSync, state.mod, state.world, state.players = now, req.Mod, req.World, players
	state.date = [5]int{req.Year, req.Month, req.Day, req.Hour, req.Minute}
	state.weather = req.Weather

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
		if bool(r.OK) && c.ChatCar {
			if r.Entity != "" {
				chatCars = append(chatCars, chatCarResult{c.GUID, r.Entity, c.Text})
			} else {
				untracked = true
			}
		}
		if !r.OK {
			c.Status, c.Error = "failed", r.Error
		} else if c.From != nil {
			if state.previous == nil {
				state.previous = map[string]Position{}
			}
			from := *c.From
			from.At = now
			state.previous[c.GUID] = from
		}
	}
	// What to send: everything queued, unless it waited too long.
	reply := SyncReply{OK: true, Commands: []Command{}, Watch: []string{}}
	if len(state.catalog) < state.catalogTotal {
		reply.CatalogNext = len(state.catalog) + 1
	}
	for id, until := range state.vehWatch {
		if now.After(until) {
			delete(state.vehWatch, id)
			continue
		}
		reply.WatchVehicles = append(reply.WatchVehicles, id)
	}
	sort.Ints(reply.WatchVehicles)
	reply.WantPlaces = state.placesFor != strings.ToLower(req.World)
	for g, until := range state.watch {
		if now.After(until) {
			delete(state.watch, g)
			continue
		}
		if p, ok := players[g]; ok {
			reply.Watch = append(reply.Watch, p.SteamID)
		}
	}
	sort.Strings(reply.Watch)
	for g := range state.inventory {
		if _, ok := players[g]; !ok {
			delete(state.inventory, g) // gone: their next inventory starts fresh
		}
	}
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

	keepChatCars(chatCars, untracked, now)
	if saveWorld != "" {
		saveLastWorld(saveWorld)
	}
	if saveSteamID != nil {
		for _, g := range newSteam {
			if err := saveSteamID(g[0], g[1]); err != nil {
				log.Printf("recon-mod: saving %s's SteamID: %v", g[0], err)
			}
		}
	}
	if db != nil {
		for _, e := range req.Events {
			if e.Type != "kill" {
				continue
			}
			k := Kill{At: now, VictimName: e.VictimName, KillerName: e.KillerName,
				Weapon: e.Weapon, Cause: e.Cause, Agents: e.Agents, Distance: e.Distance, X: e.X, Z: e.Z}
			k.VictimGUID = guidOf(e.Victim)
			k.KillerGUID = guidOf(e.Killer)
			if err := addKill(k); err != nil {
				log.Printf("recon-mod: saving a kill: %v", err)
			}
		}
	}
	return reply, nil
}

// takeCatalog adds the catalog entries a sync carries (state is locked).
func takeCatalog(req SyncRequest) {
	if req.CatalogTotal != state.catalogTotal {
		state.catalog, state.catalogTotal, state.catalogTypes = nil, req.CatalogTotal, nil
	}
	if req.CatalogAt != len(state.catalog) || len(req.Catalog) == 0 || state.catalogTypes != nil {
		return // not the entries asked for next (a resend, or out of step): asked again
	}
	for _, it := range req.Catalog {
		if len(state.catalog) >= state.catalogTotal {
			break
		}
		state.catalog = append(state.catalog, it)
	}
	if len(state.catalog) == state.catalogTotal {
		sort.Slice(state.catalog, func(a, b int) bool {
			na, nb := strings.ToLower(itemName(state.catalog[a])), strings.ToLower(itemName(state.catalog[b]))
			if na != nb {
				return na < nb
			}
			return state.catalog[a].Type < state.catalog[b].Type
		})
		state.catalogTypes = make(map[string]CatalogItem, len(state.catalog))
		for _, it := range state.catalog {
			state.catalogTypes[it.Type] = it
		}
		log.Printf("recon-mod: got the list of %d items the server can spawn", len(state.catalog))
	}
}

// itemName is an item's display name, or its class name when it has none
// (or only an untranslated string key).
func itemName(it CatalogItem) string {
	if it.Name == "" || strings.HasPrefix(it.Name, "$") {
		return it.Type
	}
	return it.Name
}

// Catalog is every item the server can spawn, sorted by name, once the mod
// has sent them all (complete false until then, or while it isn't connected).
func Catalog(now time.Time) (items []CatalogItem, complete bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.catalogTypes == nil {
		return []CatalogItem{}, false
	}
	return state.catalog, true
}

// Vehicles is every vehicle on the server as the mod last sent them, and
// when; none while the mod isn't connected (sent false: it hasn't sent
// any, so it may be a mod before 0.0.1i).
func Vehicles(now time.Time) (vs []Vehicle, at time.Time, sent bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.vehiclesAt.IsZero() {
		return []Vehicle{}, time.Time{}, false
	}
	return state.vehicles, state.vehiclesAt, true
}

// Predators is the bears, wolves and guard dogs as the mod last sent them
// (sent false: it hasn't, as a mod before 0.0.1l doesn't).
func Predators(now time.Time) (ps []Predator, sent bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.predators == nil {
		return []Predator{}, false
	}
	return state.predators, true
}

// WorldEvents is the map's dynamic events as the mod last sent them (sent
// false: it hasn't, as a mod before 0.0.1i doesn't).
func WorldEvents(now time.Time) (es []WorldEvent, sent bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.worldEvents == nil {
		return []WorldEvent{}, false
	}
	return state.worldEvents, true
}

// Places is the map's place names, and the world they are of (sent false:
// the mod hasn't sent them, as one before 0.0.1i doesn't).
func Places(now time.Time) (ps []Place, world string, sent bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.places == nil {
		return []Place{}, "", false
	}
	return state.places, state.placesFor, true
}

// logRefusal logs why a sync was refused, once until the reason (key)
// changes or a sync is accepted.
func logRefusal(msg string) { logRefusalKeyed(msg, msg) }

func logRefusalKeyed(key, msg string) {
	state.Lock()
	defer state.Unlock()
	if state.refused != key {
		state.refused = key
		log.Printf("recon-mod: refused a sync from the mod: %s", msg)
	}
}

// guidOf is the GUID of a SteamID64 ("" for none).
func guidOf(steamID string) string {
	id, err := strconv.ParseUint(steamID, 10, 64)
	if err != nil || id == 0 {
		return ""
	}
	return battlEyeGUID(id)
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
	Weather   World     `json:"weather"`
}

// Current is the mod's connection and every online player's state (none
// once the mod stopped syncing), sorted by name.
func Current(now time.Time) (Status, []Player) {
	state.Lock()
	defer state.Unlock()
	st := Status{LastSync: state.lastSync, Mod: state.mod, World: state.world, Date: state.date, Weather: state.weather}
	if st.World == "" {
		st.World = state.lastWorld // the mod isn't connected (or not since a restart): its last map
	}
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
	if !connected(now) {
		return Player{}, false
	}
	p, ok := state.players[strings.ToLower(guid)]
	return p, ok
}

// Watch asks the mod for a player's inventory for the next watchFor.
func Watch(guid string, now time.Time) {
	state.Lock()
	defer state.Unlock()
	if state.watch == nil {
		state.watch = map[string]time.Time{}
	}
	state.watch[strings.ToLower(guid)] = now.Add(watchFor)
}

// AskVehicle asks the mod, once, for what is in a vehicle now: it is sent
// in the next sync (asked until it is, for up to watchFor). What came in
// before is forgotten, so VehicleOf has nothing until then.
func AskVehicle(id int, now time.Time) {
	state.Lock()
	defer state.Unlock()
	if state.vehWatch == nil {
		state.vehWatch = map[int]time.Time{}
	}
	state.vehWatch[id] = now.Add(watchFor)
	delete(state.vehDetail, id)
}

// VehicleOf is a vehicle as last sent, and what is in it if the mod has
// sent that (false: no such vehicle, or the mod isn't connected).
func VehicleOf(id int, now time.Time) (v Vehicle, d VehicleDetail, haveDetail, ok bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) {
		return
	}
	for _, x := range state.vehicles {
		if x.ID == id {
			v, ok = x, true
			break
		}
	}
	d, haveDetail = state.vehDetail[id]
	return
}

// InventoryOf is a player's last inventory from the mod (false: none yet,
// or the mod isn't connected).
func InventoryOf(guid string, now time.Time) (Inventory, bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) {
		return Inventory{}, false
	}
	inv, ok := state.inventory[strings.ToLower(guid)]
	return inv, ok
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
	// back is a teleport to where they were before the last one.
	// spawn puts Value (1 to maxSpawn) of the item class Text in front of them.
	// Shock, stamina and temperature are read only.
	Actions  = map[string]bool{"heal": true, "dry": true, "kill": true, "teleport": true, "teleportTo": true, "back": true, "set": true, "message": true, "spawn": true, "condition": true}
	StatMaxs = map[string]float64{"health": 100, "blood": 5000, "water": 5000, "energy": 5000, "heatBuffer": 30}
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
			return CommandStatus{}, errors.New("stat is health, blood, water, energy or heatBuffer (shock, stamina and temperature are read only)")
		}
		if c.Value < 0 || c.Value > max {
			return CommandStatus{}, errors.New(c.Stat + " is from 0 to " + strconv.FormatFloat(max, 'f', -1, 64))
		}
	case "teleport":
		if c.X <= 0 || c.Z <= 0 {
			return CommandStatus{}, errors.New("pick where to teleport them")
		}
	case "spawn":
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			return CommandStatus{}, errors.New("pick an item to spawn")
		}
		if c.Value != float64(int(c.Value)) || c.Value < 1 || c.Value > maxSpawn {
			return CommandStatus{}, errors.New("spawn 1 to " + strconv.Itoa(maxSpawn) + " at a time")
		}
		// Where it goes rides in Stat: ground (""), inventory or hands
		// (one). A mod before 0.0.1h puts everything on the ground.
		switch c.Stat {
		case "", "ground":
			c.Stat = ""
		case "inventory":
		case "hands":
			c.Value = 1
		default:
			return CommandStatus{}, errors.New("spawn on the ground, in their inventory or in their hands")
		}
	case "condition":
		// Text is the item's id (ReCON_Item.id), Value its condition: 0
		// pristine, 1 worn, 2 damaged, 3 badly damaged, 4 ruined.
		if _, err := strconv.Atoi(c.Text); err != nil {
			return CommandStatus{}, errors.New("pick an item")
		}
		if c.Value != float64(int(c.Value)) || c.Value < 0 || c.Value > 4 {
			return CommandStatus{}, errors.New("pick a condition: pristine, worn, damaged, badly damaged or ruined")
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
	if !connected(now) {
		return CommandStatus{}, errors.New("recon-mod isn't connected")
	}
	p, ok := state.players[guid]
	if !ok {
		return CommandStatus{}, errors.New("the player isn't online")
	}
	c.SteamID = p.SteamID
	back := false
	switch c.Action {
	case "teleportTo":
		t, ok := state.players[strings.ToLower(c.Target)]
		if !ok {
			return CommandStatus{}, errors.New("the player to go to isn't online")
		}
		c.Target = t.SteamID
	case "spawn":
		if state.catalogTypes == nil {
			return CommandStatus{}, errors.New("the item list hasn't come in from the mod yet")
		}
		it, ok := state.catalogTypes[c.Text]
		if !ok {
			return CommandStatus{}, errors.New(c.Text + " isn't an item the server can spawn")
		}
		switch it.Kind {
		case "vehicle":
			c.Value, c.Stat = 1, "" // whole, one at a time, in front of them
		case "animal":
			c.Stat = "" // on the ground in front of them
		}
	case "back":
		prev, ok := state.previous[guid]
		if !ok {
			return CommandStatus{}, errors.New("they haven't been teleported yet, so there is no previous spot")
		}
		// The mod runs it as a plain teleport, to the exact spot (height too).
		c.Action, c.X, c.Y, c.Z, c.Target = "teleport", prev.X, prev.Y, prev.Z, ""
		back = true
	}
	cs := add(c, guid, now)
	if c.Action == "teleport" || c.Action == "teleportTo" {
		// add keeps a pointer; set From and Back on the stored one.
		stored := state.commands[len(state.commands)-1]
		stored.From = &Position{X: p.X, Y: p.Y, Z: p.Z}
		stored.Back = back
		cs = *stored
	}
	return cs, nil
}

// Previous is where a player was before their last teleport.
func Previous(guid string) (Position, bool) {
	state.Lock()
	defer state.Unlock()
	p, ok := state.previous[strings.ToLower(guid)]
	return p, ok
}

// maxSpawn is the most of one item a spawn makes.
const maxSpawn = 20

// connected reports whether the mod synced lately (state is locked).
func connected(now time.Time) bool {
	return !state.lastSync.IsZero() && now.Sub(state.lastSync) <= staleAfter
}

// add queues a checked command (state is locked).
func add(c Command, guid string, now time.Time) CommandStatus {
	state.nextID++
	c.ID = state.nextID
	cs := &CommandStatus{Command: c, GUID: guid, Status: "queued", Queued: now}
	state.commands = append(state.commands, cs)
	return *cs
}

// WorldActions are the commands on the world rather than a player.
var WorldActions = map[string]bool{"weather": true, "weatherMode": true, "storm": true, "rainThresholds": true, "date": true, "freezeTime": true, "deleteVehicle": true, "spawnAt": true}

// weatherMax is each weather part's largest value.
var weatherMax = map[string]float64{"overcast": 1, "rain": 1, "fog": 1, "snow": 1, "windSpeed": 50, "windDir": 360}

// QueueWorld adds a command on the weather or the in-game time. Errors are
// user-facing.
func QueueWorld(c Command, now time.Time) (CommandStatus, error) {
	if err := checkWorld(&c); err != nil {
		return CommandStatus{}, err
	}
	state.Lock()
	defer state.Unlock()
	if !connected(now) {
		return CommandStatus{}, errors.New("recon-mod isn't connected")
	}
	if c.Action == "spawnAt" {
		if state.catalogTypes == nil {
			return CommandStatus{}, errors.New("the item list hasn't come in from the mod yet")
		}
		it, ok := state.catalogTypes[c.Text]
		if !ok {
			return CommandStatus{}, errors.New(c.Text + " isn't an item the server can spawn")
		}
		if it.Kind == "vehicle" {
			c.Value = 1 // whole, one at a time
		}
	}
	return add(c, "", now), nil
}

func checkWorld(c *Command) error {
	if !WorldActions[c.Action] {
		return errors.New("unknown action")
	}
	c.SteamID, c.Target = "", ""
	switch c.Action {
	case "deleteVehicle":
		// Text is the vehicle's id (Vehicle.ID).
		if _, err := strconv.Atoi(c.Text); err != nil {
			return errors.New("pick a vehicle")
		}
	case "spawnAt":
		// Text is the class, Value how many, X and Z where (mod 0.0.1j on).
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			return errors.New("pick an item to spawn")
		}
		if c.X <= 0 || c.Z <= 0 {
			return errors.New("pick where to spawn it")
		}
		if c.Value != float64(int(c.Value)) || c.Value < 1 || c.Value > maxSpawn {
			return errors.New("spawn 1 to " + strconv.Itoa(maxSpawn) + " at a time")
		}
	case "weather":
		max, ok := weatherMax[c.Stat]
		if !ok {
			return errors.New("the weather is overcast, rain, fog, snow, windSpeed or windDir")
		}
		if c.Value < 0 || c.Value > max {
			return errors.New(c.Stat + " is from 0 to " + strconv.FormatFloat(max, 'f', -1, 64))
		}
		if c.Transition < 0 || c.Transition > 3600 {
			return errors.New("the change takes 0 to 3600 seconds")
		}
		if c.Hold < 0 || c.Hold > 7*24*3600 {
			return errors.New("it holds for 0 seconds to 7 days")
		}
	case "weatherMode":
		if c.Text != "world" && c.Text != "forecast" && c.Text != "static" {
			return errors.New("the weather mode is world, forecast or static")
		}
	case "storm":
		if c.Value < 0 || c.Value > 1 || c.Min < 0 || c.Min > 1 {
			return errors.New("the storm's density and overcast are from 0 to 1")
		}
		if c.Hold < 1 {
			return errors.New("lightning needs at least 1 second between strikes")
		}
	case "rainThresholds":
		if c.Min < 0 || c.Max > 1 || c.Min > c.Max {
			return errors.New("rain's overcast range is from 0 to 1, lowest first")
		}
		if c.Transition < 0 {
			return errors.New("the time it takes rain to stop can't be negative")
		}
	case "date":
		t := time.Date(c.Year, time.Month(c.Month), c.Day, c.Hour, c.Minute, 0, 0, time.UTC)
		if c.Year < 1 || t.Year() != c.Year || int(t.Month()) != c.Month || t.Day() != c.Day || t.Hour() != c.Hour || t.Minute() != c.Minute {
			return errors.New("that isn't a date and time")
		}
	case "freezeTime":
		if c.Value != 0 && c.Value != 1 {
			return errors.New("freezeTime is 1 (frozen) or 0")
		}
	}
	return nil
}

// reset forgets everything (tests).
func reset() {
	state.Lock()
	defer state.Unlock()
	state.lastSync, state.mod, state.world, state.players = time.Time{}, "", "", nil
	state.nextID, state.commands, state.steamSaved, state.refused = 0, nil, nil, ""
	state.watch, state.inventory, state.previous = nil, nil, nil
	state.catalog, state.catalogTotal, state.catalogTypes = nil, 0, nil
	state.predators, state.vehFileFor, state.lastWorld = nil, "", ""
}

// battlEyeGUID is the BattlEye GUID of a SteamID64: md5("BE" + the ID as 8
// little-endian bytes), in lower-case hex.
func battlEyeGUID(id uint64) string {
	b := make([]byte, 10)
	b[0], b[1] = 'B', 'E'
	binary.LittleEndian.PutUint64(b[2:], id)
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Flag is a true/false the mod sends. DayZ's JSON writer may write it as
// true/false or as 1/0; both read. It is written back as true/false.
type Flag bool

// UnmarshalJSON reads true, false, 1, 0 (or "true", "1"...).
func (f *Flag) UnmarshalJSON(b []byte) error {
	switch strings.Trim(strings.TrimSpace(string(b)), `"`) {
	case "true", "1":
		*f = true
	case "false", "0", "", "null":
		*f = false
	default:
		return fmt.Errorf("not a true/false: %s", b)
	}
	return nil
}
