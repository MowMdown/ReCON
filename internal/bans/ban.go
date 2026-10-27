package bans

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
)

// Result is what BanGUID did, as the API reports it.
type Result struct {
	OK        bool   `json:"ok"`
	GUIDBan   bool   `json:"guid_ban"`
	DBUpdated bool   `json:"db_updated"`
	Online    bool   `json:"online"`
	Kicked    bool   `json:"kicked"`
	Perm      bool   `json:"perm"`
	Error     string `json:"error,omitempty"`
	Code      int    `json:"-"`
}

// BanGUID bans player p by GUID. online says whether p is connected right
// now (p.ID is only meaningful then). The pages, flows and the !ban chat
// command all ban through it; logAs labels the rcon log lines.
//
//  1. `addBan <guid> <minutes> <reason>` (0 minutes = permanent).
//  2. Record GUID/name/IP/reason and when it runs out in SQLite (the IP is
//     informational, not banned). This comes before the kick so the player's
//     session is closed as "banned", not "kicked".
//  3. If online, kick with the same reason. A failed kick is only reported (Kicked).
func BanGUID(p battleye.Player, online bool, minutes int, reason, logAs string) Result {
	ListLock.Lock()
	defer ListLock.Unlock()
	res := Result{Online: online, Perm: minutes == 0, Code: http.StatusOK}
	placed := time.Now()
	_, guidAck, guidErr := rcon.Run(fmt.Sprintf("addBan %s %d %s", p.GUID, minutes, reason), 1500*time.Millisecond, false, logAs)
	if guidErr != nil {
		res.Error, res.Code = guidErr.Error(), http.StatusBadGateway
		return res
	}
	if !guidAck {
		res.Error, res.Code = "GUID ban was not acknowledged", http.StatusBadGateway
		return res
	}
	res.GUIDBan = true
	dbErr := database.RecordPlayerBan(p.GUID, battleye.BareName(p.Name), p.IP, reason, res.Perm, placed.Add(time.Duration(minutes)*time.Minute))
	RefreshExpiry()
	if online {
		_, kickAck, kickErr := rcon.Run(fmt.Sprintf("kick %d %s", p.ID, reason), 1500*time.Millisecond, false, logAs)
		res.Kicked = kickErr == nil && kickAck
		if !res.Kicked {
			log.Printf("ban: kick of player %d after ban not confirmed (err=%v)", p.ID, kickErr)
		}
	}
	if dbErr != nil {
		log.Printf("error recording ban in DB: %v", dbErr)
		res.Error, res.Code = "GUID ban succeeded but database update failed", http.StatusInternalServerError
		return res
	}
	res.DBUpdated, res.OK = true, true
	return res
}
