package livemap

import (
	"testing"
	"time"
)

// The inventory the mod sends while a player is watched is kept as their
// last known one after they leave, and a death replaces it.
func TestLastInventoryOutlivesLogout(t *testing.T) {
	setup(t)
	openTestDB(t)
	now := time.Now().Truncate(time.Second)
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true}
	if _, ok, err := LastInventory(guid(steamA)); ok || err != nil {
		t.Fatalf("one kept before any was sent (err %v)", err)
	}

	vest := Item{ID: 7, Type: "PlateCarrierVest", Cargo: []Item{{ID: 8, Type: "Mag_STANAG_30Rnd", Quantity: 30}}}
	a.Inv, a.Inventory = true, []Item{vest}
	Sync(syncReq(a), now)
	Sync(syncReq(), now.Add(time.Second)) // they leave
	if _, ok := InventoryOf(guid(steamA), now.Add(time.Second)); ok {
		t.Fatal("the live inventory stayed after they left")
	}
	inv, ok, err := LastInventory(guid(steamA))
	if err != nil || !ok || inv.Why != "watched" || inv.Count != 2 || !inv.At.Equal(now) || inv.Items[0].Cargo[0].Quantity != 30 {
		t.Fatalf("last inventory %+v, %v, %v", inv, ok, err)
	}

	// The same inventory again isn't rewritten (its time stays).
	Sync(syncReq(a), now.Add(2*time.Second))
	if inv, _, _ := LastInventory(guid(steamA)); !inv.At.Equal(now) {
		t.Fatalf("rewritten at %v", inv.At)
	}

	if err := saveLastInventory(guid(steamA), "death", []Item{{ID: 9, Type: "Apple"}}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if inv, _, _ := LastInventory(guid(steamA)); inv.Why != "death" || inv.Count != 1 || inv.Items[0].Type != "Apple" {
		t.Fatalf("after death %+v", inv)
	}
}
