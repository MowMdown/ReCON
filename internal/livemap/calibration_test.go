package livemap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCalibration(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mod/map/{world}/calibration", calibration(dir))
	mux.HandleFunc("POST /api/mod/map/{world}/calibration", setCalibration(dir))
	file := filepath.Join(dir, "maps", "deerisle", "calibration.json")
	do := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}

	if w := do("GET", "/api/mod/map/deerisle/calibration", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Fatalf("not calibrated: %d %s", w.Code, w.Body)
	}
	pts := `{"points":[{"x":6904,"z":1553,"u":0.4181,"v":0.9},{"x":12000,"z":9000,"u":0.73,"v":0.45}]}`
	if w := do("POST", "/api/mod/map/DeerIsle/calibration", pts); w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/api/mod/map/deerisle/calibration", ""); !strings.Contains(w.Body.String(), `{"x":12000,"z":9000,"u":0.73,"v":0.45}`) {
		t.Errorf("after set: %s", w.Body)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), `"x": 12000`) {
		t.Errorf("the file: %s %v", b, err)
	}
	if w := do("GET", "/api/mod/map/enoch/calibration", ""); !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Errorf("another map: %s", w.Body)
	}
	for _, bad := range []string{
		`{"points":[{"x":1,"z":1,"u":0,"v":0},{"x":2,"z":2,"u":0,"v":0},{"x":3,"z":3,"u":0,"v":0}]}`,
		`{"points":[{"x":1,"z":1,"u":5,"v":0}]}`,
		`{"points":`,
	} {
		if w := do("POST", "/api/mod/map/deerisle/calibration", bad); w.Code != 400 {
			t.Errorf("%s: %d, want 400", bad, w.Code)
		}
	}
	if w := do("POST", "/api/mod/map/..%2Fx/calibration", `{"points":[]}`); w.Code != 404 {
		t.Errorf("a bad world: %d", w.Code)
	}
	if w := do("POST", "/api/mod/map/deerisle/calibration", `{"points":[]}`); w.Code != 200 {
		t.Fatalf("reset: %d", w.Code)
	}
	if w := do("GET", "/api/mod/map/deerisle/calibration", ""); !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Errorf("after reset: %s", w.Body)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("reset left the file")
	}
	os.WriteFile(file, []byte("not json"), 0o644)
	if w := do("GET", "/api/mod/map/deerisle/calibration", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Errorf("a broken file: %d %s", w.Code, w.Body)
	}
}
