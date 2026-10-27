package players

import (
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// ResetForTesting forgets every tracked and remembered player. For tests.
func ResetForTesting() {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.byID, tracker.ended = map[int]*trackedPlayer{}, map[string]time.Time{}
	tracker.away, tracker.recent, tracker.known = map[string]*trackedPlayer{}, map[string]*trackedPlayer{}, false
	tracker.downAt, tracker.graceUntil = time.Time{}, time.Time{}
}

// SetOnlineForTesting makes players the online list, as if the console log
// and a poll had reported them, without writing to the database. For tests.
func SetOnlineForTesting(players ...battleye.Player) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.byID, tracker.known = map[int]*trackedPlayer{}, true
	for _, p := range players {
		tracker.byID[p.ID] = &trackedPlayer{id: p.ID, name: battleye.BareName(p.Name), guid: p.GUID, ping: p.Ping, lobby: battleye.InLobby(p)}
	}
}

// TrackLineForTesting feeds one server message to the tracker as if it
// arrived at `at`. For tests.
func TrackLineForTesting(text string, at time.Time) {
	trackServerEvent(battleye.ParseServerMessage(text, at))
}

// TrackEventForTesting feeds one console log event to the tracker. For tests.
func TrackEventForTesting(ev battleye.ServerEvent) { trackServerEvent(ev) }
