package livemap

import (
	"reflect"
	"testing"
	"time"
)

func TestPlacedKept(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(), now)
	if _, sent := PlacedObjects(now); sent {
		t.Fatal("placed objects before the mod sent any")
	}
	r := syncReq()
	r.PlacedSent = true
	r.Placed = []Placed{{ID: 7, PID: "1:2:3:4", Type: "Barrel_Green", Kind: "storage", Name: "Barrel", X: 10, Z: 20, Health: 0.9, Placer: "MowMdown", PlacerID: "765", PlacedAt: 1700000000}}
	Sync(r, now)
	ps, sent := PlacedObjects(now)
	if !sent || len(ps) != 1 || ps[0].Kind != "storage" || ps[0].Placer != "MowMdown" {
		t.Errorf("%+v %v", ps, sent)
	}
	// A sync without them (the mod sends every 30 s) keeps the last list.
	Sync(syncReq(), now.Add(time.Second))
	if ps, _ := PlacedObjects(now.Add(time.Second)); len(ps) != 1 {
		t.Errorf("lost between sends: %+v", ps)
	}
	if _, sent := PlacedObjects(now.Add(time.Hour)); sent {
		t.Error("placed objects while the mod isn't connected")
	}
}

func TestPlacedDetailAsked(t *testing.T) {
	setup(t)
	now := time.Now()
	r := syncReq()
	r.PlacedSent = true
	r.Placed = []Placed{{ID: 7, PID: "1:2:3:4", Type: "Barrel_Green"}}
	Sync(r, now)
	AskPlaced(7, now)
	reply, _ := Sync(syncReq(), now)
	if len(reply.WatchPlaced) != 1 || reply.WatchPlaced[0] != 7 {
		t.Fatalf("not asked: %v", reply.WatchPlaced)
	}
	r = syncReq()
	r.PlacedDetails = []PlacedDetail{{ID: 7, Items: []Item{{Type: "Apple"}}}}
	Sync(r, now)
	p, d, have, ok := PlacedOf(7, now)
	if !ok || !have || p.Type != "Barrel_Green" || len(d.Items) != 1 {
		t.Errorf("%+v %+v %v %v", p, d, have, ok)
	}
}

func TestRemoveEntityValidated(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(), now)
	if _, err := QueueWorld(Command{Action: "removeEntity", Text: "1:2:3:4"}, now); err == nil {
		t.Error("queued without a class")
	}
	if _, err := QueueWorld(Command{Action: "removeEntity", Text: "1:2:3:4", Stat: "Barrel_Green"}, now); err != nil {
		t.Error(err)
	}
}

func TestPlacedDetailsBatch(t *testing.T) {
	setup(t)
	now := time.Now()
	r := syncReq()
	r.PlacedSent = true
	r.Placed = []Placed{{ID: 7, Type: "Barrel_Green"}, {ID: 8, Type: "WoodenCrate"}}
	Sync(r, now)
	if got := PlacedDetails([]int{7, 8}, true, now); len(got) != 0 {
		t.Fatalf("details before the mod sent any: %v", got)
	}
	reply, _ := Sync(syncReq(), now)
	if !reflect.DeepEqual(reply.WatchPlaced, []int{7, 8}) {
		t.Fatalf("asked for %v, want [7 8]", reply.WatchPlaced)
	}
	r = syncReq()
	r.PlacedDetails = []PlacedDetail{{ID: 7, Items: []Item{{Type: "Apple"}}}, {ID: 8}}
	Sync(r, now)
	got := PlacedDetails([]int{7, 8}, false, now)
	if len(got) != 2 || len(got[7].Items) != 1 || got[8].Items == nil {
		t.Fatalf("details %+v", got)
	}
	if got := PlacedDetails([]int{7, 8}, true, now); len(got) != 0 {
		t.Fatalf("asking again kept the old details: %v", got)
	}
}
