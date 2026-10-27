package bans

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// Permanent bans move between servers as bans.json:
//
//	{"bans": [{"guid": "...", "name": "...", "ip": "...", "reason": "...", "steamId": "..."}]}
//
// Only permanent bans are exported or imported; timed bans belong to the
// server that placed them.

// File is a bans.json file.
type File struct {
	Bans []store.PermanentBan `json:"bans"`
}

// Export returns the permanent bans to save: every one, or only the GUIDs
// in want when it isn't nil.
func Export(want map[string]bool) (File, error) {
	all, err := database.PermanentBans()
	if err != nil {
		return File{}, err
	}
	out := File{Bans: []store.PermanentBan{}}
	for _, b := range all {
		if want == nil || want[b.GUID] {
			out.Bans = append(out.Bans, b)
		}
	}
	return out, nil
}

// ImportResult is what Import did.
type ImportResult struct {
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

// Import adds the permanent bans in f that aren't banned permanently already.
// They are recorded first, then put on BattlEye's list the way a permanent
// ban the server lost is (SyncWithServer: addBan, then writeBans); while the
// server isn't answering, that happens at ReCON's next login. Imported
// players who are online are kicked with the ban's reason.
func Import(f File) ImportResult {
	res := ImportResult{Skipped: []string{}}
	var added []store.PermanentBan
	ListLock.Lock()
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
	ListLock.Unlock()
	res.Added = len(added)
	if len(added) == 0 {
		return res
	}
	log.Printf("bans: imported %d permanent ban(s)", len(added))
	if err := SyncWithServer(); err != nil {
		log.Printf("bans: imported bans go on the server's list at the next login: %v", err)
	} else {
		res.OnServer = true
	}
	for _, b := range added {
		p, online := players.ByGUID(b.GUID)
		if !online {
			continue
		}
		_, acked, err := rcon.Run(fmt.Sprintf("kick %d %s", p.ID, b.Reason), 1500*time.Millisecond, false, "web")
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
