package livemap

// lastinv.go keeps each player's last known inventory in the database, so
// it outlives their logout and a restart (the Developer page shows it). The
// mod only sends a player's inventory while ReCON watches them (their
// dialog is open on the Live Map or the Players page), and with a kill, so
// "last known" is the newest of those.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
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

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(raw)
	if err := zw.Close(); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO last_inventory (guid, at, why, items, count) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (guid) DO UPDATE SET at = excluded.at, why = excluded.why, items = excluded.items, count = excluded.count`,
		guid, at.Unix(), why, buf.Bytes(), countItems(items))
	if err != nil {
		lastInvSent.Lock()
		delete(lastInvSent.m, guid) // try again next time
		lastInvSent.Unlock()
	}
	return err
}

// LastInv is a player's last known inventory: when, why ("watched": sent
// while their dialog was open; "death": what they died with), how many
// items (all levels) and the items.
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
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return inv, false, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return inv, false, err
	}
	if err := json.Unmarshal(raw, &inv.Items); err != nil {
		return inv, false, err
	}
	inv.At = time.Unix(at, 0)
	return inv, true, nil
}
