package players

import (
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
)

var PollWait = 3 * time.Second

// PollNow reads `players` (not logged) and hands the list to the
// tracker (who is online, sessions) and the join flows of players who
// just finished loading (SetHooks). It reports whether the
// server answered.
func PollNow() bool {
	sentAt := time.Now()
	out, acked, err := rcon.Run("players", PollWait, false, "")
	if err != nil || !acked {
		return false
	}
	playersJoined(ApplyPoll(battleye.ParsePlayersReply(out), sentAt))
	if anyoneInLobby() {
		wakeLobbyWatcher() // someone was already loading
	}
	return true
}
