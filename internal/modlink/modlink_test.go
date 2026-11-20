package modlink

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/test"
)

const (
	steamA = "76561198000000001"
	steamB = "76561198000000002"
)

func setup(t *testing.T) {
	t.Helper()
	reset()
	SetKeySource(func() string { return "secret" })
	SetDatabase(nil)
	t.Cleanup(func() { reset(); SetDatabase(nil) })
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
	SetKeySource(func() string { return "" })
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
	if len(players) != 1 || players[0].GUID != steam.BattlEyeGUID(76561198000000001) || players[0].Health != 80 {
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
	db := test.OpenDB(t)
	SetDatabase(db)
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
	kills, deaths, err := db.KillCounts(guid(steamA))
	if err != nil {
		t.Fatal(err)
	}
	if kills != 1 || deaths != 2 {
		t.Errorf("a: %d kills, %d deaths", kills, deaths)
	}
	recent, err := db.KillsOf(guid(steamA), 10)
	if err != nil || len(recent) != 3 || recent[0].Weapon != "ZmbM_Soldier" || recent[0].KillerGUID != "" {
		t.Errorf("recent %+v %v", recent, err)
	}
}
