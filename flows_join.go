package main

// ============================================================================
// flows_join.go: the Player join trigger
// ----------------------------------------------------------------------------
// A join flow runs once for each player who joins, as soon as they have
// finished loading in:
//
//	Player #3 Name (1.2.3.4:2304) connected   the join is queued (players.go)
//	`players` every 3 seconds                 while they are in the lobby
//	"(Lobby)" gone from their name            the join flows start, each
//	                                          after its delay
//
// Only players seen connecting count, so a player already on when ReCON
// starts (or logs in again) isn't greeted. The tracker (players.go) decides
// when a queued join has finished loading and hands it to startJoinFlows.
// ============================================================================

// startJoinFlows runs the join flows for each player who just joined.
func startJoinFlows(joined []Player) {
	for _, p := range joined {
		startPlayerFlows(triggerJoin, sourceJoin, &flowPlayer{Player: p, role: lookupPlayerRole(p.GUID)})
	}
}
