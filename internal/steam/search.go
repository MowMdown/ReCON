package steam

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// minTime is the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

const steamSearchRange = uint64(1) << 32 // every possible account ID

// steamSearchSplit is just above the newest Steam accounts: every account ID
// in use today is below 2^31 (about 2.1 billion), and the IDs above it don't
// exist yet. The search works up from the oldest accounts and down from here
// at the same time, so old accounts and brand-new ones (often the ban
// evaders) both turn up early. IDs above it are checked last, only for GUIDs
// still not found when the two fronts meet.
const steamSearchSplit = uint64(1) << 31

// chunkPlan hands out the ranges to check, in that order: alternately the
// next block up from start and the next block down from split, until the two
// meet; then split .. limit upwards. Every ID in [start, limit) is handed out
// exactly once. start is above 0 when a steam2guid table already covers the
// accounts below it.
type chunkPlan struct {
	mu             sync.Mutex
	up, down, tail uint64 // next ID of each front (down is exclusive)
	limit          uint64
	upTurn         bool
}

func newChunkPlan(start, split, limit uint64) *chunkPlan {
	start = min(start, limit)
	split = min(max(split, start), limit)
	return &chunkPlan{up: start, down: split, tail: split, limit: limit}
}

// next returns the next range [lo, hi) of at most size IDs; ok is false when
// everything has been handed out.
func (p *chunkPlan) next(size uint64) (lo, hi uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up < p.down {
		p.upTurn = !p.upTurn
		if p.upTurn {
			lo, hi = p.up, min(p.up+size, p.down)
			p.up = hi
		} else {
			lo, hi = p.up, p.down
			if hi-lo > size {
				lo = hi - size
			}
			p.down = lo
		}
		return lo, hi, true
	}
	if p.tail < p.limit {
		lo, hi = p.tail, min(p.tail+size, p.limit)
		p.tail = hi
		return lo, hi, true
	}
	return 0, 0, false
}

// steamSearchConfig is how hard the search works, from two settings:
//
//	STEAM_SEARCH_CORES     how many CPU cores it uses (default 1, at most all of them)
//	STEAM_SEARCH_PRIORITY  low, medium or high (default medium)
//
// Priority sets how much of the time each of those cores works:
//
//	low     33%  (pauses twice as long as it works)   1 core: about 33 min worst case
//	medium  66%  (pauses half as long as it works)    1 core: about 17 min worst case
//	high    99%  (barely pauses)                      1 core: about 11 min worst case
//
// The oldest and newest accounts are found long before the worst case. Pausing is used
// instead of OS thread priority because inside a container, thread priority
// only ranks ReCON's own threads against each other, not against other
// programs on the machine. The search threads are also marked lowest-
// priority, so they give way to the rest of ReCON.
type steamSearchConfig struct {
	Cores    int    `json:"cores"`
	Priority string `json:"priority"`
	Duty     int    `json:"-"` // percent of the time each core works
}

var steamPriorityDuty = map[string]int{"low": 33, "medium": 66, "high": 99}

// parseSteamSearchConfig reads the two settings; bad values are logged and
// replaced with the defaults.
func parseSteamSearchConfig(cores, priority string) steamSearchConfig {
	c := steamSearchConfig{Cores: 1, Priority: "medium"}
	if s := strings.TrimSpace(cores); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			c.Cores = min(n, runtime.NumCPU())
		} else {
			log.Printf("steam: STEAM_SEARCH_CORES=%q isn't a number of cores; using 1", s)
		}
	}
	if s := strings.ToLower(strings.TrimSpace(priority)); s != "" {
		if _, ok := steamPriorityDuty[s]; ok {
			c.Priority = s
		} else {
			log.Printf("steam: STEAM_SEARCH_PRIORITY=%q isn't low, medium or high; using medium", priority)
		}
	}
	c.Duty = steamPriorityDuty[c.Priority]
	return c
}

var steamSearchCfg = parseSteamSearchConfig(os.Getenv("STEAM_SEARCH_CORES"), os.Getenv("STEAM_SEARCH_PRIORITY"))

// steamSearchAuto is STEAM_SEARCH_MODE: "manual" (the default) searches only
// when someone presses Search Steam ID; "auto" also looks up every player
// with no SteamID when they join (PlayerJoined).
var steamSearchAuto = parseSteamSearchMode(os.Getenv("STEAM_SEARCH_MODE"))

func parseSteamSearchMode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "manual":
		return false
	case "auto":
		return true
	}
	log.Printf("steam: STEAM_SEARCH_MODE=%q isn't manual or auto; using manual", s)
	return false
}

// steamAutoSeen is every GUID PlayerJoined has already looked at since ReCON
// started, so a player who rejoins, or whom no search could match, isn't
// looked up again and again.
var steamAutoSeen = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// The auto mode batch: how long it waits after the first join, how much each
// further join adds, and when it stops waiting.
var (
	steamBatchFirstWait  = 60 * time.Second
	steamBatchJoinWait   = 30 * time.Second
	steamBatchMaxWait    = 3 * time.Minute
	steamBatchMaxPlayers = 15
)

// steamAutoBatch is the players waiting for the next auto mode search.
var steamAutoBatch = struct {
	sync.Mutex
	guids map[string]bool
	first time.Time   // when the first of them joined
	due   time.Time   // when the search starts
	timer *time.Timer // fires at due
}{guids: map[string]bool{}}

// PlayerJoined is called by the player tracker (package players) when a player's
// session opens. A player with no SteamID is looked up in the steam2guid
// table straight away; in auto mode, one the table can't answer joins the
// batch for the next search.
func PlayerJoined(guid, name string) {
	g := strings.ToLower(guid)
	if !battleye.GUIDPattern.MatchString(g) {
		return
	}
	steamAutoSeen.Lock()
	seen := steamAutoSeen.m[g]
	steamAutoSeen.m[g] = true
	steamAutoSeen.Unlock()
	if seen {
		return
	}
	go func() {
		id, known, err := database.SteamIDByGUID(g)
		if err != nil {
			log.Printf("steam: auto search: reading %s: %v", g, err)
			steamAutoSeen.Lock()
			delete(steamAutoSeen.m, g) // try again when they next join
			steamAutoSeen.Unlock()
			return
		}
		if !known || id != "" {
			return
		}
		if id, ok := lookupSteamTable(g); ok {
			saveSteamID(g, id)
			return
		}
		if !steamSearchAuto {
			return // manual: hashing waits for the button
		}
		log.Printf("steam: %s (%s) has no SteamID; searching soon (STEAM_SEARCH_MODE=auto)", name, g)
		addToSteamBatch(g, time.Now())
	}()
}

// lookupSteamTable reads one GUID's SteamID64 from the steam2guid table, if
// there is one and it has it.
func lookupSteamTable(guid string) (uint64, bool) {
	t := openSteamTable()
	if t == nil {
		return 0, false
	}
	defer t.close()
	t.workers = steamSearchCfg.Cores
	id, ok, err := t.lookup(guid)
	if err != nil {
		log.Printf("steam: table lookup of %s: %v", guid, err)
		return 0, false
	}
	return id, ok
}

// addToSteamBatch adds a joined player to the auto mode batch and sets when
// its search starts (see steamBatchFirstWait).
func addToSteamBatch(guid string, now time.Time) {
	b := &steamAutoBatch
	b.Lock()
	defer b.Unlock()
	b.guids[guid] = true
	if len(b.guids) == 1 {
		b.first = now
	}
	b.due = steamBatchDue(b.first, b.due, len(b.guids), now)
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(max(b.due.Sub(now), 0), runSteamBatch)
}

// steamBatchDue is when a batch of n players, the first of whom joined at
// first, should be searched for, now that another has joined; due is when it
// was going to start.
func steamBatchDue(first, due time.Time, n int, now time.Time) time.Time {
	if n >= steamBatchMaxPlayers {
		return now
	}
	if n == 1 {
		due = now.Add(steamBatchFirstWait)
	} else {
		due = due.Add(steamBatchJoinWait)
	}
	return minTime(due, first.Add(steamBatchMaxWait))
}

// runSteamBatch starts the search for the auto mode batch.
func runSteamBatch() {
	b := &steamAutoBatch
	b.Lock()
	guids := make([]string, 0, len(b.guids))
	for g := range b.guids {
		guids = append(guids, g)
	}
	b.guids, b.timer = map[string]bool{}, nil
	b.Unlock()
	if len(guids) == 0 {
		return
	}
	if err := StartSearch(guids...); err != nil {
		log.Printf("steam: auto search: %v", err)
	}
}

// lowerThreadPriority gives the calling OS thread the lowest CPU priority
// (nice 19). The caller has locked its goroutine to the thread. Needs no
// privileges; a failure only means the search runs at normal priority.
// Linux only: ReCON always runs in a Linux container.
func lowerThreadPriority() {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 19)
}

// searchSteamIDs hashes account IDs start .. limit-1 (in chunkPlan's order)
// until every wanted GUID (lower-case hex) is found, calling onFound once per
// match, one call at a time. checked counts the IDs done so far, for
// progress. Returns GUID -> SteamID64 for everything found.
func searchSteamIDs(wanted []string, start, limit uint64, cfg steamSearchConfig, checked *atomic.Uint64, onFound func(guid string, steamID uint64)) map[string]uint64 {
	want := make(map[[16]byte]string, len(wanted))
	for _, g := range wanted {
		var raw [16]byte
		if b, err := hex.DecodeString(g); err == nil && len(b) == 16 {
			copy(raw[:], b)
			want[raw] = g
		}
	}
	var (
		mu        sync.Mutex
		found     = map[string]uint64{}
		remaining atomic.Int64
		plan      = newChunkPlan(start, steamSearchSplit, limit)
		wg        sync.WaitGroup
	)
	remaining.Store(int64(len(want)))
	const chunk = 1 << 18 // about 40 ms of work per core: short pauses keep the load smooth
	duty := cfg.Duty
	if duty <= 0 || duty > 100 {
		duty = 100
	}
	for w := 0; w < max(cfg.Cores, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if duty < 100 {
				// Never unlocked: when this goroutine ends, Go retires the
				// lowered thread instead of reusing it for the rest of ReCON.
				runtime.LockOSThread()
				lowerThreadPriority()
			}
			var in [10]byte
			in[0], in[1] = 'B', 'E'
			for remaining.Load() > 0 {
				lo, hi, ok := plan.next(chunk)
				if !ok {
					return
				}
				start := time.Now()
				for id := lo; id < hi; id++ {
					binary.LittleEndian.PutUint64(in[2:], steamID64Base+id)
					g, ok := want[md5.Sum(in[:])]
					if !ok {
						continue
					}
					mu.Lock()
					if _, dup := found[g]; !dup {
						found[g] = steamID64Base + id
						remaining.Add(-1)
						onFound(g, steamID64Base+id)
					}
					mu.Unlock()
				}
				checked.Add(hi - lo)
				if duty < 100 && remaining.Load() > 0 {
					time.Sleep(time.Since(start) * time.Duration(100-duty) / time.Duration(duty))
				}
			}
		}()
	}
	wg.Wait()
	return found
}

// steamSearch is the background search: at most one pass at a time.
var steamSearch = struct {
	sync.Mutex
	running  bool
	wanted   map[string]bool      // GUIDs the current pass looks for
	queued   map[string]bool      // asked for during a pass they aren't in
	notFound map[string]time.Time // a full pass ended without a match
	started  time.Time
	checked  atomic.Uint64
}{queued: map[string]bool{}, notFound: map[string]time.Time{}}

// StartSearch asks for these GUIDs' SteamIDs. If no pass is running it
// starts one for them and every other GUID missing a SteamID; if one is
// running, those it isn't looking for wait for the next pass.
func StartSearch(guids ...string) error {
	steamSearch.Lock()
	defer steamSearch.Unlock()
	var list []string
	for _, g := range guids {
		g = strings.ToLower(g)
		delete(steamSearch.notFound, g)
		if !steamSearch.running {
			list = append(list, g)
		} else if !steamSearch.wanted[g] {
			steamSearch.queued[g] = true
		}
	}
	if steamSearch.running {
		return nil
	}
	return beginSteamSearchLocked(list)
}

// beginSteamSearchLocked starts a pass for the GUIDs in list plus every
// other GUID in players and bans still missing a SteamID: a pass costs the
// same for many GUIDs as for one. Caller holds steamSearch's lock.
func beginSteamSearchLocked(list []string) error {
	pending, err := database.ListPendingSteamGUIDs()
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, g := range append(pending, list...) {
		wanted[strings.ToLower(g)] = true
	}
	list = make([]string, 0, len(wanted))
	for g := range wanted {
		list = append(list, g)
	}
	if len(list) == 0 {
		return nil
	}
	steamSearch.running, steamSearch.wanted, steamSearch.started = true, wanted, time.Now()
	steamSearch.checked.Store(0)
	log.Printf("steam: searching for the SteamIDs of %d GUID(s) on %d core(s) at %s priority", len(list), steamSearchCfg.Cores, steamSearchCfg.Priority)
	go runSteamSearch(list)
	return nil
}

// saveSteamID stores a SteamID64 found for a GUID.
func saveSteamID(guid string, id uint64) {
	if err := database.SetSteamID(guid, strconv.FormatUint(id, 10)); err != nil {
		log.Printf("steam: found %s for %s but couldn't save it: %v", strconv.FormatUint(id, 10), guid, err)
		return
	}
	log.Printf("steam: %s is %d", guid, id)
}

// runSteamSearch is one pass: the table first, then hashing what it couldn't
// answer, then the next pass for anything queued meanwhile.
func runSteamSearch(list []string) {
	save := saveSteamID

	// The table first, if there is one: it answers for every account below
	// its MaxAccount, so only the IDs above that are left to hash.
	found := map[string]uint64{}
	start := uint64(0)
	if t := openSteamTable(); t != nil {
		t.workers = steamSearchCfg.Cores
		tableStart := time.Now()
		failed := false
		for _, g := range list {
			id, ok, err := t.lookup(g)
			if err != nil {
				log.Printf("steam: table lookup of %s: %v; searching without the table", g, err)
				failed = true
				break
			}
			if ok {
				found[g] = id
				save(g, id)
			}
		}
		t.close()
		if !failed {
			start = t.maxAccount
			log.Printf("steam: table (accounts below %d) found %d of %d in %s", t.maxAccount, len(found), len(list), time.Since(tableStart).Round(time.Millisecond))
		}
	}
	rest := make([]string, 0, len(list))
	for _, g := range list {
		if _, ok := found[g]; !ok {
			rest = append(rest, g)
		}
	}
	steamSearch.checked.Store(start)
	if len(rest) > 0 && start < steamSearchRange {
		if start > 0 {
			log.Printf("steam: searching account IDs %d and up for the other %d GUID(s)", start, len(rest))
		}
		for g, id := range searchSteamIDs(rest, start, steamSearchRange, steamSearchCfg, &steamSearch.checked, save) {
			found[g] = id
		}
	}
	steamSearch.checked.Store(steamSearchRange)

	steamSearch.Lock()
	defer steamSearch.Unlock()
	now := time.Now()
	for _, g := range list {
		if _, ok := found[g]; !ok {
			steamSearch.notFound[g] = now
		}
	}
	log.Printf("steam: search done in %s: found %d of %d", now.Sub(steamSearch.started).Round(time.Second), len(found), len(list))
	steamSearch.running, steamSearch.wanted = false, nil
	if len(steamSearch.queued) > 0 {
		queued := make([]string, 0, len(steamSearch.queued))
		for g := range steamSearch.queued {
			if _, done := found[g]; !done {
				queued = append(queued, g)
			}
		}
		steamSearch.queued = map[string]bool{}
		if err := beginSteamSearchLocked(queued); err != nil {
			log.Printf("steam: starting the next search: %v", err)
		}
	}
}

// SearchStatus is the search as the tab shows it, for a player with no
// SteamID: state is "searching", "queued", "notfound" or "" (never asked).
func SearchStatus(guid string) map[string]any {
	steamSearch.Lock()
	defer steamSearch.Unlock()
	st := map[string]any{"state": ""}
	switch {
	case steamSearch.running && steamSearch.wanted[guid]:
		st["state"] = "searching"
	case steamSearch.queued[guid]:
		st["state"] = "queued"
	case !steamSearch.notFound[guid].IsZero():
		st["state"] = "notfound"
		st["finishedUnix"] = steamSearch.notFound[guid].Unix()
	}
	st["cores"], st["priority"] = steamSearchCfg.Cores, steamSearchCfg.Priority
	st["auto"] = steamSearchAuto
	if steamSearch.running {
		st["percent"] = float64(steamSearch.checked.Load()) * 100 / float64(steamSearchRange)
		st["startedUnix"] = steamSearch.started.Unix()
		st["searchingFor"] = len(steamSearch.wanted)
	}
	return st
}
