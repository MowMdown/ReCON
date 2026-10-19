package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
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
// until the GUID matches. Older accounts turn up within seconds; a full pass
// takes a few minutes. It runs in the background, one pass at a time, and a
// pass looks for every GUID in players and bans that has no SteamID yet,
// since many GUIDs cost the same as one. Each SteamID is written to players
// and bans the moment it is found. A search asked for while a pass is running
// waits for the next pass. STEAM_SEARCH_CORES and STEAM_SEARCH_PRIORITY set
// how much CPU it uses (see steamSearchConfig; default 1 core, medium).
// STEAM_SEARCH_MODE=auto also starts a search when a player without a
// SteamID is first seen online (steamPlayersSeen); manual, the default, only
// searches when the button is pressed.
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
// Older accounts are found long before the worst case. Pausing is used
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

// searchSteamIDs hashes account IDs 0 .. limit-1 until every wanted GUID
// (lower-case hex) is found, calling onFound once per match, one call at a
// time. checked counts the IDs done so far, for progress. Returns GUID ->
// SteamID64 for everything found.
func searchSteamIDs(wanted []string, limit uint64, cfg steamSearchConfig, checked *atomic.Uint64, onFound func(guid string, steamID uint64)) map[string]uint64 {
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
		next      atomic.Uint64
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
				lo := next.Add(chunk) - chunk
				if lo >= limit {
					return
				}
				hi := min(lo+chunk, limit)
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
	found := searchSteamIDs(list, steamSearchRange, steamSearchCfg, &steamSearch.checked, func(guid string, id uint64) {
		if err := globalDatabase.SetSteamID(guid, strconv.FormatUint(id, 10)); err != nil {
			log.Printf("steam: found %s for %s but couldn't save it: %v", strconv.FormatUint(id, 10), guid, err)
			return
		}
		log.Printf("steam: %s is %d", guid, id)
	})

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
