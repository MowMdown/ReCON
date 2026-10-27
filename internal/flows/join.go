package flows

import (
	"github.com/MowMdown/ReCON/internal/battleye"
)

// StartJoinFlows runs the join flows for each player who just joined.
func StartJoinFlows(joined []battleye.Player) {
	for _, p := range joined {
		startPlayerFlows(triggerJoin, sourceJoin, &flowPlayer{Player: p, role: lookupPlayerRole(p.GUID)})
	}
}
