package livemap

// invsaves.go keeps a copy of each player's inventory at their death (the
// mod sends it with the kill), for an admin to give chosen items back after
// a hacker or a griefer killed them. Copies are kept INVENTORY_KEEP_DAYS
// days (7 by default).

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const invSavesSchema = `
CREATE TABLE IF NOT EXISTS inventory_saves (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	guid     TEXT NOT NULL,
	at       INTEGER NOT NULL,
	reason   TEXT NOT NULL,
	kill_id  INTEGER NOT NULL DEFAULT 0,
	items    BLOB NOT NULL,
	count    INTEGER NOT NULL,
	restored TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS inventory_saves_guid ON inventory_saves (guid, at)`

// keepSaves is how long a copy is kept.
var keepSaves = time.Duration(envInt("INVENTORY_KEEP_DAYS", 7)) * 24 * time.Hour

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

func openInvSaves(d *sql.DB) error {
	_, err := d.Exec(invSavesSchema)
	return err
}

// SaveInfo is one saved copy, as the list shows it.
type SaveInfo struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"` // death
	KillID   int64     `json:"killId"`
	Items    int       `json:"items"`    // how many, all levels
	Restored int       `json:"restored"` // how many of them were given back
}

// Save is a copy with its items, and the item ids already given back.
type Save struct {
	SaveInfo
	GUID        string `json:"guid"`
	Items       []Item `json:"items"`
	RestoredIDs []int  `json:"restoredIds"`
}

func countItems(items []Item) int {
	n := 0
	for _, it := range items {
		n += 1 + countItems(it.Attachments) + countItems(it.Cargo)
	}
	return n
}

// saveDeath keeps the inventory a player died with.
func saveDeath(guid string, killID int64, items []Item, at time.Time) error {
	if db == nil || len(items) == 0 {
		return nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(items); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO inventory_saves (guid, at, reason, kill_id, items, count) VALUES (lower(?), ?, 'death', ?, ?, ?)`,
		guid, at.Unix(), killID, buf.Bytes(), countItems(items))
	return err
}

// InventorySaves is a player's saved copies, newest first.
func InventorySaves(guid string) []SaveInfo {
	out := []SaveInfo{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT id, at, reason, kill_id, count, restored FROM inventory_saves WHERE guid = lower(?) ORDER BY at DESC, id DESC`, guid)
	if err != nil {
		log.Printf("recon-mod: reading saved inventories: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s SaveInfo
		var at int64
		var restored string
		if err := rows.Scan(&s.ID, &at, &s.Reason, &s.KillID, &s.Items, &restored); err != nil {
			continue
		}
		s.At = time.Unix(at, 0)
		var ids []int
		json.Unmarshal([]byte(restored), &ids)
		s.Restored = len(ids)
		out = append(out, s)
	}
	return out
}

// ErrNoSave: no such saved copy (or it was cleaned up).
var ErrNoSave = errors.New("that saved inventory isn't kept any more")

// InventorySave is one copy with its items.
func InventorySave(id int64) (Save, error) {
	var s Save
	if db == nil {
		return s, ErrNoSave
	}
	var at int64
	var blob []byte
	var restored string
	err := db.QueryRow(`SELECT id, guid, at, reason, kill_id, count, items, restored FROM inventory_saves WHERE id = ?`, id).
		Scan(&s.ID, &s.GUID, &at, &s.Reason, &s.KillID, &s.SaveInfo.Items, &blob, &restored)
	if err == sql.ErrNoRows {
		return s, ErrNoSave
	}
	if err != nil {
		return s, err
	}
	s.At = time.Unix(at, 0)
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return s, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s.Items); err != nil {
		return s, err
	}
	json.Unmarshal([]byte(restored), &s.RestoredIDs)
	if s.RestoredIDs == nil {
		s.RestoredIDs = []int{}
	}
	s.Restored = len(s.RestoredIDs)
	return s, nil
}

// cleanupSaves removes copies older than keepSaves.
func cleanupSaves(now time.Time) {
	if db == nil {
		return
	}
	if _, err := db.Exec(`DELETE FROM inventory_saves WHERE at < ?`, now.Add(-keepSaves).Unix()); err != nil {
		log.Printf("recon-mod: removing old saved inventories: %v", err)
	}
}

// handleModSaves serves GET /api/mod/players/{guid}/saves.
func handleModSaves(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saves": InventorySaves(r.PathValue("guid"))})
}

// handleModSave serves GET /api/mod/saves/{id}.
func handleModSave(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s, err := InventorySave(id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "save": s})
}
