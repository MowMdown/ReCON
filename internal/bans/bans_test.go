package bans

import (
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

const timedGUID = "cccccccccccccccccccccccccccccccc"

// testDB opens a fresh database for this package with one permanent ban
// (test.GUID1) and one timed ban (timedGUID), and no RCON server to talk to.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	SetDatabase(db)
	oldAddr, oldPass := rcon.Target()
	rcon.CloseSession()
	t.Cleanup(func() { rcon.SetTarget(oldAddr, oldPass); rcon.CloseSession() })
	rcon.SetTarget("127.0.0.1:1", "")
	if err := db.RecordPlayerBan(test.GUID1, "Cheater", "1.1.1.1", "aimbot", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPlayerBan(timedGUID, "Spammer", "2.2.2.2", "spam", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestImportPermanentBans checks Import's rules: a GUID already banned
// permanently and a malformed one are skipped with a reason, a timed ban
// becomes permanent, missing names and reasons get defaults, and with the
// server not answering the bans wait for the next login.
func TestImportPermanentBans(t *testing.T) {
	db := testDB(t)
	res := Import(File{Bans: []store.PermanentBan{
		{GUID: test.GUID1, Name: "Cheater", Reason: "aimbot"},
		{GUID: strings.ToUpper(test.GUID2), Name: "", Reason: ""},
		{GUID: "not-a-guid", Name: "Broken"},
		{GUID: timedGUID, Name: "Spammer", Reason: "spam again", SteamID: "76561198000000001"},
	}})
	if res.Added != 2 || res.OnServer || res.Kicked != 0 {
		t.Fatalf("result = %+v, want 2 added, not on the server yet", res)
	}
	want := []string{"Cheater (01234567): already banned permanently", "Broken (not-a-gu): not a BattlEye GUID"}
	if strings.Join(res.Skipped, "|") != strings.Join(want, "|") {
		t.Fatalf("skipped = %q, want %q", res.Skipped, want)
	}
	if st, _ := db.GetBanState(test.GUID2); !st.Banned || !st.Perm || st.Reason != importReason {
		t.Fatalf("imported ban with no reason = %+v", st)
	}
	if st, _ := db.GetBanState(timedGUID); !st.Banned || !st.Perm || st.Reason != "spam again" {
		t.Fatalf("timed ban after import = %+v, want permanent", st)
	}
	f, err := Export(nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, b := range f.Bans {
		names[b.GUID] = b.Name + "/" + b.SteamID
	}
	if len(f.Bans) != 3 || names[test.GUID2] != "Unknown/" || names[timedGUID] != "Spammer/76561198000000001" {
		t.Fatalf("export after import = %+v", f.Bans)
	}
	if !importedPending[test.GUID2] || !importedPending[timedGUID] {
		t.Fatalf("imported bans not waiting for the server: %v", importedPending)
	}
}

// TestExportOnlyPermanent checks that Export leaves timed bans out and,
// given GUIDs, keeps only those.
func TestExportOnlyPermanent(t *testing.T) {
	testDB(t)
	f, err := Export(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Bans) != 1 || f.Bans[0].GUID != test.GUID1 || f.Bans[0].Reason != "aimbot" || f.Bans[0].IP != "1.1.1.1" {
		t.Fatalf("export = %+v, want only the permanent ban", f.Bans)
	}
	if f, _ := Export(map[string]bool{test.GUID2: true}); len(f.Bans) != 0 {
		t.Fatalf("export of a GUID with no permanent ban = %+v", f.Bans)
	}
}
