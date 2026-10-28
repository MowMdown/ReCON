// Package steam finds players' SteamIDs and looks up their Steam profiles
// for the Details dialog's Steam Profile tab (from the SteamID64 stored in
// players.steam_id).
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
// soonest; a full pass takes several minutes. It runs in the background, one
// pass at a time, and a pass looks for every GUID in players and bans that
// has no SteamID yet, since many GUIDs cost the same as one. Each SteamID is
// written to players and bans the moment it is found. A search asked for
// while a pass is running waits for the next pass. STEAM_SEARCH_CORES and
// STEAM_SEARCH_PRIORITY set how much CPU it uses (see steamSearchConfig;
// default 1 core, medium).
//
// When a player with no SteamID joins (PlayerJoined), the steam2guid
// table, if there is one, is read for them at once, in either mode.
// STEAM_SEARCH_MODE only decides when hashing starts for the rest:
//
//	manual  (the default) when the button is pressed.
//	auto    by itself, as a batch: 60 seconds after the first join the
//	        table couldn't answer, each further join adds 30 seconds, and
//	        it starts at once when 3 minutes have passed or 15 players are
//	        waiting, so a busy restart costs one pass, not one per player
//	        (steamAutoBatch).
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
package steam

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
	"time"

	"github.com/MowMdown/ReCON/internal/store"
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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

const (
	dayzAppID       = 221100           // DayZ on Steam
	steamCacheFor   = 30 * time.Minute // how long a fetched profile is reused
	steamMinRefresh = 30 * time.Second // ?refresh=1 within this of the last fetch gets the cached copy
	steamID64Base   = uint64(76561197960265728)
)

var (
	steamAPIBase = "https://api.steampowered.com" // a var so tests can use a fake server
	APIKey       = strings.TrimSpace(os.Getenv("STEAM_API_KEY"))
	steamHTTP    = &http.Client{Timeout: 10 * time.Second}
)

func init() {
	if APIKey == "" {
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

// ParseID64 accepts a 17-digit SteamID64 of an individual account.
func ParseID64(s string) (uint64, bool) {
	if len(s) != 17 {
		return 0, false
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id <= steamID64Base || id-steamID64Base > 0xFFFFFFFF {
		return 0, false
	}
	return id, true
}

// BattlEyeGUID is the BattlEye GUID of a SteamID64: md5("BE" + the ID as 8
// little-endian bytes), in lower-case hex. Used to check that the stored
// SteamID really belongs to the player.
func BattlEyeGUID(id uint64) string {
	b := make([]byte, 10)
	b[0], b[1] = 'B', 'E'
	binary.LittleEndian.PutUint64(b[2:], id)
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

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

// callSteamAPI calls one Web API method and decodes the JSON reply into out. The
// key travels in the query string, so it is kept out of every error (net/http
// puts the whole URL into its errors).
func callSteamAPI(ctx context.Context, method string, q url.Values, out any) error {
	q.Set("key", APIKey)
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
	call(func() { sumErr = callSteamAPI(ctx, "/ISteamUser/GetPlayerSummaries/v2/", ids, &sums) })
	call(func() {
		banErr = callSteamAPI(ctx, "/ISteamUser/GetPlayerBans/v1/", url.Values{"steamids": {steamID}}, &bans)
	})
	call(func() {
		levelErr = callSteamAPI(ctx, "/IPlayerService/GetSteamLevel/v1/", url.Values{"steamid": {steamID}}, &level)
	})
	call(func() {
		gamesErr = callSteamAPI(ctx, "/IPlayerService/GetOwnedGames/v1/",
			url.Values{"steamid": {steamID}, "include_played_free_games": {"1"}}, &games)
	})
	call(func() {
		friendsErr = callSteamAPI(ctx, "/ISteamUser/GetFriendList/v1/",
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

var steamCache = struct {
	sync.Mutex
	byID map[string]*steamProfile
}{byID: map[string]*steamProfile{}}

// ProfileFor returns a profile younger than steamCacheFor, fetching it
// when there isn't one (or when refresh asks and the copy is older than
// steamMinRefresh). If the fetch fails but an older copy exists, that copy is
// returned along with the error.
func ProfileFor(ctx context.Context, steamID string, refresh bool) (*steamProfile, error) {
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

// database is the player database, set at startup (SetDatabase).
var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }
