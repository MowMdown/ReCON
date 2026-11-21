package livemap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapTiles(t *testing.T) {
	dir := t.TempDir()
	tiles := filepath.Join(dir, "maps", "namalsk")
	os.MkdirAll(filepath.Join(tiles, "3"), 0o755)
	os.WriteFile(filepath.Join(tiles, "tiles.json"), []byte(`{"px":4000,"tile":500,"levels":4,"ext":"jpg"}`), 0o644)
	os.WriteFile(filepath.Join(tiles, "3", "7_2.jpg"), []byte("jpeg 7_2"), 0o644)
	os.WriteFile(filepath.Join(tiles, "3", "notes.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "maps", "enoch"), 0o755)
	os.WriteFile(filepath.Join(dir, "maps", "enoch", "tiles.json"), []byte(`{"px":0}`), 0o644)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mod/map/{world}/tiles", mapTileList(dir))
	mux.HandleFunc("GET /api/mod/map/{world}/tile/{level}/{file}", mapTile(dir))
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}

	w := get("/api/mod/map/Namalsk/tiles")
	for _, want := range []string{`"px":4000`, `"tile":500`, `"levels":4`, `"ext":"jpg"`, `"version":"`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("tiles: %d %s, want %s", w.Code, w.Body, want)
		}
	}
	for _, world := range []string{"chernarusplus", "enoch"} {
		if get("/api/mod/map/"+world+"/tiles").Code != 404 {
			t.Errorf("%s (no tiles, or a bad tiles.json): not 404", world)
		}
	}
	if w := get("/api/mod/map/namalsk/tile/3/7_2.jpg?v=1"); w.Code != 200 || w.Body.String() != "jpeg 7_2" {
		t.Errorf("a tile: %d %q", w.Code, w.Body)
	}
	for _, bad := range []string{
		"/api/mod/map/namalsk/tile/3/2_2.jpg",   // not there
		"/api/mod/map/namalsk/tile/3/notes.txt", // not a tile
		"/api/mod/map/namalsk/tile/3/..%2F..%2Fp.db",
		"/api/mod/map/namalsk/tile/..%2F3/7_2.jpg",
		"/api/mod/map/namalsk/tile/x/7_2.jpg",
		"/api/mod/map/..%2Fx/tile/0/0_0.jpg",
	} {
		if c := get(bad).Code; c != 404 {
			t.Errorf("%s: %d, want 404", bad, c)
		}
	}
}
