package steam

import (
	"strings"
	"time"
)

// DevBans is a cached Steam ban record, for the Developer page.
type DevBans struct {
	Bans
	From string `json:"from"` // "profile" (the Steam tab's fetch) or "bans" (GetPlayerBans)
}

// CachedBans is guid's Steam ban record if one is held and fresh. Unlike
// BanRecord it never asks Steam.
func CachedBans(guid string) (DevBans, bool) {
	if database == nil {
		return DevBans{}, false
	}
	steamID, _, err := database.SteamIDByGUID(guid)
	if err != nil || steamID == "" {
		return DevBans{}, false
	}
	b, from, ok := cachedBans(steamID)
	return DevBans{Bans: b, From: from}, ok
}

// Dev is everything this package holds about a player's GUID, read from
// memory only: nothing is fetched from Steam.
func Dev(guid string) map[string]any {
	g := strings.ToLower(guid)
	out := map[string]any{"apiKeySet": APIKey != "", "steamId": nil, "profile": nil, "bans": nil}
	steamID := ""
	if database != nil {
		if id, _, err := database.SteamIDByGUID(g); err == nil && id != "" {
			steamID = id
			out["steamId"] = id
		}
	}
	if steamID != "" {
		steamCache.Lock()
		if p := steamCache.byID[steamID]; p != nil {
			cp := *p
			out["profile"] = &cp
			out["profileFriendIds"] = append([]string{}, p.FriendIDs...)
			out["profileAgeSeconds"] = int(time.Since(time.Unix(p.FetchedUnix, 0)).Seconds())
		}
		steamCache.Unlock()
		banCache.Lock()
		if seen, ok := banCache.byID[steamID]; ok {
			out["bans"] = map[string]any{"vac": seen.bans.VAC, "game": seen.bans.Game, "daysSinceLast": seen.bans.DaysSinceLast, "at": seen.at}
		}
		banCache.Unlock()
	}
	out["cacheFor"] = steamCacheFor.String()
	out["search"] = SearchStatus(g)
	steamAutoSeen.Lock()
	out["autoSearchSeen"] = steamAutoSeen.m[g]
	steamAutoSeen.Unlock()
	steamAutoBatch.Lock()
	out["inAutoBatch"] = steamAutoBatch.guids[g]
	if steamAutoBatch.guids[g] {
		out["autoBatchDue"] = steamAutoBatch.due
	}
	steamAutoBatch.Unlock()
	return out
}
