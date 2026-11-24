package livemap

// lastinv.go keeps each player's last known inventory in the database, so
// it outlives their logout and a restart (the Developer page and the
// player dialog show it). The mod sends a player's inventory while ReCON
// watches them (their dialog is open, or every 5 minutes: pollInventories)
// and with a kill, so "last known" is the newest of those.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const lastInvSchema = `
CREATE TABLE IF NOT EXISTS last_inventory (
	guid  TEXT PRIMARY KEY,
	at    INTEGER NOT NULL,
	why   TEXT NOT NULL,
	items BLOB NOT NULL,
	count INTEGER NOT NULL
)`

func openLastInv(d *sql.DB) error {
	_, err := d.Exec(lastInvSchema)
	return err
}

// lastInvSent is what was last written for each player (a hash of the
// items, and when), so an unchanged inventory isn't rewritten every sync.
var lastInvSent = struct {
	sync.Mutex
	m map[string]lastInvMark
}{m: map[string]lastInvMark{}}

type lastInvMark struct {
	sum [32]byte
	at  time.Time
}

// lastInvRefresh is how often an unchanged inventory's time is rewritten.
const lastInvRefresh = 5 * time.Minute

// saveLastInventory writes a player's inventory as their last known one;
// why is "watched" or "death".
func saveLastInventory(guid, why string, items []Item, at time.Time) error {
	if db == nil {
		return nil
	}
	guid = strings.ToLower(guid)
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte(why+"\n"), raw...))
	lastInvSent.Lock()
	prev, ok := lastInvSent.m[guid]
	if ok && prev.sum == sum && at.Sub(prev.at) < lastInvRefresh {
		lastInvSent.Unlock()
		return nil
	}
	lastInvSent.m[guid] = lastInvMark{sum, at}
	lastInvSent.Unlock()

	blob, err := gzipBytes(raw)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO last_inventory (guid, at, why, items, count) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (guid) DO UPDATE SET at = excluded.at, why = excluded.why, items = excluded.items, count = excluded.count`,
		guid, at.Unix(), why, blob, countItems(items))
	if err != nil {
		lastInvSent.Lock()
		delete(lastInvSent.m, guid) // try again next time
		lastInvSent.Unlock()
	}
	return err
}

// LastInv is a player's last known inventory: when, why ("watched": read
// while they were online; "death": what they died with), how many items
// (all levels) and the items.
type LastInv struct {
	At    time.Time `json:"at"`
	Why   string    `json:"why"`
	Count int       `json:"count"`
	Items []Item    `json:"items"`
}

// LastInventory is a player's last known inventory (ok false: none kept).
func LastInventory(guid string) (inv LastInv, ok bool, err error) {
	if db == nil {
		return inv, false, nil
	}
	var at int64
	var blob []byte
	err = db.QueryRow(`SELECT at, why, count, items FROM last_inventory WHERE guid = lower(?)`, guid).
		Scan(&at, &inv.Why, &inv.Count, &blob)
	if err == sql.ErrNoRows {
		return inv, false, nil
	}
	if err != nil {
		return inv, false, err
	}
	if inv.Items, err = unpackItems(blob); err != nil {
		return inv, false, err
	}
	inv.At = time.Unix(at, 0)
	return inv, true, nil
}

// invPollEvery is how old a player's last known inventory may get before
// the mod is asked for it again; pollPerSync how many are asked for in one
// sync (spread out, so a full server isn't read at once).
const (
	invPollEvery = 5 * time.Minute
	pollPerSync  = 2
	pollRetry    = 30 * time.Second
)

var invPolled = struct {
	sync.Mutex
	m map[string]time.Time // last asked, by GUID
}{m: map[string]time.Time{}}

// pollInventories picks the online players (guid -> SteamID) whose last
// known inventory is the oldest and over invPollEvery old, at most
// pollPerSync, not asked in the last pollRetry.
func pollInventories(online map[string]string, now time.Time) []string {
	if db == nil {
		return nil
	}
	type cand struct {
		guid, steam string
		at          time.Time
	}
	var cs []cand
	lastInvSent.Lock()
	invPolled.Lock()
	for g, steam := range online {
		at := lastInvSent.m[g].at // zero: not this run
		if now.Sub(at) < invPollEvery || now.Sub(invPolled.m[g]) < pollRetry {
			continue
		}
		cs = append(cs, cand{g, steam, at})
	}
	sort.Slice(cs, func(i, j int) bool {
		return cs[i].at.Before(cs[j].at) || (cs[i].at.Equal(cs[j].at) && cs[i].guid < cs[j].guid)
	})
	var out []string
	for _, c := range cs {
		if len(out) == pollPerSync {
			break
		}
		invPolled.m[c.guid] = now
		out = append(out, c.steam)
	}
	for g := range invPolled.m {
		if _, ok := online[g]; !ok {
			delete(invPolled.m, g)
		}
	}
	invPolled.Unlock()
	lastInvSent.Unlock()
	return out
}
