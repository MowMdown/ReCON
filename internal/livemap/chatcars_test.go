package livemap

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// chatCarSetup: the mod connected with a complete item list, player A online.
func chatCarSetup(t *testing.T) (time.Time, func(results ...Result) SyncReply) {
	setup(t)
	openTestDB(t)
	now := time.Now()
	a := PlayerState{SteamID: steamA, Name: "A", Alive: true}
	items := []CatalogItem{{"Hatchback_02", "Gunter 2", "vehicle"}}
	sync := func(results ...Result) SyncReply {
		r := syncReq(a)
		r.CatalogTotal, r.CatalogAt, r.Catalog = len(items), 0, items
		r.Results = results
		reply, _ := Sync(r, now)
		return reply
	}
	sync()
	return now, sync
}

func TestChatCarReplaceSent(t *testing.T) {
	now, sync := chatCarSetup(t)
	c, err := SpawnChatVehicle(guid(steamA), "Hatchback_02", now)
	if err != nil || c.Replace != "" {
		t.Fatalf("first: %+v %v", c, err)
	}
	if err := saveChatCar(guid(steamA), "1:2:3:4", "Hatchback_02", now); err != nil {
		t.Fatal(err)
	}
	sync()
	c, err = SpawnChatVehicle(guid(steamA), "Hatchback_02", now)
	if err != nil || c.Replace != "1:2:3:4" || !c.ChatCar {
		t.Errorf("second: %+v %v", c, err)
	}
	reply := sync()
	found := false
	for _, cmd := range reply.Commands {
		found = found || (cmd.ID == c.ID && cmd.Replace == "1:2:3:4")
	}
	if !found {
		t.Errorf("replace not sent to the mod: %+v", reply.Commands)
	}
}

func TestChatCarStoredOnResult(t *testing.T) {
	now, sync := chatCarSetup(t)
	c, _ := SpawnChatVehicle(guid(steamA), "Hatchback_02", now)
	sync()
	sync(Result{ID: c.ID, OK: true, Entity: "5:6:7:8"})
	if e, ok := ChatVehicleOf(guid(steamA)); !ok || e != "5:6:7:8" {
		t.Errorf("stored %q %v", e, ok)
	}
}

func TestChatCarKeptOnFailure(t *testing.T) {
	now, sync := chatCarSetup(t)
	if err := saveChatCar(guid(steamA), "1:2:3:4", "Hatchback_02", now); err != nil {
		t.Fatal(err)
	}
	c, _ := SpawnChatVehicle(guid(steamA), "Hatchback_02", now)
	sync()
	sync(Result{ID: c.ID, OK: false, Error: "the game couldn't create Hatchback_02"})
	if e, _ := ChatVehicleOf(guid(steamA)); e != "1:2:3:4" {
		t.Errorf("stored %q, want the old car kept", e)
	}
}

func TestChatCarOldMod(t *testing.T) {
	now, sync := chatCarSetup(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	c, _ := SpawnChatVehicle(guid(steamA), "Hatchback_02", now)
	sync()
	sync(Result{ID: c.ID, OK: true})
	if _, ok := ChatVehicleOf(guid(steamA)); ok {
		t.Error("stored a car without an entity")
	}
	if !strings.Contains(buf.String(), "tracking chat vehicles needs recon-mod") {
		t.Errorf("no log line: %q", buf.String())
	}
}

func TestLiveMapSpawnNotTracked(t *testing.T) {
	now, sync := chatCarSetup(t)
	c, err := Queue(guid(steamA), Command{Action: "spawn", Text: "Hatchback_02", Value: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	sync()
	sync(Result{ID: c.ID, OK: true, Entity: "5:6:7:8"})
	if _, ok := ChatVehicleOf(guid(steamA)); ok {
		t.Error("a Live Map spawn was tracked")
	}
}
