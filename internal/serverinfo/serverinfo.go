// Package serverinfo is the Server Info page's data: what the game server
// tells Steam's server query (name, map, version, players, in-game time and
// its speed, mods), when it answered (uptime), how many played when
// (population and busiest times, from the session history), and which mods
// Steam says were updated since someone acknowledged it.
//
// ReCON asks the server's query port every pollEvery, from the game host:
// QUERY_HOST (default the RCON host) and QUERY_PORT (default 27016).
package serverinfo

import (
	"context"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
)

// Config is where to ask, and what the page shows that the query can't
// tell: the server's public IP and its in-game date and latitude, for
// sunrise and sunset.
type Config struct {
	QueryHost string // where to send the query (the game host)
	QueryPort int
	PublicIP  string // shown under Connect; "" until known (SERVER_IP, or Steam)
	// ServerTime is serverDZ.cfg's serverTime: "2000/8/31/06/00"
	// (YYYY/MM/DD/HH/MM) or "SystemTime". Only its date is used.
	ServerTime string
	Latitude   float64 // degrees north; NaN: the map's (mapLatitude)
}

// ConfigFromEnv reads QUERY_HOST (default rconHost), QUERY_PORT (27016),
// SERVER_IP (default the query host when it is a public address, else what
// Steam says), SERVER_TIME (SystemTime) and SERVER_LATITUDE (the map's).
func ConfigFromEnv(rconHost string) Config {
	env := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{QueryHost: env("QUERY_HOST", rconHost), ServerTime: env("SERVER_TIME", "SystemTime"), Latitude: math.NaN()}
	c.QueryPort, _ = strconv.Atoi(env("QUERY_PORT", "27016"))
	if c.QueryPort <= 0 || c.QueryPort > 65535 {
		c.QueryPort = 27016
	}
	c.PublicIP = env("SERVER_IP", "")
	if c.PublicIP == "" {
		if ip := net.ParseIP(c.QueryHost); ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() {
			c.PublicIP = ip.String()
		}
	}
	if _, ok := parseServerDate(c.ServerTime, time.Now()); !ok {
		log.Printf("serverinfo: SERVER_TIME %q isn't like 2000/8/31/06/00 or SystemTime; using SystemTime", c.ServerTime)
		c.ServerTime = "SystemTime"
	}
	if v := env("SERVER_LATITUDE", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= -89 && f <= 89 {
			c.Latitude = f
		} else {
			log.Printf("serverinfo: SERVER_LATITUDE %q isn't a latitude between -89 and 89; using the map's", v)
		}
	}
	return c
}

// Snapshot is the latest answer, as the page shows it.
type Snapshot struct {
	Answered  bool      `json:"answered"` // the last query was answered
	Error     string    `json:"error,omitempty"`
	ReadAt    time.Time `json:"readAt"`   // when the last query was sent
	LastSeen  time.Time `json:"lastSeen"` // when the server last answered
	Info      Info      `json:"info"`
	Keywords  Keywords  `json:"keywords"`
	Mods      []Mod     `json:"mods"`
	ModsKnown bool      `json:"modsKnown"` // the mod list could be read
}

var (
	pollEvery     = time.Minute
	queryTimeout  = 3 * time.Second
	modCheckEvery = 3 * time.Hour
	modRetryAfter = 10 * time.Minute // after Steam didn't answer
	// uptimeFlushEvery is how often an unchanged uptime interval's end is
	// written (a change is written at once).
	uptimeFlushEvery = 10 * time.Minute
)

var state struct {
	sync.Mutex
	cfg  Config
	snap Snapshot
	// The open uptime interval.
	upID      int64
	up        bool
	upFlushed time.Time
	// The public IP Steam gave for the server's Steam ID (when SERVER_IP
	// isn't set and the query host isn't public), and when it was asked.
	steamIP      string
	steamIPFor   uint64
	steamIPAsked time.Time
	// The Workshop IDs last checked with Steam, and when.
	modsChecked    map[uint64]bool
	modsCheckedAt  time.Time
	modsCheckError string
}

var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }

// Current is the latest snapshot and the config.
func Current() (Snapshot, Config) {
	state.Lock()
	defer state.Unlock()
	s := state.snap
	s.Mods = append([]Mod(nil), s.Mods...)
	c := state.cfg
	if c.PublicIP == "" {
		c.PublicIP = state.steamIP
	}
	return s, c
}

// Start queries the server now and then every pollEvery, for as long as
// ReCON runs.
func Start(c Config) {
	state.Lock()
	state.cfg = c
	state.Unlock()
	log.Printf("serverinfo: asking %s:%d every %s", c.QueryHost, c.QueryPort, pollEvery)
	go func() {
		for {
			Refresh()
			time.Sleep(pollEvery)
		}
	}()
}

// Refresh queries the server now (the poller, and the page's Refresh).
func Refresh() Snapshot {
	state.Lock()
	cfg := state.cfg
	state.Unlock()
	now := time.Now()
	snap := Snapshot{ReadAt: now}
	a, err := dialA2S(net.JoinHostPort(cfg.QueryHost, strconv.Itoa(cfg.QueryPort)), queryTimeout)
	if err == nil {
		defer a.close()
		snap.Info, err = queryInfo(a)
	}
	if err != nil {
		snap.Error = err.Error()
	} else {
		snap.Answered, snap.LastSeen = true, now
		snap.Keywords = parseKeywords(snap.Info.Keywords)
		if rules, rerr := queryRules(a); rerr == nil {
			snap.Mods, snap.ModsKnown = decodeMods(rules)
		}
	}
	state.Lock()
	if !snap.Answered {
		// Keep the last answer on show, marked as not answering.
		old := state.snap
		old.Answered, old.Error, old.ReadAt = false, snap.Error, now
		snap = old
	} else if !snap.ModsKnown && state.snap.ModsKnown {
		snap.Mods, snap.ModsKnown = state.snap.Mods, true // a lost rules answer keeps the last list
	}
	state.snap = snap
	state.Unlock()
	recordUptime(snap.Answered, now)
	if snap.Answered && cfg.PublicIP == "" {
		lookUpPublicIP(snap.Info.SteamID, now)
	}
	if snap.ModsKnown {
		checkMods(snap.Mods, now)
	}
	return snap
}

// recordUptime extends the open uptime interval, or starts a new one when
// the server's state changed (or none is open since ReCON started).
func recordUptime(up bool, now time.Time) {
	if database == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	if state.upID != 0 && state.up == up {
		if now.Sub(state.upFlushed) >= uptimeFlushEvery {
			if err := database.ExtendUptime(state.upID, now); err != nil {
				log.Printf("serverinfo: uptime: %v", err)
			}
			state.upFlushed = now
		}
		return
	}
	if state.upID != 0 {
		if err := database.ExtendUptime(state.upID, now); err != nil {
			log.Printf("serverinfo: uptime: %v", err)
		}
	}
	id, err := database.StartUptime(up, now)
	if err != nil {
		log.Printf("serverinfo: uptime: %v", err)
		return
	}
	state.upID, state.up, state.upFlushed = id, up, now
}

// FlushUptime writes the open uptime interval's end as now, so a reading
// right after includes the time since the last write (the page asks first).
func FlushUptime(now time.Time) {
	if database == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	if state.upID == 0 {
		return
	}
	if err := database.ExtendUptime(state.upID, now); err != nil {
		log.Printf("serverinfo: uptime: %v", err)
	}
	state.upFlushed = now
}

// serverAddress asks Steam for a server's public IP (a var for the tests).
var serverAddress = steam.ServerAddress

// lookUpPublicIP asks Steam for the public IP of the server with this Steam
// ID, once (again after an hour if it failed, or if the ID changes).
func lookUpPublicIP(id uint64, now time.Time) {
	if id == 0 || steam.APIKey == "" {
		return
	}
	state.Lock()
	due := state.steamIPFor != id || (state.steamIP == "" && now.Sub(state.steamIPAsked) >= time.Hour)
	if due {
		state.steamIPFor, state.steamIPAsked = id, now
	}
	state.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ip, err := serverAddress(ctx, id)
	if err != nil {
		log.Printf("serverinfo: public IP from Steam: %v (set SERVER_IP to show it)", err)
	}
	state.Lock()
	state.steamIP = ip
	state.Unlock()
}

// workshopItems asks Steam about mods (a var for the tests).
var workshopItems = steam.WorkshopItems

// checkMods asks Steam when each mod was last updated, every modCheckEvery
// or at once when the server's mod list changes.
func checkMods(mods []Mod, now time.Time) {
	if database == nil {
		return
	}
	ids := make([]uint64, 0, len(mods))
	set := map[uint64]bool{}
	for _, m := range mods {
		if m.WorkshopID != 0 && !m.DLC {
			ids = append(ids, m.WorkshopID)
			set[m.WorkshopID] = true
		}
	}
	state.Lock()
	same := len(set) == len(state.modsChecked)
	for id := range set {
		same = same && state.modsChecked[id]
	}
	due := !same || now.Sub(state.modsCheckedAt) >= modCheckEvery
	if due {
		state.modsChecked, state.modsCheckedAt = set, now
	}
	state.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, err := workshopItems(ctx, ids)
	state.Lock()
	state.modsCheckError = ""
	if err != nil {
		state.modsCheckError = err.Error()
		state.modsCheckedAt = now.Add(modRetryAfter - modCheckEvery) // try again in modRetryAfter
	}
	state.Unlock()
	if err != nil {
		log.Printf("serverinfo: mods: %v", err)
		return
	}
	for _, it := range items {
		if err := database.SaveModFromSteam(store.ModUpdate{WorkshopID: strconv.FormatUint(it.ID, 10),
			Name: it.Title, SizeBytes: it.SizeBytes, SteamUpdated: it.Updated}); err != nil {
			log.Printf("serverinfo: mods: %v", err)
		}
	}
}

// ModsChecked is when Steam was last asked about the mods, and its error.
func ModsChecked() (time.Time, string) {
	state.Lock()
	defer state.Unlock()
	return state.modsCheckedAt, state.modsCheckError
}
