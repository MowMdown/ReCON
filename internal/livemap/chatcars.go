package livemap

// chatcars.go is each player's chat car: the one vehicle their last !car,
// !truck, !boat or !vehicle spawned. The next one replaces it: the spawn
// names it (Command.Replace) and the mod deletes it once the new one exists
// (crew put out first), then reports the new one's persistent ID
// (Result.Entity), which is kept here. Vehicles from the Live Map aren't.

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const chatCarsSchema = `
CREATE TABLE IF NOT EXISTS chat_vehicles (
	guid       TEXT PRIMARY KEY,
	entity     TEXT NOT NULL,
	type       TEXT NOT NULL,
	spawned_at INTEGER NOT NULL
)`

func openChatCars(d *sql.DB) error {
	_, err := d.Exec(chatCarsSchema)
	return err
}

// ChatVehicleOf is the persistent ID (b1:b2:b3:b4) of the player's chat car.
func ChatVehicleOf(guid string) (entity string, ok bool) {
	entity, _, ok = chatCarOf(guid)
	return entity, ok
}

// chatCarOf is the player's chat car: its persistent ID and its class.
func chatCarOf(guid string) (entity, class string, ok bool) {
	if db == nil {
		return "", "", false
	}
	err := db.QueryRow(`SELECT entity, type FROM chat_vehicles WHERE guid = ?`, strings.ToLower(guid)).Scan(&entity, &class)
	return entity, class, err == nil
}

func saveChatCar(guid, entity, class string, at time.Time) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`INSERT INTO chat_vehicles (guid, entity, type, spawned_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(guid) DO UPDATE SET entity = excluded.entity, type = excluded.type, spawned_at = excluded.spawned_at`,
		strings.ToLower(guid), entity, class, at.Unix())
	return err
}

// SpawnChatVehicle queues a whole vehicle of class in front of the player
// (guid) that replaces their chat car, and is tracked as the new one once
// the mod reports it.
func SpawnChatVehicle(guid, class string, now time.Time) (CommandStatus, error) {
	old, oldClass, _ := chatCarOf(guid)
	c, err := Queue(guid, Command{Action: "spawn", Text: class, Value: 1, Replace: old, ReplaceType: oldClass}, now)
	if err != nil {
		return c, err
	}
	state.Lock()
	for _, q := range state.commands {
		if q.ID == c.ID {
			q.ChatCar = true
			c = *q
		}
	}
	state.Unlock()
	return c, nil
}

// chatCarResult is a chat car the mod reported, to keep (after the lock).
type chatCarResult struct{ guid, entity, class string }

var oldModOnce sync.Once

// keepChatCars stores what the results reported; a chat spawn that worked
// without an entity is a mod before 0.0.1m, said once.
func keepChatCars(found []chatCarResult, untracked bool, now time.Time) {
	for _, f := range found {
		if err := saveChatCar(f.guid, f.entity, f.class, now); err != nil {
			log.Printf("recon-mod: keeping %s's chat vehicle: %v", f.guid, err)
		}
	}
	if untracked {
		oldModOnce.Do(func() {
			log.Printf("recon-mod: tracking chat vehicles needs recon-mod 0.0.1m (vehicles spawn, but the next one won't replace them)")
		})
	}
}

// ErrNoChatVehicle is !remove's answer when the player has no chat car.
var ErrNoChatVehicle = errors.New("You don't have a spawned vehicle")

// RemoveChatVehicle queues the removal of the player's chat car (matched by
// persistent ID and class, its crew put out first) and returns its class.
// It is forgotten once the mod has removed it or says it's already gone.
func RemoveChatVehicle(guid string, now time.Time) (CommandStatus, string, error) {
	pid, class, ok := chatCarOf(guid)
	if !ok {
		return CommandStatus{}, "", ErrNoChatVehicle
	}
	c, err := QueueWorld(Command{Action: "removeEntity", Text: pid, Stat: class}, now)
	if err != nil {
		return c, class, err
	}
	state.Lock()
	for _, q := range state.commands {
		if q.ID == c.ID {
			q.RemoveFor = guid
			c = *q
		}
	}
	state.Unlock()
	return c, class, nil
}

func forgetChatCar(guid string) {
	if db == nil {
		return
	}
	if _, err := db.Exec(`DELETE FROM chat_vehicles WHERE guid = ?`, strings.ToLower(guid)); err != nil {
		log.Printf("recon-mod: forgetting %s's chat vehicle: %v", guid, err)
	}
}

var oldRemoveOnce sync.Once
