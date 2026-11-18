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
// tell: the server's public IP and the map's sunrise and sunset.
type Config struct {
	QueryHost string // where to send the query (the game host)
	QueryPort int
	PublicIP  string // shown under Connect; "" when unknown
	Sunrise   string // "05:00", in-game
	Sunset    string // "19:00"
}

// ConfigFromEnv reads QUERY_HOST (default rconHost), QUERY_PORT (27016),
// SERVER_IP (default the query host when it is a public address),
// SERVER_SUNRISE (05:00) and SERVER_SUNSET (19:00).
func ConfigFromEnv(rconHost string) Config {
	env := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{QueryHost: env("QUERY_HOST", rconHost), Sunrise: env("SERVER_SUNRISE", "05:00"), Sunset: env("SERVER_SUNSET", "19:00")}
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
	if _, ok := clockHours(c.Sunrise); !ok {
		log.Printf("serverinfo: SERVER_SUNRISE %q isn't a time like 05:00; using 05:00", c.Sunrise)
		c.Sunrise = "05:00"
	}
	if _, ok := clockHours(c.Sunset); !ok {
		log.Printf("serverinfo: SERVER_SUNSET %q isn't a time like 19:00; using 19:00", c.Sunset)
		c.Sunset = "19:00"
	}
	return c
}

// clockHours reads "HH:MM" as hours (5.5 for 05:30).
func clockHours(s string) (float64, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return float64(hh) + float64(mm)/60, true
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
	return s, state.cfg
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
