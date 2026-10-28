package game

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/store"
)

// BanResult is what BanGUID did, as the API reports it.
type BanResult struct {
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
func BanGUID(p battleye.Player, online bool, minutes int, reason, logAs string) BanResult {
	BanListLock.Lock()
	defer BanListLock.Unlock()
	res := BanResult{Online: online, Perm: minutes == 0, Code: http.StatusOK}
	placed := time.Now()
	_, guidAck, guidErr := Run(fmt.Sprintf("addBan %s %d %s", p.GUID, minutes, reason), 1500*time.Millisecond, false, logAs)
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
	RefreshBanExpiry()
	if online {
		_, kickAck, kickErr := Run(fmt.Sprintf("kick %d %s", p.ID, reason), 1500*time.Millisecond, false, logAs)
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

// BanListLock serialises everything that changes or mirrors the ban list
// (ban, unban, and BattlEye->DB sync) so a sync can't act on a stale
// list and delete a ban that was just added.
var BanListLock sync.Mutex

// BanSyncEvery is how often the server watch (package watch) reads BattlEye's ban
// list. It also reads it after every RCON login, so after a server restart.
const BanSyncEvery = 5 * time.Minute

// SyncBans reads BattlEye's ban list and brings the bans table in line
// with it (SyncBansFromServer), then puts back any permanent ban the server has lost
// (`addBan <guid> 0 <reason>`) and saves the list with writeBans. Holds
// ListLock so it can't interleave with a ban or unban.
func SyncBans() error {
	BanListLock.Lock()
	defer BanListLock.Unlock()
	readAt := time.Now()
	bans, err := ReadBanList()
	if err != nil {
		return err
	}
	readd, err := database.SyncBansFromServer(bans, readAt)
	RefreshBanExpiry()
	if err != nil {
		return err
	}
	added := 0
	for _, b := range readd {
		cmd := strings.TrimSpace(fmt.Sprintf("addBan %s 0 %s", b.GUID, battleye.CleanReason(b.Reason)))
		_, acked, err := Run(cmd, 1500*time.Millisecond, false, "bans")
		what := "re-adding permanent ban"
		if importedPending[b.GUID] {
			what = "adding imported ban"
		}
		if err != nil || !acked {
			log.Printf("bans: %s %s failed (acked=%v err=%v)", what, b.GUID, acked, err)
			continue
		}
		if importedPending[b.GUID] {
			log.Printf("bans: added imported ban %s to BattlEye's list", b.GUID)
			delete(importedPending, b.GUID)
		} else {
			log.Printf("bans: re-added permanent ban %s (missing from BattlEye's list)", b.GUID)
		}
		added++
	}
	if added > 0 {
		Run("writeBans", 1500*time.Millisecond, false, "bans") // persist bans.txt, best effort
	}
	return nil
}

// BanStatuses returns every banned GUID's status, from the bans table.
func BanStatuses() map[string]store.BanState {
	out, err := database.AllBanStates()
	if err != nil {
		log.Printf("bans: reading the bans table: %v", err)
		return map[string]store.BanState{}
	}
	return out
}

// BanStatusOf is one player's ban status (Banned false when not banned).
func BanStatusOf(guid string) store.BanState {
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

// RefreshBanExpiry re-reads nextBanExpiry after the bans table changed.
func RefreshBanExpiry() {
	next, err := database.NextBanExpiry()
	if err != nil {
		log.Printf("bans: reading the next expiry: %v", err)
		return
	}
	nextBanExpiry.Store(next)
}

// ExpireBans deletes timed bans whose time has run out, if one is due.
// BattlEye drops them by itself, and nothing ever adds one back.
func ExpireBans(now time.Time) {
	if next := nextBanExpiry.Load(); next == 0 || now.Unix() < next {
		return
	}
	BanListLock.Lock()
	defer BanListLock.Unlock()
	n, err := database.ExpireTimedBans(now)
	if err != nil {
		log.Printf("bans: deleting expired bans: %v", err)
		return
	}
	if n > 0 {
		log.Printf("bans: %d timed ban(s) ran out", n)
	}
	RefreshBanExpiry()
}

// ReadBanList reads the live GUID ban list from BattlEye (retrying read).
// An unrecognised reply is logged and returned as an error, not an empty list.
func ReadBanList() ([]battleye.Ban, error) {
	out, acked, err := Run("bans", 4*time.Second, true, "")
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

// Permanent bans move between servers as bans.json:
//
//	{"bans": [{"guid": "...", "name": "...", "ip": "...", "reason": "...", "steamId": "..."}]}
//
// Only permanent bans are exported or imported; timed bans belong to the
// server that placed them.

// BanFile is a bans.json file.
type BanFile struct {
	Bans []store.PermanentBan `json:"bans"`
}

// ExportBans returns the permanent bans to save: every one, or only the GUIDs
// in want when it isn't nil.
func ExportBans(want map[string]bool) (BanFile, error) {
	all, err := database.PermanentBans()
	if err != nil {
		return BanFile{}, err
	}
	out := BanFile{Bans: []store.PermanentBan{}}
	for _, b := range all {
		if want == nil || want[b.GUID] {
			out.Bans = append(out.Bans, b)
		}
	}
	return out, nil
}

// BanImportResult is what Import did.
type BanImportResult struct {
	Added    int      `json:"added"`
	Skipped  []string `json:"skipped"`  // "Name (guid): why", one per ban not added
	OnServer bool     `json:"onServer"` // BattlEye's list was updated now (else at ReCON's next login)
	Kicked   int      `json:"kicked"`   // imported players who were online and were kicked
}

// importedPending is the imported bans not on BattlEye's list yet, so
// SyncWithServer logs them as added rather than re-added. Guarded by
// ListLock.
var importedPending = map[string]bool{}

// importReason is the reason given to an imported ban that has none.
const importReason = "Imported ban"

// ImportBans adds the permanent bans in f that aren't banned permanently already.
// They are recorded first, then put on BattlEye's list the way a permanent
// ban the server lost is (SyncWithServer: addBan, then writeBans); while the
// server isn't answering, that happens at ReCON's next login. Imported
// players who are online are kicked with the ban's reason.
func ImportBans(f BanFile) BanImportResult {
	res := BanImportResult{Skipped: []string{}}
	var added []store.PermanentBan
	BanListLock.Lock()
	for _, b := range f.Bans {
		b.GUID = strings.ToLower(strings.TrimSpace(b.GUID))
		b.Name = battleye.CleanReason(b.Name)
		if b.Name == "" {
			b.Name = "Unknown"
		}
		label := fmt.Sprintf("%s (%s)", b.Name, short(b.GUID))
		if !battleye.GUIDPattern.MatchString(b.GUID) {
			res.Skipped = append(res.Skipped, label+": not a BattlEye GUID")
			continue
		}
		if b.Reason = battleye.CleanReason(b.Reason); b.Reason == "" {
			b.Reason = importReason
		}
		b.IP = strings.TrimSpace(b.IP)
		if !isSteamID64(b.SteamID) {
			b.SteamID = ""
		}
		ok, err := database.AddPermanentBan(b)
		switch {
		case err != nil:
			log.Printf("bans: importing %s: %v", b.GUID, err)
			res.Skipped = append(res.Skipped, label+": could not be saved")
		case !ok:
			res.Skipped = append(res.Skipped, label+": already banned permanently")
		default:
			added = append(added, b)
			importedPending[b.GUID] = true
		}
	}
	BanListLock.Unlock()
	res.Added = len(added)
	if len(added) == 0 {
		return res
	}
	log.Printf("bans: imported %d permanent ban(s)", len(added))
	if err := SyncBans(); err != nil {
		log.Printf("bans: imported bans go on the server's list at the next login: %v", err)
	} else {
		res.OnServer = true
	}
	for _, b := range added {
		p, online := PlayerByGUID(b.GUID)
		if !online {
			continue
		}
		_, acked, err := Run(fmt.Sprintf("kick %d %s", p.ID, b.Reason), 1500*time.Millisecond, false, "web")
		if err == nil && acked {
			res.Kicked++
		}
	}
	return res
}

// short is the start of a GUID, enough to tell players apart in a message.
func short(guid string) string {
	if len(guid) > 8 {
		return guid[:8]
	}
	return guid
}

// isSteamID64 reports whether s looks like a SteamID64 (17 digits).
func isSteamID64(s string) bool {
	if len(s) != 17 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
