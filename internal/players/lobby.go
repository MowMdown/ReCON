package players

import (
	"time"

	"github.com/MowMdown/ReCON/internal/rcon"
)

// lobbyPollEvery is how often `players` is polled while someone is loading,
// so join flows start soon after "(Lobby)" leaves their name.
var lobbyPollEvery = 3 * time.Second

// lobbyWake starts watchLobbyPlayers when someone connects.
var lobbyWake = make(chan struct{}, 1)

// wakeLobbyWatcher tells watchLobbyPlayers someone may be loading.
func wakeLobbyWatcher() {
	select {
	case lobbyWake <- struct{}{}:
	default: // already told
	}
}

// watchLobbyPlayers polls `players` every lobbyPollEvery for as long as
// anyone is still loading, then sleeps until the next connect.
func watchLobbyPlayers() {
	for range lobbyWake {
		for anyoneInLobby() {
			time.Sleep(lobbyPollEvery)
			if anyoneInLobby() && rcon.Status() != "unavailable" {
				PollNow()
			}
		}
	}
}

// anyoneInLobby reports whether a tracked player is still loading.
func anyoneInLobby() bool {
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		if t.lobby {
			return true
		}
	}
	return false
}
