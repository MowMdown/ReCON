package bans

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// ListLock serialises everything that changes or mirrors the ban list
// (ban, unban, and BattlEye->DB sync) so a sync can't act on a stale
// list and delete a ban that was just added.
var ListLock sync.Mutex

// SyncEvery is how often the server watch (package watch) reads BattlEye's ban
// list. It also reads it after every RCON login, so after a server restart.
const SyncEvery = 5 * time.Minute

// SyncWithServer reads BattlEye's ban list and brings the bans table in line
// with it (SyncBansFromServer), then puts back any permanent ban the server has lost
// (`addBan <guid> 0 <reason>`) and saves the list with writeBans. Holds
// ListLock so it can't interleave with a ban or unban.
func SyncWithServer() error {
	ListLock.Lock()
	defer ListLock.Unlock()
	readAt := time.Now()
	bans, err := ReadServerList()
	if err != nil {
		return err
	}
	readd, err := database.SyncBansFromServer(bans, readAt)
	RefreshExpiry()
	if err != nil {
		return err
	}
	added := 0
	for _, b := range readd {
		cmd := strings.TrimSpace(fmt.Sprintf("addBan %s 0 %s", b.GUID, battleye.CleanReason(b.Reason)))
		_, acked, err := rcon.Run(cmd, 1500*time.Millisecond, false, "bans")
		if err != nil || !acked {
			log.Printf("bans: re-adding permanent ban %s failed (acked=%v err=%v)", b.GUID, acked, err)
			continue
		}
		log.Printf("bans: re-added permanent ban %s (missing from BattlEye's list)", b.GUID)
		added++
	}
	if added > 0 {
		rcon.Run("writeBans", 1500*time.Millisecond, false, "bans") // persist bans.txt, best effort
	}
	return nil
}

// AllStatuses returns every banned GUID's status, from the bans table.
func AllStatuses() map[string]store.BanState {
	out, err := database.AllBanStates()
	if err != nil {
		log.Printf("bans: reading the bans table: %v", err)
		return map[string]store.BanState{}
	}
	return out
}

// StatusOf is one player's ban status (Banned false when not banned).
func StatusOf(guid string) store.BanState {
	st, err := database.GetBanState(guid)
	if err != nil {
		log.Printf("bans: reading %s: %v", guid, err)
		return store.BanState{MinutesLeft: -1}
	}
	return st
}

// nextBanExpiry is when the next timed ban runs out (Unix seconds, 0 for
// none), so the server watch only deletes expired bans when one is due.
var nextBanExpiry atomic.Int64

// RefreshExpiry re-reads nextBanExpiry after the bans table changed.
func RefreshExpiry() {
	next, err := database.NextBanExpiry()
	if err != nil {
		log.Printf("bans: reading the next expiry: %v", err)
		return
	}
	nextBanExpiry.Store(next)
}

// ExpireDue deletes timed bans whose time has run out, if one is due.
// BattlEye drops them by itself, and nothing ever adds one back.
func ExpireDue(now time.Time) {
	if next := nextBanExpiry.Load(); next == 0 || now.Unix() < next {
		return
	}
	ListLock.Lock()
	defer ListLock.Unlock()
	n, err := database.ExpireTimedBans(now)
	if err != nil {
		log.Printf("bans: deleting expired bans: %v", err)
		return
	}
	if n > 0 {
		log.Printf("bans: %d timed ban(s) ran out", n)
	}
	RefreshExpiry()
}

// ReadServerList reads the live GUID ban list from BattlEye (retrying read).
// An unrecognised reply is logged and returned as an error, not an empty list.
func ReadServerList() ([]battleye.Ban, error) {
	out, acked, err := rcon.Run("bans", 4*time.Second, true, "")
	if err != nil {
		return nil, err
	}
	if !acked {
		return nil, fmt.Errorf("RCON unavailable")
	}
	bans, ok := battleye.ParseBansReply(out)
	if !ok {
		if len(out) > 500 {
			out = out[:500]
		}
		log.Printf("bans: unrecognised output: %q", out)
		return nil, fmt.Errorf("could not parse bans output")
	}
	return bans, nil
}
