package livemap

// loadouts.go keeps each mod's and admin's loadout (!save), apart from the
// inventories saved at death: one per player, given back by !equip (their
// gear replaced, every item pristine).

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const loadoutSchema = `
CREATE TABLE IF NOT EXISTS loadouts (
	guid  TEXT PRIMARY KEY,
	at    INTEGER NOT NULL,
	items BLOB NOT NULL,
	count INTEGER NOT NULL
)`

func openLoadouts(d *sql.DB) error {
	_, err := d.Exec(loadoutSchema)
	return err
}

// ErrNoLoadout is !equip without a !save first.
var ErrNoLoadout = errors.New("no loadout saved")

// loadoutWait is how long !save waits for the mod to send the inventory.
var loadoutWait = 5 * time.Second

// SaveLoadout reads the player's inventory now (asking the mod for it) and
// keeps it as their loadout; it returns how many items (all levels).
func SaveLoadout(guid string) (int, error) {
	if db == nil {
		return 0, errors.New("no database")
	}
	guid = strings.ToLower(guid)
	start := time.Now()
	Watch(guid, start)
	var inv Inventory
	for {
		now := time.Now()
		if i, ok := InventoryOf(guid, now); ok && !i.At.Before(start) {
			inv = i
			break
		}
		if now.Sub(start) > loadoutWait {
			return 0, errors.New("recon-mod didn't send your inventory")
		}
		time.Sleep(200 * time.Millisecond)
	}
	blob, err := gzipJSON(inv.Items)
	if err != nil {
		return 0, err
	}
	n := countItems(inv.Items)
	_, err = db.Exec(`INSERT INTO loadouts (guid, at, items, count) VALUES (?, ?, ?, ?)
		ON CONFLICT (guid) DO UPDATE SET at = excluded.at, items = excluded.items, count = excluded.count`,
		guid, inv.At.Unix(), blob, n)
	return n, err
}

// Loadout is a player's saved loadout (ErrNoLoadout: none).
func Loadout(guid string) ([]Item, int, error) {
	if db == nil {
		return nil, 0, ErrNoLoadout
	}
	var blob []byte
	var n int
	err := db.QueryRow(`SELECT items, count FROM loadouts WHERE guid = lower(?)`, guid).Scan(&blob, &n)
	if err == sql.ErrNoRows {
		return nil, 0, ErrNoLoadout
	}
	if err != nil {
		return nil, 0, err
	}
	items, err := unpackItems(blob)
	return items, n, err
}

// EquipLoadout replaces the player's gear with their loadout, every item
// pristine; it returns how many items.
func EquipLoadout(guid string, now time.Time) (int, error) {
	items, n, err := Loadout(guid)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, errors.New("your saved loadout is empty")
	}
	if _, err := Queue(guid, Command{Action: "restore", Items: noNulls(items), Condition: "pristine", Clear: true}, now); err != nil {
		return 0, err
	}
	return n, nil
}
