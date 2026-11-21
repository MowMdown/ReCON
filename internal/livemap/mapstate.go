package livemap

// mapstate.go lets the Live Map show its map without the mod: the map the
// mod last reported is kept in the database (so it survives a restart), and
// the data folder's map pictures can be listed.

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const mapStateSchema = `
CREATE TABLE IF NOT EXISTS live_map_state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
)`

// openMapState creates the table if needed and reads the last map back.
func openMapState(d *sql.DB) error {
	if _, err := d.Exec(mapStateSchema); err != nil {
		return err
	}
	var w string
	if err := d.QueryRow(`SELECT value FROM live_map_state WHERE key = 'world'`).Scan(&w); err != nil && err != sql.ErrNoRows {
		return err
	}
	state.Lock()
	state.lastWorld = w
	state.Unlock()
	return nil
}

// saveLastWorld keeps the map the mod reported (lowercase).
func saveLastWorld(w string) {
	if db == nil {
		return
	}
	if _, err := db.Exec(`INSERT INTO live_map_state (key, value) VALUES ('world', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, w); err != nil {
		log.Printf("recon-mod: keeping the map's name: %v", err)
	}
}

// mapsIn is the maps with a picture in dataDir's maps folder (a folder of
// tiles with tiles.json, or <world>.jpg|jpeg|png|webp), lowercase, sorted.
func mapsIn(dataDir string) []string {
	out := []string{}
	entries, err := os.ReadDir(filepath.Join(dataDir, "maps"))
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dataDir, "maps", name, "tiles.json")); err != nil {
				continue
			}
		} else {
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".webp" {
				continue
			}
			name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		if n := strings.ToLower(name); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// mapList serves GET /api/mod/maps: the maps with a picture in the data folder.
func mapList(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "maps": mapsIn(dataDir)})
	}
}
