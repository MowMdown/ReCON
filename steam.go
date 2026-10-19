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
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// steam.go overview
// ----------------------------------------------------------------------------
// The Details dialog's Steam Profile tab: a player's Steam account, looked
// up from the SteamID64 that guidsearch stores in players.steam_id. ReCON
// only reads that column; until guidsearch has filled it the tab says the
// SteamID is pending.
//
//	GET /api/players/{guid}/steam             the profile (cached for steamCacheFor)
//	GET /api/players/{guid}/steam?refresh=1   fetch now (at most every steamMinRefresh)
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
// HTTP (registered in runWebServer, web.go)
// ---------------------------------------------------------------------------

// handlePlayerSteam serves GET /api/players/{guid}/steam[?refresh=1]:
//
//	{ok, enabled, steamId, pending}                 always ("" + pending while guidsearch hasn't filled it)
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
