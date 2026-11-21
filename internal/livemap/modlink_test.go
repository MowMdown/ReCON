package livemap

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"database/sql"

	_ "modernc.org/sqlite"
)

const (
	steamA = "76561198000000001"
	steamB = "76561198000000002"
)

func setup(t *testing.T) {
	t.Helper()
	reset()
	keySource = func() string { return "secret" }
	db, saveSteamID = nil, nil
	t.Cleanup(func() { reset(); db, saveSteamID = nil, nil })
}

func syncReq(players ...PlayerState) SyncRequest {
	return SyncRequest{V: Protocol, Mod: "0.1.0", Key: "secret", World: "chernarusplus",
		Year: 2026, Month: 11, Day: 20, Hour: 14, Minute: 5, Players: players}
}

func guid(steamID string) string { return guidOf(steamID) }

func TestSyncRefusesWrongPassword(t *testing.T) {
	setup(t)
	req := syncReq()
	req.Key = "nope"
	reply, err := Sync(req, time.Now())
	if !errors.Is(err, ErrRefused) || reply.OK || reply.Error == "" {
		t.Fatalf("reply %+v, err %v", reply, err)
	}
	// No password set in ReCON: nothing is accepted.
	keySource = func() string { return "" }
	req.Key = ""
	if _, err := Sync(req, time.Now()); !errors.Is(err, ErrRefused) {
		t.Fatalf("empty password accepted: %v", err)
	}
	if st, _ := Current(time.Now()); st.Connected {
		t.Fatal("connected after refusals")
	}
}

func TestSyncRefusesOtherProtocol(t *testing.T) {
	setup(t)
	req := syncReq()
	req.V = 99
	if _, err := Sync(req, time.Now()); err == nil {
		t.Fatal("protocol 99 accepted")
	}
}

func TestSyncKeepsPlayersByGUID(t *testing.T) {
	setup(t)
	now := time.Now()
	_, err := Sync(syncReq(PlayerState{SteamID: steamA, Name: "bob", Health: 80, Agents: 1 | 4},
		PlayerState{SteamID: "not a number", Name: "x"}), now)
	if err != nil {
		t.Fatal(err)
	}
	st, players := Current(now.Add(time.Second))
	if !st.Connected || st.World != "chernarusplus" || st.Date != [5]int{2026, 11, 20, 14, 5} {
		t.Fatalf("status %+v", st)
	}
	if len(players) != 1 || players[0].GUID != battlEyeGUID(76561198000000001) || players[0].Health != 80 {
		t.Fatalf("players %+v", players)
	}
	if !reflect.DeepEqual(players[0].Illnesses, []string{"cholera", "salmonella"}) {
		t.Errorf("illnesses %v", players[0].Illnesses)
	}
	if _, ok := PlayerOf(players[0].GUID, now); !ok {
		t.Error("PlayerOf missed them")
	}
	// Once the mod stops syncing, the data is old.
	st, players = Current(now.Add(staleAfter + time.Second))
	if st.Connected || len(players) != 0 {
		t.Errorf("stale: %+v %v", st, players)
	}
	if _, err := Queue(guid(steamA), Command{Action: "heal"}, now.Add(staleAfter+time.Second)); err == nil {
		t.Error("queued while the mod is gone")
	}
}

func TestCommandRoundTrip(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(PlayerState{SteamID: steamA, Name: "a"}, PlayerState{SteamID: steamB, Name: "b"}), now)

	heal, err := Queue(guid(steamA), Command{Action: "heal"}, now)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Queue(guid(steamA), Command{Action: "teleportTo", Target: guid(steamB)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.Target != steamB || tp.SteamID != steamA {
		t.Errorf("teleportTo carries %q -> %q", tp.SteamID, tp.Target)
	}
	reply, _ := Sync(syncReq(), now.Add(time.Second))
	if len(reply.Commands) != 2 || reply.Commands[0].ID != heal.ID || reply.Commands[0].SteamID != steamA {
		t.Fatalf("commands %+v", reply.Commands)
	}
	// Sent once only.
	if reply, _ := Sync(syncReq(), now.Add(2*time.Second)); len(reply.Commands) != 0 {
		t.Fatalf("sent again: %+v", reply.Commands)
	}
	req := syncReq()
	req.Results = []Result{{ID: heal.ID, OK: true}, {ID: tp.ID, Error: "the player to go to isn't online"}}
	Sync(req, now.Add(3*time.Second))
	got := map[int]CommandStatus{}
	for _, c := range Commands(guid(steamA)) {
		got[c.ID] = c
	}
	if got[heal.ID].Status != "done" || got[tp.ID].Status != "failed" || got[tp.ID].Error == "" {
		t.Errorf("statuses %+v", got)
	}
}

func TestQueueChecks(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(PlayerState{SteamID: steamA}), now)
	g := guid(steamA)
	for _, c := range []Command{
		{Action: "dance"},
		{Action: "set", Stat: "mood", Value: 1},
		{Action: "set", Stat: "blood", Value: 6000},
		{Action: "teleport"},
		{Action: "message", Text: "  "},
		{Action: "teleportTo", Target: guid(steamB)},
	} {
		if _, err := Queue(g, c, now); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := Queue(guid(steamB), Command{Action: "heal"}, now); err == nil {
		t.Error("queued for a player who isn't online")
	}
	if _, err := Queue(g, Command{Action: "set", Stat: "blood", Value: 2500}, now); err != nil {
		t.Error(err)
	}
}

func TestCommandsExpire(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(PlayerState{SteamID: steamA}), now)
	c, _ := Queue(guid(steamA), Command{Action: "kill"}, now)
	// The mod went quiet, then came back after the timeout: the kill
	// isn't sent that late.
	reply, _ := Sync(syncReq(PlayerState{SteamID: steamA}), now.Add(commandTimeout+time.Second))
	if len(reply.Commands) != 0 {
		t.Fatalf("sent a stale command: %+v", reply.Commands)
	}
	if got := Commands(""); got[0].ID != c.ID || got[0].Status != "expired" {
		t.Errorf("commands %+v", got)
	}
}

func TestOnlyRecentResultsKept(t *testing.T) {
	setup(t)
	now := time.Now()
	Sync(syncReq(PlayerState{SteamID: steamA}), now)
	for i := 0; i < keepResults+10; i++ {
		Queue(guid(steamA), Command{Action: "heal"}, now)
	}
	Sync(syncReq(PlayerState{SteamID: steamA}), now)
	req := syncReq(PlayerState{SteamID: steamA})
	for i := 1; i <= keepResults+10; i++ {
		req.Results = append(req.Results, Result{ID: i, OK: true})
	}
	Sync(req, now)
	if n := len(Commands("")); n != keepResults {
		t.Errorf("%d commands kept", n)
	}
}

func TestKillsAndSteamIDsStored(t *testing.T) {
	setup(t)
	openTestDB(t)
	saved := map[string]string{}
	saveSteamID = func(guid, id string) error { saved[guid] = id; return nil }
	now := time.Now()
	req := syncReq(PlayerState{SteamID: steamA, Name: "a"})
	req.Events = []Event{
		{Type: "kill", Victim: steamB, VictimName: "b", Killer: steamA, KillerName: "a", Weapon: "M4A1", Distance: 120},
		{Type: "kill", Victim: steamA, VictimName: "a", Killer: steamA, KillerName: "a", Weapon: "Grenade"},
		{Type: "kill", Victim: steamA, VictimName: "a", Weapon: "ZmbM_Soldier"},
	}
	if _, err := Sync(req, now); err != nil {
		t.Fatal(err)
	}
	if saved[guid(steamA)] != steamA {
		t.Errorf("SteamIDs saved: %v", saved)
	}
	kills, deaths, err := killCounts(guid(steamA))
	if err != nil {
		t.Fatal(err)
	}
	if kills != 1 || deaths != 2 {
		t.Errorf("a: %d kills, %d deaths", kills, deaths)
	}
	recent, err := killsOf(guid(steamA), 10)
	if err != nil || len(recent) != 3 || recent[0].Weapon != "ZmbM_Soldier" || recent[0].KillerGUID != "" {
		t.Errorf("recent %+v %v", recent, err)
	}
}

func TestWorldCommands(t *testing.T) {
	setup(t)
	now := time.Now()
	req := syncReq()
	req.Weather = World{Overcast: 0.4, Rain: 0.1, Mode: "world"}
	Sync(req, now)
	if st, _ := Current(now); st.Weather.Overcast != 0.4 || st.Weather.Mode != "world" {
		t.Fatalf("weather %+v", st.Weather)
	}
	for _, c := range []Command{
		{Action: "weather", Stat: "overcast", Value: 0.9, Transition: 60, Hold: 1800},
		{Action: "weather", Stat: "windDir", Value: 270},
		{Action: "weatherMode", Text: "static"},
		{Action: "storm", Value: 1, Min: 0.7, Hold: 25},
		{Action: "rainThresholds", Min: 0, Max: 1, Transition: 30},
		{Action: "date", Year: 2026, Month: 2, Day: 28, Hour: 23, Minute: 59},
		{Action: "freezeTime", Value: 1},
	} {
		if _, err := QueueWorld(c, now); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Command{
		{Action: "heal"},
		{Action: "weather", Stat: "hail", Value: 1},
		{Action: "weather", Stat: "rain", Value: 2},
		{Action: "weather", Stat: "fog", Value: 0.5, Transition: -1},
		{Action: "weatherMode", Text: "sunny"},
		{Action: "storm", Value: 1, Min: 0.5},
		{Action: "rainThresholds", Min: 0.8, Max: 0.2},
		{Action: "date", Year: 2026, Month: 2, Day: 30, Hour: 12},
		{Action: "date", Year: 2026, Month: 1, Day: 1, Hour: 24},
		{Action: "freezeTime", Value: 2},
	} {
		if _, err := QueueWorld(c, now); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	reply, _ := Sync(syncReq(), now.Add(time.Second))
	if len(reply.Commands) != 7 || reply.Commands[0].Action != "weather" || reply.Commands[0].Hold != 1800 {
		t.Fatalf("commands %+v", reply.Commands)
	}
	if _, err := QueueWorld(Command{Action: "freezeTime"}, now.Add(time.Hour)); err == nil {
		t.Error("queued while the mod is gone")
	}
}

// openTestDB gives the package a fresh in-memory database.
func openTestDB(t *testing.T) {
	t.Helper()
	d, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	if err := openKills(d); err != nil {
		t.Fatal(err)
	}
}

// md5("BE" + the SteamID64 as 8 little-endian bytes), worked out apart
// from the code (Python's hashlib).
func TestBattlEyeGUID(t *testing.T) {
	if g := battlEyeGUID(76561197960265728); g != "4fc867abf98b934e9e7eeaf15170258c" {
		t.Errorf("guid %q", g)
	}
}

func TestInventoryOnlyWhileWatched(t *testing.T) {
	setup(t)
	now := time.Now()
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true}
	b := PlayerState{SteamID: steamB, Name: "B", Alive: true}
	reply, err := Sync(syncReq(a, b), now)
	if err != nil || len(reply.Watch) != 0 {
		t.Fatalf("watch %v before anyone looked, err %v", reply.Watch, err)
	}

	// A page opens A: the next reply asks for A's inventory, not B's.
	Watch(guid(steamA), now)
	reply, _ = Sync(syncReq(a, b), now.Add(time.Second))
	if !reflect.DeepEqual(reply.Watch, []string{steamA}) {
		t.Fatalf("watch %v, want [%s]", reply.Watch, steamA)
	}
	if _, ok := InventoryOf(guid(steamA), now.Add(time.Second)); ok {
		t.Fatal("an inventory before the mod sent one")
	}

	// The mod sends it; it is kept apart from the live state.
	vest := Item{ID: 7, Type: "PlateCarrierVest", Slot: "Vest", Row: -1, Col: -1,
		Cargo: []Item{{ID: 8, Type: "Mag_STANAG_30Rnd", Row: 0, Col: 1, Quantity: 30, QuantityMax: 30, Unit: "rounds"}}}
	a.Inv, a.Inventory = true, []Item{vest}
	at := now.Add(2 * time.Second)
	Sync(syncReq(a, b), at)
	inv, ok := InventoryOf(guid(steamA), at)
	if !ok || !inv.At.Equal(at) || len(inv.Items) != 1 || inv.Items[0].Cargo[0].Quantity != 30 {
		t.Fatalf("inventory %+v, %v", inv, ok)
	}
	if p, _ := PlayerOf(guid(steamA), at); p.Inv || p.Inventory != nil {
		t.Fatal("the inventory is in the live state too")
	}
	if _, players := Current(at); players[0].Inventory != nil {
		t.Fatal("the players list carries inventories")
	}

	// An empty inventory is still one.
	b.Inv = true
	Sync(syncReq(a, b), at)
	if inv, ok := InventoryOf(guid(steamB), at); !ok || inv.Items == nil || len(inv.Items) != 0 {
		t.Fatalf("empty inventory %+v, %v", inv, ok)
	}

	// Nobody looks for watchFor: the mod is told to stop.
	reply, _ = Sync(syncReq(a, b), now.Add(watchFor+time.Second))
	if len(reply.Watch) != 0 {
		t.Fatalf("still watching %v", reply.Watch)
	}

	// They leave: their inventory goes.
	Sync(syncReq(b), at)
	if _, ok := InventoryOf(guid(steamA), at); ok {
		t.Fatal("kept the inventory of a player who left")
	}
}

func TestTeleportBack(t *testing.T) {
	setup(t)
	now := time.Now()
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true, X: 1000, Y: 212.5, Z: 2000}
	Sync(syncReq(a), now)
	g := guid(steamA)
	if _, err := Queue(g, Command{Action: "back"}, now); err == nil {
		t.Fatal("back with no previous spot")
	}

	// Teleported away: the spot they left is their previous one, once done.
	cs, err := Queue(g, Command{Action: "teleport", X: 5000, Z: 6000}, now)
	if err != nil {
		t.Fatal(err)
	}
	Sync(syncReq(a), now)
	if _, ok := Previous(g); ok {
		t.Fatal("previous spot before the mod ran the teleport")
	}
	a.X, a.Y, a.Z = 5000, 90, 6000
	Sync(SyncRequest{V: Protocol, Key: "secret", Players: []PlayerState{a}, Results: []Result{{ID: cs.ID, OK: true}}}, now)
	prev, ok := Previous(g)
	if !ok || prev.X != 1000 || prev.Y != 212.5 || prev.Z != 2000 {
		t.Fatalf("previous %+v, %v", prev, ok)
	}

	// Back is a teleport to that exact spot, height too.
	back, err := Queue(g, Command{Action: "back"}, now)
	if err != nil || back.Action != "teleport" || !back.Back || back.X != 1000 || back.Y != 212.5 || back.Z != 2000 {
		t.Fatalf("back %+v, %v", back, err)
	}
	reply, _ := Sync(syncReq(a), now)
	if len(reply.Commands) != 1 || reply.Commands[0].Action != "teleport" || reply.Commands[0].Y != 212.5 {
		t.Fatalf("sent %+v", reply.Commands)
	}
	// Once there, back again returns them to where they were.
	Sync(SyncRequest{V: Protocol, Key: "secret", Players: []PlayerState{a}, Results: []Result{{ID: back.ID, OK: true}}}, now)
	if prev, _ := Previous(g); prev.X != 5000 || prev.Z != 6000 {
		t.Fatalf("previous after back %+v", prev)
	}

	// A failed teleport changes nothing.
	failed, _ := Queue(g, Command{Action: "teleport", X: 7000, Z: 7000}, now)
	Sync(syncReq(a), now)
	Sync(SyncRequest{V: Protocol, Key: "secret", Players: []PlayerState{a}, Results: []Result{{ID: failed.ID, OK: false, Error: "the player is dead"}}}, now)
	if prev, _ := Previous(g); prev.X != 5000 {
		t.Fatalf("a failed teleport moved the previous spot: %+v", prev)
	}
}

func TestCatalogPagesAndSpawn(t *testing.T) {
	setup(t)
	now := time.Now()
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true}
	all := []CatalogItem{{Type: "AKM", Name: "AKM", Kind: "weapon"}, {Type: "Apple", Name: "Apple", Kind: "item"},
		{Type: "Mag_AKM_30Rnd", Name: "$STR_untranslated", Kind: "magazine"}}
	req := func(at int, items ...CatalogItem) SyncRequest {
		r := syncReq(a)
		r.CatalogTotal, r.CatalogAt, r.Catalog = len(all), at, items
		return r
	}

	// The mod says how many; ReCON asks from the first.
	reply, _ := Sync(req(0), now)
	if reply.CatalogNext != 1 {
		t.Fatalf("next %d, want 1", reply.CatalogNext)
	}
	if _, err := Queue(guid(steamA), Command{Action: "spawn", Text: "AKM", Value: 1}, now); err == nil {
		t.Fatal("spawn before the list came in")
	}
	reply, _ = Sync(req(0, all[0], all[1]), now)
	if reply.CatalogNext != 3 {
		t.Fatalf("next %d after 2, want 3", reply.CatalogNext)
	}
	// A resend of a page already taken is ignored.
	reply, _ = Sync(req(0, all[0], all[1]), now)
	if reply.CatalogNext != 3 {
		t.Fatalf("next %d after a resend, want 3", reply.CatalogNext)
	}
	if _, complete := Catalog(now); complete {
		t.Fatal("complete with 2 of 3")
	}
	reply, _ = Sync(req(2, all[2]), now)
	items, complete := Catalog(now)
	if reply.CatalogNext != 0 || !complete || len(items) != 3 {
		t.Fatalf("next %d, complete %v, %d items", reply.CatalogNext, complete, len(items))
	}
	// Sorted by name; an untranslated name sorts by its class name.
	if items[0].Type != "AKM" || items[1].Type != "Apple" || items[2].Type != "Mag_AKM_30Rnd" {
		t.Fatalf("order %+v", items)
	}

	for _, bad := range []Command{{Text: "", Value: 1}, {Text: "AKM", Value: 0}, {Text: "AKM", Value: 21},
		{Text: "AKM", Value: 1.5}, {Text: "Hatchback_02", Value: 1}} {
		bad.Action = "spawn"
		if _, err := Queue(guid(steamA), bad, now); err == nil {
			t.Errorf("spawn %q x%v accepted", bad.Text, bad.Value)
		}
	}
	cs, err := Queue(guid(steamA), Command{Action: "spawn", Text: " Apple ", Value: 5}, now)
	if err != nil || cs.Text != "Apple" || cs.Value != 5 || cs.SteamID != steamA {
		t.Fatalf("spawn %+v, %v", cs, err)
	}

	// The server restarts with another mod list: it is fetched again.
	all = append(all, CatalogItem{Type: "Banana", Name: "Banana", Kind: "item"})
	if reply, _ := Sync(req(0), now.Add(time.Minute)); reply.CatalogNext != 1 {
		t.Fatalf("next %d after a reconnect, want 1", reply.CatalogNext)
	}
	if _, complete := Catalog(now.Add(time.Minute)); complete {
		t.Fatal("kept the old list")
	}
}
