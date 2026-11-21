package livemap

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A map's picture can be sliced into tiles (satmap does it,
// github.com/MowMdown/satmap): maps/<world>/<level>/<column>_<row>.jpg and
// maps/<world>/tiles.json. Level 0 is the whole map in one tile, each level
// after it twice the detail; the page draws only the tiles in view from
// the least detailed level that is sharp enough. ReCON only lists and
// serves them.

// tileInfo is maps/<world>/tiles.json, as satmap writes it.
type tileInfo struct {
	Px     int    `json:"px"`     // the picture's width and height
	Tile   int    `json:"tile"`   // a tile's width and height (those on the east and south edges can be narrower)
	Levels int    `json:"levels"` // levels 0 to Levels-1
	Ext    string `json:"ext"`    // the tiles' file extension
}

var tileFile = regexp.MustCompile(`^\d{1,4}_\d{1,4}\.(jpg|jpeg|png|webp)$`)
var tileLevel = regexp.MustCompile(`^\d{1,2}$`)

func tileDir(dataDir, world string) (string, bool) {
	world = strings.ToLower(world)
	if dataDir == "" || world == "" || strings.ContainsAny(world, `/\.`) {
		return "", false
	}
	return filepath.Join(dataDir, "maps", world), true
}

// mapTileList answers GET /api/mod/map/{world}/tiles: maps/<world>/tiles.json
// ("px", "tile", "levels", "ext") and a "version" for the tiles' URLs, 404
// when there are no tiles.
func mapTileList(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, ok := tileDir(dataDir, r.PathValue("world"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(dir, "tiles.json")
		b, err := os.ReadFile(p)
		st, err2 := os.Stat(p)
		var info tileInfo
		if err != nil || err2 != nil || json.Unmarshal(b, &info) != nil ||
			info.Px <= 0 || info.Tile <= 0 || info.Levels < 1 || info.Levels > 20 || !tileFile.MatchString("0_0."+info.Ext) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(map[string]any{"px": info.Px, "tile": info.Tile, "levels": info.Levels, "ext": info.Ext,
			"version": strconv.FormatInt(st.ModTime().Unix(), 36)})
	}
}

// mapTile serves one tile, GET /api/mod/map/{world}/tile/{level}/{file}.
func mapTile(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, ok := tileDir(dataDir, r.PathValue("world"))
		level, file := r.PathValue("level"), r.PathValue("file")
		if !ok || !tileLevel.MatchString(level) || !tileFile.MatchString(strings.ToLower(file)) {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(dir, level, file)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable") // the page adds ?v=<version>
		http.ServeFile(w, r, p)
	}
}
