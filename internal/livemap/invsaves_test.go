package livemap

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var deathItems = []Item{
	{ID: 1, Type: "AssaultBag_Black", Slot: "Back", Health: 100, HealthMax: 100,
		Cargo: []Item{{ID: 2, Type: "Apple", Row: 0, Col: 0, Health: 50, HealthMax: 50}}},
	{ID: 3, Type: "M4A1", Slot: "Shoulder", Attachments: []Item{{ID: 4, Type: "Mag_STANAG_30Rnd", Slot: "magazine", Quantity: 30, QuantityMax: 30}}},
}

func deathSync(t *testing.T, items []Item) (time.Time, func()) {
	setup(t)
	openTestDB(t)
	now := time.Now()
	return now, func() {
		r := syncReq()
		r.Events = []Event{{Type: "kill", Victim: steamA, VictimName: "A", Cause: "player", Inventory: items}}
		Sync(r, now)
	}
}

func TestDeathSaveAlways(t *testing.T) {
	_, die := deathSync(t, deathItems)
	die()
	saves := InventorySaves(guid(steamA))
	if len(saves) != 1 || saves[0].Reason != "death" || saves[0].KillID == 0 || saves[0].Items != 4 {
		t.Fatalf("%+v", saves)
	}
	die() // a second death, the same gear: saved again (each death is its own)
	if n := len(InventorySaves(guid(steamA))); n != 2 {
		t.Errorf("%d saves, want 2", n)
	}
}

func TestDeathWithoutInventoryNotSaved(t *testing.T) {
	_, die := deathSync(t, nil)
	die()
	if n := len(InventorySaves(guid(steamA))); n != 0 {
		t.Errorf("%d saves for a death the mod sent no inventory with", n)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	_, die := deathSync(t, deathItems)
	die()
	s, err := InventorySave(InventorySaves(guid(steamA))[0].ID)
	if err != nil || s.GUID != guid(steamA) || !reflect.DeepEqual(s.Items, deathItems) {
		t.Errorf("%+v %v", s, err)
	}
}

func TestSavesCleanup(t *testing.T) {
	now, die := deathSync(t, deathItems)
	die()
	cleanupSaves(now.Add(keepSaves - time.Minute))
	if n := len(InventorySaves(guid(steamA))); n != 1 {
		t.Fatalf("removed too early: %d", n)
	}
	cleanupSaves(now.Add(keepSaves + time.Minute))
	if n := len(InventorySaves(guid(steamA))); n != 0 {
		t.Errorf("kept past %v: %d", keepSaves, n)
	}
}

func TestRestorePrunesTree(t *testing.T) {
	// The bag (1) without its apple (2); the magazine (4) without its rifle (3).
	roots := pruneSelected(deathItems, map[int]bool{1: true, 4: true})
	if len(roots) != 2 || roots[0].ID != 1 || len(roots[0].Cargo) != 0 || roots[1].ID != 4 {
		t.Errorf("roots %+v", roots)
	}
	// The rifle with its magazine: one root carrying it.
	roots = pruneSelected(deathItems, map[int]bool{3: true, 4: true})
	if len(roots) != 1 || roots[0].ID != 3 || len(roots[0].Attachments) != 1 {
		t.Errorf("rifle %+v", roots)
	}
}

func TestRestoreRecords(t *testing.T) {
	now, die := deathSync(t, deathItems)
	die()
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true}
	Sync(syncReq(a), now)
	id := InventorySaves(guid(steamA))[0].ID
	c, err := RestoreSaved(id, []int{1, 2}, now)
	if err != nil || c.Action != "restore" || c.Text == "" {
		t.Fatalf("%+v %v", c, err)
	}
	if strings.Contains(c.Text, "null") {
		t.Errorf("the mod's JSON reader gets null for a list: %s", c.Text)
	}
	s, _ := InventorySave(id)
	if !reflect.DeepEqual(s.RestoredIDs, []int{1, 2}) || s.Restored != 2 {
		t.Errorf("restored %v", s.RestoredIDs)
	}
	RestoreSaved(id, []int{2, 3}, now)
	if s, _ := InventorySave(id); !reflect.DeepEqual(s.RestoredIDs, []int{1, 2, 3}) {
		t.Errorf("after a second restore %v", s.RestoredIDs)
	}
}

func TestRestoreNeedsSelectionAndPlayer(t *testing.T) {
	now, die := deathSync(t, deathItems)
	die()
	id := InventorySaves(guid(steamA))[0].ID
	if _, err := RestoreSaved(id, nil, now); err == nil {
		t.Error("restored nothing without an error")
	}
	if _, err := RestoreSaved(id, []int{1}, now); err == nil {
		t.Error("restored to a player who isn't online") // deathSync has nobody online
	}
}
