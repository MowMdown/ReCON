package main

import "strings"

// ============================================================================
// flows_message.go: the Player message trigger
// ----------------------------------------------------------------------------
// A message flow runs once for each chat line a player sends, as soon as the
// line arrives in the console log, with the player and {message} set. The
// built-in chat commands (flows.go) see the line first.
// ============================================================================

// startChatWatcher reacts to every chat line for as long as ReCON runs:
// built-in chat commands first, then the Player message flows.
//
// The sender is found by exact name among the players online (without the
// "(Lobby)" tag). If nobody online has that name, or more than one player
// does, the line is ignored; what they may do comes from their GUID's role.
func startChatWatcher() {
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind == eventChat {
				handleChatLine(ev.Name, ev.Message)
			}
		}
	}()
}

func handleChatLine(name, message string) {
	p, ok := onlineByUniqueName(name)
	if !ok || p.GUID == "" {
		return
	}
	role := lookupPlayerRole(p.GUID)
	handleChatCommand(p, role, message)
	startPlayerFlows(triggerChat, sourceChat, &flowPlayer{Player: p, role: role, message: strings.TrimSpace(message)})
}
