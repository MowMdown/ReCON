package livemap

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed templates
var templateFiles embed.FS

// Host is what ReCON gives the Live Map: its database, the RCON password
// the mod must send, and the hooks to add pages, routes and nav links.
// cmd/recon/livemap.go fills it in.
type Host struct {
	DB           *sql.DB
	RCONPassword func() string
	// SaveSteamID stores a SteamID64 for a GUID ReCON has no SteamID for.
	SaveSteamID func(guid, steamID string) error
	AddRoute    func(pattern string, h http.HandlerFunc)
	AddNavLink  func(key, label, href string)
	AddQuiet    func(func(*http.Request) bool)
	// Page serves the template name in files, parsed with ReCON's base.html.
	Page func(files fs.FS, name string) http.HandlerFunc
	// SetGameDate gives Server Info the in-game date ("2026/11/20/14/05").
	SetGameDate func(func() (string, bool))
	// DataDir is ReCON's data folder; a picture of a map in its maps
	// folder (maps/chernarusplus.jpg) is drawn under the Live Map's grid.
	DataDir string
}

// Start adds the Live Map page and recon-mod's API to ReCON. Call it once,
// before ReCON's web server starts.
func Start(h Host) error {
	if h.DB == nil || h.RCONPassword == nil || h.AddRoute == nil || h.Page == nil {
		return errors.New("recon-mod: ReCON didn't give the Live Map its database, RCON password, routes or pages")
	}
	if err := openKills(h.DB); err != nil {
		return fmt.Errorf("recon-mod: creating its table: %w", err)
	}
	keySource, saveSteamID = h.RCONPassword, h.SaveSteamID

	page, err := fs.Sub(templateFiles, "templates")
	if err != nil {
		return err
	}
	h.AddRoute("GET /live", h.Page(page, "live.html"))
	h.AddRoute("GET /api/mod/map/{world}", mapImage(h.DataDir))                    // maps/<world>.jpg|png|webp from the data folder
	h.AddRoute("GET /api/mod/map/{world}/tiles", mapTileList(h.DataDir))           // maps/<world>/tiles.json: the tiles' levels and size
	h.AddRoute("GET /api/mod/map/{world}/tile/{level}/{file}", mapTile(h.DataDir)) // one of them: maps/<world>/<level>/<column>_<row>.jpg
	h.AddRoute("GET /api/mod/map/{world}/calibration", calibration(h.DataDir))     // maps/<world>/calibration.json: {"points"}
	h.AddRoute("POST /api/mod/map/{world}/calibration", setCalibration(h.DataDir)) // {"points"}: set them (none: not calibrated)
	h.AddRoute("POST /api/mod/sync", handleModSync)                                // the mod's player data in, commands out
	h.AddRoute("GET /api/mod/vehicles", handleModVehicles)                         // every vehicle on the server
	h.AddRoute("GET /api/mod/worldevents", handleModWorldEvents)                   // helicopter crashes and toxic zones
	h.AddRoute("GET /api/mod/predators", handleModPredators)                       // bears, wolves and guard dogs
	h.AddRoute("GET /api/mod/places", handleModPlaces)                             // the map's place names
	h.AddRoute("GET /api/mod/deaths", handleModDeaths)                             // every death in the last ?hours=
	h.AddRoute("GET /api/mod/vehicles/{id}", handleModVehicle)                     // one, with its damage zones, parts and cargo
	h.AddRoute("GET /api/mod/items", handleModItems)                               // every item the server can spawn
	h.AddRoute("GET /api/mod/players", handleModPlayers)                           // every online player's live state, the weather
	h.AddRoute("GET /api/mod/players/{guid}", handleModPlayer)                     // one player's live state, kills, commands
	h.AddRoute("POST /api/mod/players/{guid}/{action}", handleModAction)           // heal | kill | teleport | teleportTo | back | set | message | spawn
	h.AddRoute("POST /api/mod/world/{action}", handleModWorld)                     // weather | weatherMode | storm | rainThresholds | date | freezeTime | deleteVehicle | spawnAt
	if h.AddNavLink != nil {
		h.AddNavLink("live", "Live Map", "/live")
	}
	if h.AddQuiet != nil {
		h.AddQuiet(func(r *http.Request) bool {
			p := r.URL.Path
			return (r.Method == http.MethodPost && p == "/api/mod/sync") || // every second
				(r.Method == http.MethodGet && (p == "/live" || strings.HasPrefix(p, "/api/mod/")))
		})
	}
	if h.SetGameDate != nil {
		h.SetGameDate(func() (string, bool) {
			st, _ := Current(time.Now())
			if !st.Connected || st.Date[0] <= 0 {
				return "", false
			}
			return fmt.Sprintf("%d/%d/%d/%02d/%02d", st.Date[0], st.Date[1], st.Date[2], st.Date[3], st.Date[4]), true
		})
	}
	return nil
}

// mapImage serves a picture of a map from dataDir/maps (404 when there is
// none: the page then draws the grid alone).
func mapImage(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		world := strings.ToLower(r.PathValue("world"))
		if dataDir == "" || world == "" || strings.ContainsAny(world, "/\\.") {
			http.NotFound(w, r)
			return
		}
		for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
			p := filepath.Join(dataDir, "maps", world+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				w.Header().Set("Cache-Control", "max-age=300")
				http.ServeFile(w, r, p)
				return
			}
		}
		http.NotFound(w, r)
	}
}
