package livemap

import (
	"testing"
	"time"
)

const testEventsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<events>
	<event name="StaticHeliCrash"><active>1</active><children><child lootmax="15" type="Wreck_UH1Y"/></children></event>
	<event name="StaticSantaCrash"><active>0</active><children><child type="Wreck_SantasSleigh"/></children></event>
	<event name="StaticConvoys"><active>1</active><children/></event>
	<event name="StaticContaminatedArea"><active>1</active><children><child type="ContaminatedArea_Dynamic"/></children></event>
	<event name="StaticPlantFernA"><active>1</active><children><child type="Fern"/></children></event>
	<event name="ItemPlanks"><active>1</active><children><child type="PileOfWoodenPlanks"/></children></event>
</events>`

const testSpawnsXML = `<eventposdef>
	<event name="StaticHeliCrash"><pos x="100.5" z="200" a="0"/><pos x="300" z="400"/></event>
	<event name="StaticSantaCrash"><pos x="1" z="1"/></event>
	<event name="StaticConvoys"><zone smin="0" r="40"/><pos x="500" z="600" group="Convoy1"/></event>
	<event name="StaticContaminatedArea"><pos x="1000" z="1000"/></event>
	<event name="StaticPlantFernA"><pos x="5" z="5"/></event>
	<event name="ItemPlanks"><pos x="6" z="6"/></event>
</eventposdef>`

const testGroupsXML = `<eventgroupdef>
	<group name="Convoy1">
		<child type="Land_Wreck_Uaz" x="0" z="0"/>
		<child type="Land_Wreck_V3S_DE" x="10" z="0"/>
		<child type="Land_Wreck_Uaz" x="20" z="0"/>
	</group>
</eventgroupdef>`

func TestBuildSpots(t *testing.T) {
	list, checks := buildSpots(testEventsXML, testSpawnsXML, testGroupsXML)
	if len(list) != 4 {
		t.Fatalf("spots = %+v, want 2 heli, 1 convoy, 1 gas (Santa off, fern and planks left out)", list)
	}
	want := []struct {
		event, cat string
		x          float64
	}{{"StaticHeliCrash", "heli", 100.5}, {"StaticHeliCrash", "heli", 300}, {"StaticConvoys", "convoy", 500}, {"StaticContaminatedArea", "gas", 1000}}
	for i, w := range want {
		if list[i].Event != w.event || list[i].Category != w.cat || list[i].X != w.x || list[i].ID != i+1 {
			t.Errorf("spot %d = %+v, want %+v", i, list[i], w)
		}
	}
	if list[2].Name != "Convoy" || list[0].Name != "Heli crash" {
		t.Errorf("names: %q, %q", list[0].Name, list[2].Name)
	}
	if len(checks) != 3 {
		t.Fatalf("checks = %+v, want 3 (gas isn't checked)", checks)
	}
	if c := checks[2]; c.ID != 3 || c.Radius != groupSpotRadius || len(c.Types) != 2 || c.Types[0] != "Land_Wreck_Uaz" {
		t.Errorf("convoy check = %+v", c)
	}
	if c := checks[0]; c.Radius != spotRadius || len(c.Types) != 1 || c.Types[0] != "Wreck_UH1Y" {
		t.Errorf("heli check = %+v", c)
	}
}

func TestSpotName(t *testing.T) {
	for in, want := range map[string]string{"StaticWaterTruck": "Water truck", "StaticSomeNewThing": "Some New Thing", "StaticPoliceSituation": "Police checkpoint"} {
		if got := spotName(in); got != want {
			t.Errorf("spotName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModAtLeast(t *testing.T) {
	for _, c := range []struct {
		v, want string
		ok      bool
	}{{"1.0.3", "1.0.3", true}, {"1.0.2", "1.0.3", false}, {"1.0.10", "1.0.3", true}, {"1.1", "1.0.3", true}, {"1.0.3-dev", "1.0.3", true}, {"0.0.1m", "1.0.3", false}, {"", "1.0.3", false}} {
		if got := modAtLeast(c.v, c.want); got != c.ok {
			t.Errorf("modAtLeast(%q, %q) = %v", c.v, c.want, got)
		}
	}
}

func TestTakeSpots(t *testing.T) {
	resetSpots()
	defer resetSpots()
	now := time.Now()
	zero := 0
	req := SyncRequest{Mod: "1.0.3", World: "deerisle", SpotsHeld: &zero}
	// An older mod is never asked (the keys would break its JSON reader).
	if w, set, _ := takeSpots(SyncRequest{Mod: "1.0.2", World: "deerisle"}, now); w != nil || set {
		t.Fatalf("1.0.2 asked: %v %v", w, set)
	}
	w, _, _ := takeSpots(req, now)
	if len(w) != 1 || w[0] != "db/events.xml" {
		t.Fatalf("first ask = %v", w)
	}
	if w, _, _ := takeSpots(req, now.Add(time.Second)); w != nil {
		t.Fatalf("asked again at once: %v", w)
	}
	req.MissionFiles = []MissionFile{{Name: "db/events.xml", Found: true, Text: testEventsXML}}
	w, _, _ = takeSpots(req, now.Add(2*time.Second))
	if len(w) != 1 || w[0] != "cfgeventspawns.xml" {
		t.Fatalf("second ask = %v", w)
	}
	req.MissionFiles = []MissionFile{{Name: "cfgeventspawns.xml", Found: true, Text: testSpawnsXML}}
	w, _, _ = takeSpots(req, now.Add(3*time.Second))
	if len(w) != 1 || w[0] != "cfgeventgroups.xml" {
		t.Fatalf("third ask = %v", w)
	}
	req.MissionFiles = []MissionFile{{Name: "cfgeventgroups.xml", Found: false}}
	w, set, checks := takeSpots(req, now.Add(4*time.Second))
	if w != nil || !set || len(checks) != 2 {
		t.Fatalf("after the files: want=%v set=%v checks=%v (no groups file: the convoy has no classes)", w, set, checks)
	}
	held := 2
	req.MissionFiles, req.SpotsHeld = nil, &held
	req.Spots = []SpotState{{ID: 2, On: true}}
	if _, set, _ := takeSpots(req, now.Add(5*time.Second)); set {
		t.Fatal("spots sent again though the mod holds them")
	}
	spots.Lock()
	on := spots.on[2]
	spots.Unlock()
	if !on {
		t.Fatal("spot 2 not up")
	}
}

func TestMergeVehicles(t *testing.T) {
	all := []Vehicle{{ID: 1, X: 1}, {ID: 2, X: 2}}
	out := mergeVehicles(all, []Vehicle{{ID: 2, X: 20}, {ID: 3, X: 3}})
	if len(out) != 3 || out[1].X != 20 || out[2].ID != 3 || all[1].X != 2 {
		t.Fatalf("merge = %+v (all now %+v)", out, all)
	}
}

// A gas spot never seen up has no radius; once one there has been up, its
// radius is kept for while it is down; only a ContaminatedArea_Static is
// static.
func TestGasSpotRadiusAndStatic(t *testing.T) {
	resetSpots()
	defer resetSpots()
	gasRadii.Lock()
	oldRadii := gasRadii.m
	gasRadii.m = map[string]float64{}
	gasRadii.Unlock()
	defer func() { gasRadii.Lock(); gasRadii.m = oldRadii; gasRadii.Unlock() }()
	now := time.Now()
	spots.Lock()
	spots.world, spots.built = "chernarusplus", true
	spots.list = []EventSpot{{ID: 1, Event: "StaticContaminatedArea", Name: "Gas zone", Category: "gas", X: 1000, Z: 1000}}
	spots.on, spots.seen = map[int]bool{}, map[int]bool{}
	spots.Unlock()
	state.Lock()
	oldSync, oldEvents := state.lastSync, state.worldEvents
	state.lastSync = now
	state.worldEvents = []WorldEvent{}
	state.Unlock()
	defer func() { state.Lock(); state.lastSync, state.worldEvents = oldSync, oldEvents; state.Unlock() }()
	setEvents := func(es []WorldEvent) { state.Lock(); state.worldEvents = es; state.Unlock() }

	list, _ := EventSpots(now)
	if len(list) != 1 || list[0].On || list[0].Radius != 0 {
		t.Fatalf("never up: %+v, want off with no radius", list)
	}
	setEvents([]WorldEvent{{ID: 9, Type: "ContaminatedArea_Dynamic", Kind: "gas", X: 1040, Z: 990, Radius: 120},
		{ID: 10, Type: "ContaminatedArea_Static", Kind: "gas", X: 5000, Z: 5000, Radius: 80},
		{ID: 11, Type: "ContaminatedArea_Dynamic", Kind: "gas", X: 9000, Z: 9000, Radius: 100}})
	list, _ = EventSpots(now)
	if !list[0].On || list[0].Radius != 120 {
		t.Fatalf("up: %+v", list[0])
	}
	if len(list) != 3 || !list[1].Static || list[2].Static {
		t.Fatalf("static flags: %+v (want the _Static zone static, the stray dynamic one not)", list[1:])
	}
	// A new connection (a server restart) doesn't forget it.
	resetSpots()
	spots.Lock()
	spots.world, spots.built = "chernarusplus", true
	spots.list = []EventSpot{{ID: 1, Event: "StaticContaminatedArea", Name: "Gas zone", Category: "gas", X: 1000, Z: 1000}}
	spots.on, spots.seen = map[int]bool{}, map[int]bool{}
	spots.Unlock()
	setEvents([]WorldEvent{})
	list, _ = EventSpots(now)
	if list[0].On || list[0].Radius != 120 {
		t.Fatalf("down again: %+v, want the 120 m it had", list[0])
	}
}
