package flows

import (
	"strings"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
)

// StartChatWatcher reacts to every chat line for as long as ReCON runs:
// built-in chat commands first, then the Player message flows.
//
// The sender is found by exact name among the players online (without the
// "(Lobby)" tag). If nobody online has that name, or more than one player
// does, the line is ignored; what they may do comes from their GUID's role.
func StartChatWatcher() {
	events, _ := rcon.Events.Subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind == battleye.EventChat {
				handleChatLine(ev.Name, ev.Message)
			}
		}
	}()
}

func handleChatLine(name, message string) {
	p, ok := players.ByUniqueName(name)
	if !ok || p.GUID == "" {
		return
	}
	role := lookupPlayerRole(p.GUID)
	handleChatCommand(p, role, message)
	startPlayerFlows(triggerChat, sourceChat, &flowPlayer{Player: p, role: role, message: strings.TrimSpace(message)})
}
