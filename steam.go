package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ============================================================================
// steam.go overview
// ----------------------------------------------------------------------------
// The Details dialog's Steam Profile tab: a player's Steam account, looked
// up from the SteamID64 stored in players.steam_id.
//
//	GET  /api/players/{guid}/steam             the profile (cached for steamCacheFor)
//	GET  /api/players/{guid}/steam?refresh=1   fetch now (at most every steamMinRefresh)
//	POST /api/players/{guid}/steam/search      find the SteamID (players without one)
//
// Finding the SteamID (this replaces the separate guidsearch tool): BattlEye
// only gives us a GUID, md5("BE" + SteamID64 as 8 little-endian bytes), which
// can't be reversed. So the search hashes every Steam account ID (0 to 2^32)
// until the GUID matches, working up from the oldest accounts and down from
// the newest at once (see steamSearchSplit). Old and new accounts turn up
// soonest; a full pass takes several minutes. It runs in the background, one pass at a time, and a
// pass looks for every GUID in players and bans that has no SteamID yet,
// since many GUIDs cost the same as one. Each SteamID is written to players
// and bans the moment it is found. A search asked for while a pass is running
// waits for the next pass. STEAM_SEARCH_CORES and STEAM_SEARCH_PRIORITY set
// how much CPU it uses (see steamSearchConfig; default 1 core, medium).
// STEAM_SEARCH_MODE=auto also starts a search when a player without a
// SteamID is first seen online (steamPlayersSeen); manual, the default, only
// searches when the button is pressed.
//
// steam2guid tables (STEAM_TABLE_DIR, default the folder players.db is in,
// so /data): if a table built
// by the steam2guid tool is in that folder, each pass looks every GUID up in
// it first (a disk read, well under a millisecond for 4- and 8-byte tables)
// and only brute-forces what the table can't answer: account IDs at or above
// the table's MaxAccount. A table covering all 2^32 accounts means no
// hashing at all; a GUID it doesn't have isn't a Steam account's. No table,
// or one that is damaged or incomplete, means the full search as before. The
// table is opened fresh for every pass, so adding, rebuilding or removing it
// needs no restart. Only reading is built in; tables are made with steam2guid.
//
// Lookups need STEAM_API_KEY (free at steamcommunity.com/dev/apikey). Without
// it the tab still shows the SteamID and a link to the profile. Each lookup
// is five Web API calls, made at the same time:
//
//	ISteamUser/GetPlayerSummaries      name, avatar, online status, public or
//	                                   private; account age, country and last
//	                                   online only for public profiles
//	ISteamUser/GetPlayerBans           VAC bans, game bans, days since the
//	                                   last one, community and trade bans
//	IPlayerService/GetSteamLevel       Steam level
//	IPlayerService/GetOwnedGames       games owned, DayZ hours (if the game
//	                                   details are public)
//	ISteamUser/GetFriendList           friend count, and which friends have
//	                                   played here (if the list is public)
//
// Only the first two must succeed; the rest are left empty ("Hidden") when
// the player keeps them private or the call fails. Nothing is written to the
// database: profiles are kept in memory, so a restart starts fresh.
// ============================================================================

const (
	dayzAppID       = 221100           // DayZ on Steam
	steamCacheFor   = 30 * time.Minute // how long a fetched profile is reused
	steamMinRefresh = 30 * time.Second // ?refresh=1 within this of the last fetch gets the cached copy
	steamID64Base   = uint64(76561197960265728)
)

var (
	steamAPIBase = "https://api.steampowered.com" // a var so tests can use a fake server
	steamAPIKey  = strings.TrimSpace(os.Getenv("STEAM_API_KEY"))
	steamHTTP    = &http.Client{Timeout: 10 * time.Second}
)

func init() {
	if steamAPIKey == "" {
		log.Printf("steam: STEAM_API_KEY not set; the Steam Profile tab shows only the SteamID and profile link")
	}
}

// steamProfile is one Steam account as the Steam Profile tab shows it.
// Pointer fields are nil when Steam didn't say (a private profile or a
// failed optional call), which the page shows as "Hidden".
type steamProfile struct {
	SteamID    string `json:"steamId"` // a string: too big for a JavaScript number
	ProfileURL string `json:"profileUrl"`
	Found      bool   `json:"found"` // false when Steam has no such account

	PersonaName    string `json:"personaName"`
	AvatarURL      string `json:"avatarUrl"`
	Public         bool   `json:"public"`
	Status         string `json:"status"`           // Online, Offline, Busy, Away, Snooze, Looking to trade, Looking to play
	InGame         string `json:"inGame,omitempty"` // the game they're playing right now
	CountryCode    string `json:"countryCode,omitempty"`
	CreatedUnix    int64  `json:"createdUnix,omitempty"`
	LastOnlineUnix int64  `json:"lastOnlineUnix,omitempty"`

	VACBanned        bool   `json:"vacBanned"`
	VACBans          int    `json:"vacBans"`
	GameBans         int    `json:"gameBans"`
	DaysSinceLastBan int    `json:"daysSinceLastBan"` // as of FetchedUnix
	CommunityBanned  bool   `json:"communityBanned"`
	EconomyBan       string `json:"economyBan"` // none, probation or banned

	Level              *int  `json:"level"`
	GamesOwned         *int  `json:"gamesOwned"`
	DayZOwned          *bool `json:"dayzOwned"` // nil when the game list is hidden
	DayZMinutes        int   `json:"dayzMinutes"`
	DayZ2WeeksMinutes  int   `json:"dayz2WeeksMinutes"`
	DayZLastPlayedUnix int64 `json:"dayzLastPlayedUnix,omitempty"`
	Friends            *int  `json:"friends"`

	FriendIDs   []string `json:"-"` // for "friends who have played here"
	FetchedUnix int64    `json:"fetchedUnix"`
}

// ---------------------------------------------------------------------------
// SteamIDs and BattlEye GUIDs
// ---------------------------------------------------------------------------

// parseSteamID64 accepts a 17-digit SteamID64 of an individual account.
func parseSteamID64(s string) (uint64, bool) {
	if len(s) != 17 {
		return 0, false
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id <= steamID64Base || id-steamID64Base > 0xFFFFFFFF {
		return 0, false
	}
	return id, true
}

// battlEyeGUID is the BattlEye GUID of a SteamID64: md5("BE" + the ID as 8
// little-endian bytes), in lower-case hex. Used to check that the stored
// SteamID really belongs to the player.
func battlEyeGUID(id uint64) string {
	b := make([]byte, 10)
	b[0], b[1] = 'B', 'E'
	binary.LittleEndian.PutUint64(b[2:], id)
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Steam Web API
// ---------------------------------------------------------------------------

// steamHTTPError is a non-200 reply. GetFriendList answers 401 for a private
// list, which is not a failure.
type steamHTTPError struct {
	method string
	code   int
}

func (e *steamHTTPError) Error() string {
	switch e.code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("steam %s: the API key was rejected (HTTP %d)", e.method, e.code)
	case http.StatusTooManyRequests:
		return fmt.Sprintf("steam %s: rate limited, try again later", e.method)
	}
	return fmt.Sprintf("steam %s: HTTP %d", e.method, e.code)
}

// steamGet calls one Web API method and decodes the JSON reply into out. The
// key travels in the query string, so it is kept out of every error (net/http
// puts the whole URL into its errors).
func steamGet(ctx context.Context, method string, q url.Values, out any) error {
	q.Set("key", steamAPIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, steamAPIBase+method+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("steam %s: %w", method, err)
	}
	resp, err := steamHTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, which contains the key
		}
		return fmt.Errorf("steam %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &steamHTTPError{method: method, code: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("steam %s: bad reply: %w", method, err)
	}
	return nil
}

var steamPersonaStates = []string{"Offline", "Online", "Busy", "Away", "Snooze", "Looking to trade", "Looking to play"}

// fetchSteamProfile looks one account up (five calls at once). Only a failed
// summary or ban lookup is an error.
func fetchSteamProfile(ctx context.Context, steamID string) (*steamProfile, error) {
	var (
		sums struct {
			Response struct {
				Players []struct {
					Visibility   int    `json:"communityvisibilitystate"` // 3 = public
					PersonaName  string `json:"personaname"`
					ProfileURL   string `json:"profileurl"`
					AvatarFull   string `json:"avatarfull"`
					PersonaState int    `json:"personastate"`
					LastLogoff   int64  `json:"lastlogoff"`
					TimeCreated  int64  `json:"timecreated"`
					Country      string `json:"loccountrycode"`
					GameName     string `json:"gameextrainfo"`
				} `json:"players"`
			} `json:"response"`
		}
		bans struct {
			Players []struct {
				CommunityBanned  bool   `json:"CommunityBanned"`
				VACBanned        bool   `json:"VACBanned"`
				NumberOfVACBans  int    `json:"NumberOfVACBans"`
				DaysSinceLastBan int    `json:"DaysSinceLastBan"`
				NumberOfGameBans int    `json:"NumberOfGameBans"`
				EconomyBan       string `json:"EconomyBan"`
			} `json:"players"`
		}
		level struct {
			Response struct {
				Level *int `json:"player_level"`
			} `json:"response"`
		}
		games struct {
			Response struct {
				Count *int `json:"game_count"` // missing when the game details are private
				Games []struct {
					AppID      int   `json:"appid"`
					Forever    int   `json:"playtime_forever"` // minutes
					TwoWeeks   int   `json:"playtime_2weeks"`
					LastPlayed int64 `json:"rtime_last_played"`
				} `json:"games"`
			} `json:"response"`
		}
		friends struct {
			FriendsList struct {
				Friends []struct {
					SteamID string `json:"steamid"`
				} `json:"friends"`
			} `json:"friendslist"`
		}
		sumErr, banErr, levelErr, gamesErr, friendsErr error
	)

	ids := url.Values{"steamids": {steamID}}
	var wg sync.WaitGroup
	call := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	call(func() { sumErr = steamGet(ctx, "/ISteamUser/GetPlayerSummaries/v2/", ids, &sums) })
	call(func() {
		banErr = steamGet(ctx, "/ISteamUser/GetPlayerBans/v1/", url.Values{"steamids": {steamID}}, &bans)
	})
	call(func() {
		levelErr = steamGet(ctx, "/IPlayerService/GetSteamLevel/v1/", url.Values{"steamid": {steamID}}, &level)
	})
	call(func() {
		gamesErr = steamGet(ctx, "/IPlayerService/GetOwnedGames/v1/",
			url.Values{"steamid": {steamID}, "include_played_free_games": {"1"}}, &games)
	})
	call(func() {
		friendsErr = steamGet(ctx, "/ISteamUser/GetFriendList/v1/",
			url.Values{"steamid": {steamID}, "relationship": {"friend"}}, &friends)
	})
	wg.Wait()

	if sumErr != nil {
		return nil, sumErr
	}
	if banErr != nil {
		return nil, banErr
	}
	p := &steamProfile{SteamID: steamID, ProfileURL: "https://steamcommunity.com/profiles/" + steamID, FetchedUnix: time.Now().Unix()}
	if len(sums.Response.Players) == 0 {
		return p, nil // no such account (deleted, or never existed)
	}
	s := sums.Response.Players[0]
	p.Found = true
	p.PersonaName, p.AvatarURL, p.Public = s.PersonaName, s.AvatarFull, s.Visibility == 3
	if s.ProfileURL != "" {
		p.ProfileURL = s.ProfileURL
	}
	if s.PersonaState >= 0 && s.PersonaState < len(steamPersonaStates) {
		p.Status = steamPersonaStates[s.PersonaState]
	}
	p.InGame, p.CountryCode, p.CreatedUnix, p.LastOnlineUnix = s.GameName, s.Country, s.TimeCreated, s.LastLogoff

	if len(bans.Players) > 0 {
		b := bans.Players[0]
		p.VACBanned, p.VACBans, p.GameBans = b.VACBanned, b.NumberOfVACBans, b.NumberOfGameBans
		p.DaysSinceLastBan, p.CommunityBanned, p.EconomyBan = b.DaysSinceLastBan, b.CommunityBanned, b.EconomyBan
	}

	if levelErr == nil {
		p.Level = level.Response.Level
	} else {
		log.Printf("steam: level of %s: %v", steamID, levelErr)
	}

	if gamesErr == nil && games.Response.Count != nil {
		p.GamesOwned = games.Response.Count
		owned := false
		for _, g := range games.Response.Games {
			if g.AppID == dayzAppID {
				owned = true
				p.DayZMinutes, p.DayZ2WeeksMinutes, p.DayZLastPlayedUnix = g.Forever, g.TwoWeeks, g.LastPlayed
				break
			}
		}
		p.DayZOwned = &owned
	} else if gamesErr != nil {
		log.Printf("steam: games of %s: %v", steamID, gamesErr)
	}

	var he *steamHTTPError
	switch {
	case friendsErr == nil:
		n := len(friends.FriendsList.Friends)
		p.Friends = &n
		for _, f := range friends.FriendsList.Friends {
			p.FriendIDs = append(p.FriendIDs, f.SteamID)
		}
	case errors.As(friendsErr, &he) && he.code == http.StatusUnauthorized:
		// private friends list: leave Friends nil
	default:
		log.Printf("steam: friends of %s: %v", steamID, friendsErr)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

var steamCache = struct {
	sync.Mutex
	byID map[string]*steamProfile
}{byID: map[string]*steamProfile{}}

// steamProfileFor returns a profile younger than steamCacheFor, fetching it
// when there isn't one (or when refresh asks and the copy is older than
// steamMinRefresh). If the fetch fails but an older copy exists, that copy is
// returned along with the error.
func steamProfileFor(ctx context.Context, steamID string, refresh bool) (*steamProfile, error) {
	steamCache.Lock()
	old := steamCache.byID[steamID]
	steamCache.Unlock()
	if old != nil {
		age := time.Since(time.Unix(old.FetchedUnix, 0))
		if age < steamMinRefresh || (!refresh && age < steamCacheFor) {
			return old, nil
		}
	}
	p, err := fetchSteamProfile(ctx, steamID)
	if err != nil {
		return old, err
	}
	steamCache.Lock()
	steamCache.byID[steamID] = p
	if len(steamCache.byID) > 1000 { // drop expired copies now and then
		for id, c := range steamCache.byID {
			if time.Since(time.Unix(c.FetchedUnix, 0)) > steamCacheFor {
				delete(steamCache.byID, id)
			}
		}
	}
	steamCache.Unlock()
	return p, nil
}

// ---------------------------------------------------------------------------
// Finding a SteamID from a GUID
// ---------------------------------------------------------------------------

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
// when someone presses Search Steam ID; "auto" also starts a search the
// first time the player poller sees a player with no SteamID, so players
// get one shortly after they join.
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

// steamAutoSeen is every GUID auto mode has already looked at since ReCON
// started, so each player is checked once, not on every 5-second poll, and a
// player no search could match isn't searched for again and again.
var steamAutoSeen = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// steamPlayersSeen is called by the player poller (db.go) after each
// successful poll. In auto mode it starts a SteamID search for any online
// player seen for the first time this run who has no SteamID yet.
func steamPlayersSeen(players []Player) {
	if !steamSearchAuto {
		return
	}
	for _, p := range players {
		g := strings.ToLower(p.GUID)
		if !guidPattern.MatchString(g) {
			continue
		}
		steamAutoSeen.Lock()
		seen := steamAutoSeen.m[g]
		steamAutoSeen.m[g] = true
		steamAutoSeen.Unlock()
		if seen {
			continue
		}
		id, known, err := globalDatabase.SteamIDByGUID(g)
		if err != nil {
			log.Printf("steam: auto search: reading %s: %v", g, err)
			steamAutoSeen.Lock()
			delete(steamAutoSeen.m, g) // try again on the next poll
			steamAutoSeen.Unlock()
			continue
		}
		if !known || id != "" {
			continue
		}
		log.Printf("steam: %s (%s) has no SteamID; searching (STEAM_SEARCH_MODE=auto)", bareName(p.Name), g)
		if err := startSteamSearch(g); err != nil {
			log.Printf("steam: auto search for %s: %v", g, err)
		}
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

// startSteamSearch asks for guid's SteamID. If no pass is running it starts
// one for guid plus every other GUID still missing a SteamID; if one is
// running without guid, guid waits for the next pass.
func startSteamSearch(guid string) error {
	steamSearch.Lock()
	defer steamSearch.Unlock()
	delete(steamSearch.notFound, guid)
	if steamSearch.running {
		if !steamSearch.wanted[guid] {
			steamSearch.queued[guid] = true
		}
		return nil
	}
	return beginSteamSearchLocked([]string{guid})
}

// beginSteamSearchLocked starts a pass for extra plus every GUID in players
// and bans that has no SteamID. Caller holds steamSearch's lock.
func beginSteamSearchLocked(extra []string) error {
	pending, err := globalDatabase.PendingSteamGUIDs()
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, g := range append(pending, extra...) {
		wanted[strings.ToLower(g)] = true
	}
	list := make([]string, 0, len(wanted))
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

func runSteamSearch(list []string) {
	save := func(guid string, id uint64) {
		if err := globalDatabase.SetSteamID(guid, strconv.FormatUint(id, 10)); err != nil {
			log.Printf("steam: found %s for %s but couldn't save it: %v", strconv.FormatUint(id, 10), guid, err)
			return
		}
		log.Printf("steam: %s is %d", guid, id)
	}

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

// steamSearchStatus is the search as the tab shows it, for a player with no
// SteamID: state is "searching", "queued", "notfound" or "" (never asked).
func steamSearchStatus(guid string) map[string]any {
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

// ---------------------------------------------------------------------------
// steam2guid tables (read only; built with the steam2guid tool)
// ---------------------------------------------------------------------------
//
// A table is one or more part files, steam2guid-NNN.bin, listing every
// account ID below MaxAccount sorted by GUID. Every part:
//
//	offset  size  field
//	0       8     magic "STM2GUID"
//	8       2     version (1)
//	10      1     entry bytes: 2, 4 or 8
//	11      1     index bits (b): groups are keyed by the GUID's top b bits
//	12      2     this part's number (0-based)
//	14      2     number of parts
//	16      8     SteamID64 base (76561197960265728)
//	24      8     MaxAccount: account IDs 0 .. MaxAccount-1 are in the table
//	32      4     first group in this part
//	36      4     number of groups in this part
//	40      8     entries in this part
//	48      8     entries in all parts
//	56      4     reserved (0)
//	60      4     CRC-32 (IEEE) of bytes 0..59
//	64      4*G   index: where each group starts (an entry number within
//	              this part); a group ends where the next starts, or at the
//	              part's entry count
//	...     E*n   entries, sorted by GUID
//
// Header and index integers are little-endian. "key" is the 32 GUID bits
// right after the index bits. Entries are:
//
//	8 bytes  [key, big-endian][account ID, little-endian]: binary search on key
//	4 bytes  [account ID, little-endian]: interpolation search, an MD5 per probe
//	2 bytes  [low 16 bits of the account ID]: every high half is tried against
//	         every entry in the group, tens of millions of MD5s at worst
//
// This must stay in step with the steam2guid tool's steamtable package.

// steamTableDir is where to look for a steam2guid table: STEAM_TABLE_DIR if
// set (any folder), otherwise the database's folder, so the part files sit
// next to players.db (/data with the default DB_PATH).
var steamTableDir = func() string {
	if d := strings.TrimSpace(os.Getenv("STEAM_TABLE_DIR")); d != "" {
		return d
	}
	return filepath.Dir(environmentOrDefault("DB_PATH", "/data/players.db"))
}()

const (
	steamTableMagic      = "STM2GUID"
	steamTableVersion    = 1
	steamTableHeaderSize = 64
	steamTableGlob       = "steam2guid-*.bin"
	steamTableMinBits    = 4
	steamTableMaxBits    = 30
)

type steamTableHeader struct {
	entryBytes, indexBits int
	part, parts           int
	maxAccount            uint64
	firstGroup, groups    uint32
	entries, total        uint64
}

func (h steamTableHeader) entriesOffset() int64 { return steamTableHeaderSize + 4*int64(h.groups) }
func (h steamTableHeader) fileSize() int64 {
	return h.entriesOffset() + int64(h.entries)*int64(h.entryBytes)
}

func parseSteamTableHeader(b []byte) (steamTableHeader, error) {
	var h steamTableHeader
	if len(b) < steamTableHeaderSize || string(b[:8]) != steamTableMagic {
		return h, errors.New("not a steam2guid table (bad magic)")
	}
	le := binary.LittleEndian
	if crc32.ChecksumIEEE(b[:60]) != le.Uint32(b[60:]) {
		return h, errors.New("header checksum mismatch")
	}
	if v := le.Uint16(b[8:]); v != steamTableVersion {
		return h, fmt.Errorf("unsupported table version %d", v)
	}
	if le.Uint64(b[16:]) != steamID64Base {
		return h, errors.New("unexpected SteamID64 base")
	}
	h = steamTableHeader{
		entryBytes: int(b[10]), indexBits: int(b[11]),
		part: int(le.Uint16(b[12:])), parts: int(le.Uint16(b[14:])),
		maxAccount: le.Uint64(b[24:]), firstGroup: le.Uint32(b[32:]),
		groups: le.Uint32(b[36:]), entries: le.Uint64(b[40:]), total: le.Uint64(b[48:]),
	}
	switch {
	case h.entryBytes != 2 && h.entryBytes != 4 && h.entryBytes != 8:
		return h, fmt.Errorf("bad entry size %d", h.entryBytes)
	case h.indexBits < steamTableMinBits || h.indexBits > steamTableMaxBits:
		return h, fmt.Errorf("bad index bits %d", h.indexBits)
	case h.maxAccount == 0 || h.maxAccount > steamSearchRange:
		return h, fmt.Errorf("bad account range %d", h.maxAccount)
	case h.parts == 0 || h.part >= h.parts:
		return h, fmt.Errorf("bad part %d of %d", h.part, h.parts)
	}
	return h, nil
}

type steamTablePart struct {
	f *os.File
	h steamTableHeader
}

// steamTable is an open table.
type steamTable struct {
	parts                 []steamTablePart
	entryBytes, indexBits int
	maxAccount            uint64 // accounts 0 .. maxAccount-1 are covered
	workers               int    // goroutines for a 2-byte lookup
}

// openSteamTable opens the table in steamTableDir, or returns nil when there
// isn't one or it can't be used (logged: the search then runs without it).
func openSteamTable() *steamTable {
	names, _ := filepath.Glob(filepath.Join(steamTableDir, steamTableGlob))
	if len(names) == 0 {
		return nil
	}
	t, err := openSteamTableFiles(names)
	if err != nil {
		log.Printf("steam: ignoring the table in %s: %v", steamTableDir, err)
		return nil
	}
	return t
}

func openSteamTableFiles(names []string) (*steamTable, error) {
	t := &steamTable{workers: 1}
	fail := func(err error) (*steamTable, error) { t.close(); return nil, err }
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			return fail(err)
		}
		t.parts = append(t.parts, steamTablePart{f: f})
		p := &t.parts[len(t.parts)-1]
		buf := make([]byte, steamTableHeaderSize)
		if _, err := f.ReadAt(buf, 0); err != nil {
			return fail(fmt.Errorf("%s: %v", name, err))
		}
		if p.h, err = parseSteamTableHeader(buf); err != nil {
			return fail(fmt.Errorf("%s: %v", name, err))
		}
		if st, err := f.Stat(); err != nil || st.Size() != p.h.fileSize() {
			return fail(fmt.Errorf("%s: wrong size (incomplete or damaged file)", name))
		}
	}
	sort.Slice(t.parts, func(i, j int) bool { return t.parts[i].h.part < t.parts[j].h.part })
	h0 := t.parts[0].h
	t.entryBytes, t.indexBits, t.maxAccount = h0.entryBytes, h0.indexBits, h0.maxAccount
	var next uint64
	var sum uint64
	for i, p := range t.parts {
		h := p.h
		switch {
		case h.part != i || h.parts != len(t.parts):
			return fail(fmt.Errorf("%s: part %d of %d, but found %d part file(s)", p.f.Name(), h.part, h.parts, len(t.parts)))
		case h.entryBytes != h0.entryBytes || h.indexBits != h0.indexBits || h.maxAccount != h0.maxAccount || h.total != h0.total:
			return fail(fmt.Errorf("%s: belongs to a different table", p.f.Name()))
		case uint64(h.firstGroup) != next:
			return fail(fmt.Errorf("%s: starts at group %d, expected %d", p.f.Name(), h.firstGroup, next))
		}
		next += uint64(h.groups)
		sum += h.entries
	}
	if next != uint64(1)<<h0.indexBits || sum != h0.total || sum != h0.maxAccount {
		return fail(errors.New("parts don't cover the whole table"))
	}
	return t, nil
}

func (t *steamTable) close() {
	for _, p := range t.parts {
		if p.f != nil {
			p.f.Close()
		}
	}
}

// steamTableGUIDOf is the raw BattlEye GUID of an account ID.
func steamTableGUIDOf(account uint32) [16]byte {
	var in [10]byte
	in[0], in[1] = 'B', 'E'
	binary.LittleEndian.PutUint64(in[2:], steamID64Base+uint64(account))
	return md5.Sum(in[:])
}

// steamTableSplit returns a GUID's group (its top indexBits bits) and key
// (the next 32 bits).
func steamTableSplit(g *[16]byte, indexBits int) (group, key uint32) {
	u := binary.BigEndian.Uint64(g[:8])
	return uint32(u >> (64 - indexBits)), uint32((u << indexBits) >> 32)
}

// lookup finds the SteamID64 of a hex GUID. found is false when no account
// below maxAccount has that GUID.
func (t *steamTable) lookup(guid string) (steamID64 uint64, found bool, err error) {
	var g [16]byte
	if b, err := hex.DecodeString(strings.TrimSpace(guid)); err != nil || len(b) != 16 {
		return 0, false, fmt.Errorf("GUID %q: want 32 hex characters", guid)
	} else {
		copy(g[:], b)
	}
	group, key := steamTableSplit(&g, t.indexBits)
	i := sort.Search(len(t.parts), func(i int) bool {
		h := t.parts[i].h
		return uint64(h.firstGroup)+uint64(h.groups) > uint64(group)
	})
	if i == len(t.parts) {
		return 0, false, errors.New("group outside the table")
	}
	p := t.parts[i]
	local := group - p.h.firstGroup

	// where the group starts and ends
	var ib [8]byte
	n := 8
	if local+1 == p.h.groups {
		n = 4
	}
	if _, err := p.f.ReadAt(ib[:n], steamTableHeaderSize+4*int64(local)); err != nil {
		return 0, false, fmt.Errorf("%s: index: %v", p.f.Name(), err)
	}
	start := uint64(binary.LittleEndian.Uint32(ib[:]))
	end := p.h.entries
	if n == 8 {
		end = uint64(binary.LittleEndian.Uint32(ib[4:]))
	}
	if end < start || end > p.h.entries {
		return 0, false, fmt.Errorf("%s: damaged index at group %d", p.f.Name(), group)
	}
	if end == start {
		return 0, false, nil
	}
	eb := int64(t.entryBytes)
	buf := make([]byte, int64(end-start)*eb)
	if _, err := p.f.ReadAt(buf, p.h.entriesOffset()+int64(start)*eb); err != nil {
		return 0, false, fmt.Errorf("%s: entries: %v", p.f.Name(), err)
	}

	var acc uint32
	var ok bool
	switch t.entryBytes {
	case 8:
		acc, ok = steamTableLookup8(buf, key, &g)
	case 4:
		acc, ok = steamTableLookup4(buf, key, &g, t.indexBits)
	case 2:
		acc, ok = steamTableLookup2(buf, &g, t.maxAccount, t.workers)
	}
	if !ok {
		return 0, false, nil
	}
	return steamID64Base + uint64(acc), true, nil
}

// steamTableLookup8: entries are [key BE][account LE], sorted by key.
func steamTableLookup8(buf []byte, key uint32, g *[16]byte) (uint32, bool) {
	n := len(buf) / 8
	i := sort.Search(n, func(i int) bool { return binary.BigEndian.Uint32(buf[i*8:]) >= key })
	for ; i < n && binary.BigEndian.Uint32(buf[i*8:]) == key; i++ {
		acc := binary.LittleEndian.Uint32(buf[i*8+4:])
		if steamTableGUIDOf(acc) == *g {
			return acc, true
		}
	}
	return 0, false
}

// steamTableLookup4: entries are account IDs sorted by GUID. GUIDs are
// uniform, so the key predicts where the entry sits (interpolation search);
// after a few probes it falls back to plain bisection.
func steamTableLookup4(buf []byte, key uint32, g *[16]byte, indexBits int) (uint32, bool) {
	lo, hi := 0, len(buf)/4
	loKey, hiKey := uint64(0), uint64(1)<<32 // keys bounding [lo, hi)
	for step := 0; lo < hi; step++ {
		mid := (lo + hi) / 2
		if step < 4 && hiKey > loKey && uint64(key) >= loKey {
			mid = lo + int((uint64(key)-loKey)*uint64(hi-lo)/(hiKey-loKey))
			mid = min(max(mid, lo), hi-1)
		}
		acc := binary.LittleEndian.Uint32(buf[mid*4:])
		h := steamTableGUIDOf(acc)
		c := bytes.Compare(h[:], g[:])
		if c == 0 {
			return acc, true
		}
		_, k := steamTableSplit(&h, indexBits)
		if c < 0 {
			lo, loKey = mid+1, uint64(k)
		} else {
			hi, hiKey = mid, uint64(k)+1
		}
	}
	return 0, false
}

// steamTableLookup2: entries hold only the low 16 bits of each account ID,
// so every possible high half is tried against every entry. Runs on the
// search's cores at lowest thread priority, like the brute force.
func steamTableLookup2(buf []byte, g *[16]byte, maxAccount uint64, workers int) (uint32, bool) {
	lows := make([]uint32, len(buf)/2)
	for i := range lows {
		lows[i] = uint32(binary.LittleEndian.Uint16(buf[i*2:]))
	}
	highs := uint32((maxAccount-1)>>16) + 1
	workers = max(1, min(workers, int(highs)))
	var (
		next   atomic.Uint32
		done   atomic.Bool
		result atomic.Uint32
		wg     sync.WaitGroup
	)
	want0 := binary.LittleEndian.Uint64(g[:8])
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // never unlocked: see searchSteamIDs
			lowerThreadPriority()
			var in [10]byte
			in[0], in[1] = 'B', 'E'
			for !done.Load() {
				hi := next.Add(1) - 1
				if hi >= highs {
					return
				}
				for _, lo := range lows {
					acc := uint64(hi)<<16 | uint64(lo)
					if acc >= maxAccount {
						continue
					}
					binary.LittleEndian.PutUint64(in[2:], steamID64Base+acc)
					s := md5.Sum(in[:])
					if binary.LittleEndian.Uint64(s[:8]) == want0 && s == *g {
						result.Store(uint32(acc))
						done.Store(true)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if !done.Load() {
		return 0, false
	}
	return result.Load(), true
}

// ---------------------------------------------------------------------------
// HTTP (registered in runWebServer, web.go)
// ---------------------------------------------------------------------------

// handlePlayerSteam serves GET /api/players/{guid}/steam[?refresh=1]:
//
//	{ok, enabled, steamId, pending}                 always (steamId "" and pending true until it's found)
//	{..., search: {state, percent, ...}}            while pending (steamSearchStatus)
//	{..., profileUrl}                               once the SteamID is known
//	{..., profile, friendsHere: [{guid, name}]}     when STEAM_API_KEY is set
//	{..., stale: "<error>"}                         Steam failed; profile is the last copy
func handlePlayerSteam(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	steamID, known, err := globalDatabase.SteamIDByGUID(guid)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	} else if !known {
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	}
	resp := map[string]any{"ok": true, "enabled": steamAPIKey != "", "steamId": steamID, "pending": steamID == ""}
	if steamID == "" {
		resp["search"] = steamSearchStatus(guid)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	id, valid := parseSteamID64(steamID)
	if !valid {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("the stored steam_id %q is not a SteamID64", steamID))
		return
	}
	if battlEyeGUID(id) != guid {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("the stored steam_id %s belongs to a different GUID (%s)", steamID, battlEyeGUID(id)))
		return
	}
	resp["profileUrl"] = "https://steamcommunity.com/profiles/" + steamID
	if steamAPIKey == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	p, err := steamProfileFor(ctx, steamID, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		log.Printf("steam: %s: %v", steamID, err)
		if p == nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		resp["stale"] = err.Error()
	}
	resp["profile"] = p
	here, err := globalDatabase.PlayersBySteamIDs(p.FriendIDs)
	if err != nil {
		log.Printf("steam: friends of %s on this server: %v", steamID, err)
		here = []sharedPlayer{}
	}
	resp["friendsHere"] = here
	writeJSON(w, http.StatusOK, resp)
}

// handleSteamSearch serves POST /api/players/{guid}/steam/search: starts
// looking for the player's SteamID (see startSteamSearch) and answers like
// GET /api/players/{guid}/steam, so the page can show the progress. A player
// who already has a SteamID gets 409.
func handleSteamSearch(w http.ResponseWriter, r *http.Request) {
	guid, ok := playerGUID(w, r)
	if !ok {
		return
	}
	steamID, known, err := globalDatabase.SteamIDByGUID(guid)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	case !known:
		fail(w, http.StatusNotFound, "unknown player (this GUID has never been seen on the server)")
		return
	case steamID != "":
		fail(w, http.StatusConflict, "this player's SteamID is already known")
		return
	}
	if err := startSteamSearch(guid); err != nil {
		log.Printf("steam: starting a search for %s: %v", guid, err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": steamAPIKey != "", "steamId": "",
		"pending": true, "search": steamSearchStatus(guid)})
}