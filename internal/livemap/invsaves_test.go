package livemap

import (
	"reflect"
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
