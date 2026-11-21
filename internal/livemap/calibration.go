package livemap

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
)

// A map's picture can be calibrated: a point is where a player was (x, z,
// as the mod reported) and where on the picture they really were (u from
// its west edge, v from its north edge, each 0 to 1). The page works out
// from them where the picture lies: one point shifts it, two far apart set
// its size too. ReCON keeps them in maps/<world>/calibration.json, next to
// the map's tiles (satmap leaves it there when it slices the map again).

const calFile = "calibration.json"

// CalPoint is one calibration point.
type CalPoint struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
	U float64 `json:"u"`
	V float64 `json:"v"`
}

const maxCalPoints = 2

// calibration answers GET /api/mod/map/{world}/calibration: {"points": [...]},
// none when the map isn't calibrated.
func calibration(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, ok := tileDir(dataDir, r.PathValue("world"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "points": loadCalibration(dir)})
	}
}

// setCalibration answers POST /api/mod/map/{world}/calibration
// {"points": [...]}: the map's points from now on (none: not calibrated).
func setCalibration(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, ok := tileDir(dataDir, r.PathValue("world"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Points []CalPoint `json:"points"`
		}
		if err := decodeBody(w, r, &body, 4096); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON: "+err.Error())
			return
		}
		if err := checkCalibration(body.Points); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := saveCalibration(dir, body.Points); err != nil {
			fail(w, http.StatusInternalServerError, "saving the calibration: "+err.Error()+" (is ReCON's maps folder writable?)")
			return
		}
		if body.Points == nil {
			body.Points = []CalPoint{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "points": body.Points})
	}
}

func checkCalibration(pts []CalPoint) error {
	if len(pts) > maxCalPoints {
		return errors.New("at most 2 points")
	}
	for _, p := range pts {
		for _, f := range []float64{p.X, p.Z, p.U, p.V} {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return errors.New("a point isn't a number")
			}
		}
		if math.Abs(p.X) > 1e6 || math.Abs(p.Z) > 1e6 || p.U < -1 || p.U > 2 || p.V < -1 || p.V > 2 {
			return errors.New("a point is off the map")
		}
	}
	return nil
}

// loadCalibration is the points in dir's calibration file, none if there
// isn't one (or it can't be read).
func loadCalibration(dir string) []CalPoint {
	var pts []CalPoint
	b, err := os.ReadFile(filepath.Join(dir, calFile))
	if err != nil || json.Unmarshal(b, &pts) != nil || pts == nil || checkCalibration(pts) != nil {
		return []CalPoint{}
	}
	return pts
}

// saveCalibration writes the points to dir's calibration file (making the
// folder if need be), or removes it when there are none.
func saveCalibration(dir string, pts []CalPoint) error {
	p := filepath.Join(dir, calFile)
	if len(pts) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	js, _ := json.MarshalIndent(pts, "", "\t")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(js, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
