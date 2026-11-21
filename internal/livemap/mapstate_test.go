package livemap

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The map the mod last reported is kept, so the page can show its picture
// while the mod isn't connected, even after ReCON restarts.
func TestLastWorldRemembered(t *testing.T) {
	setup(t)
	openTestDB(t)
	now := time.Now()
	r := syncReq()
	r.World = "DeerIsle"
	Sync(r, now)
	reset() // ReCON restarts: nothing in memory
	if err := openMapState(db); err != nil {
		t.Fatal(err)
	}
	st, _ := Current(now)
	if st.Connected || st.World != "deerisle" {
		t.Errorf("after a restart: connected %v, world %q; want the last map, deerisle", st.Connected, st.World)
	}
}

func TestMapsInDataFolder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "maps", "deerisle", "tiles.json"), `{}`)
	writeFile(t, filepath.Join(dir, "maps", "ChernarusPlus.jpg"), "x")
	writeFile(t, filepath.Join(dir, "maps", "notes.txt"), "x")
	writeFile(t, filepath.Join(dir, "maps", "empty", "readme"), "x")
	if got := mapsIn(dir); !reflect.DeepEqual(got, []string{"chernarusplus", "deerisle"}) {
		t.Errorf("got %v", got)
	}
	if got := mapsIn(filepath.Join(dir, "none")); len(got) != 0 {
		t.Errorf("no folder: %v", got)
	}
}
