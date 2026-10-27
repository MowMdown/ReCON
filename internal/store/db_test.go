package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/test"
)

func TestOpenDatabasePragmasOnEveryConnection(t *testing.T) {
	db := test.OpenDB(t)
	ctx := context.Background()
	// Hold two connections at once so they are different ones from the pool.
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for i, c := range []*sql.Conn{c1, c2} {
		var fk, timeout int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || timeout != 5000 {
			t.Errorf("connection %d: foreign_keys=%d busy_timeout=%d", i, fk, timeout)
		}
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN SELECT role FROM players WHERE lower(guid) = lower(?)", test.GUID1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, "; "); !strings.Contains(p, "idx_players_guid_lower") {
		t.Errorf("GUID lookup doesn't use the index: %s", p)
	}
}

func TestSyncBans(t *testing.T) {
	db := test.OpenDB(t)
	read := time.Unix(1_700_000_000, 0)
	readd, err := db.SyncBansFromServer([]battleye.Ban{
		{Index: 0, GUID: test.GUID1, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: test.GUID2, Minutes: "60", Reason: "toxic"},
		{Index: 2, GUID: test.GUID2, Minutes: "30", Reason: "toxic"}, // listed twice: the longer one counts
	}, read)
	if err != nil || len(readd) != 0 {
		t.Fatalf("first sync: readd=%v err=%v", readd, err)
	}
	if n := test.CountRows(t, db, "bans"); n != 1 {
		t.Fatalf("%d rows in bans, want only the permanent ban", n)
	}
	next, _ := db.NextBanExpiry()
	if want := read.Add(time.Hour).Unix(); next != want {
		t.Fatalf("timed ban expires at %d, want %d", next, want)
	}

	// Read again 10s later: the expiry doesn't jitter.
	if _, err := db.SyncBansFromServer([]battleye.Ban{
		{Index: 0, GUID: test.GUID1, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: test.GUID2, Minutes: "59", Reason: "toxic"},
	}, read.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.NextBanExpiry(); n != next {
		t.Fatalf("expiry moved from %d to %d", next, n)
	}

	// Nothing has run out yet; then the timed ban has.
	if n, _ := db.ExpireTimedBans(read.Add(59 * time.Minute)); n != 0 {
		t.Fatalf("expired %d bans early", n)
	}
	if n, _ := db.ExpireTimedBans(read.Add(time.Hour)); n != 1 {
		t.Fatalf("expired %d bans at the hour, want 1", n)
	}
	if st, _ := db.GetBanState(test.GUID2); st.Banned {
		t.Fatal("expired ban still shows as banned")
	}

	// A timed ban BattlEye stops listing is deleted; a permanent one is kept
	// and handed back to be re-added.
	if err := db.RecordPlayerBan(test.GUID2, "Someone", "1.2.3.4", "spam", false, read.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	readd, err = db.SyncBansFromServer(nil, read.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(readd) != 1 || readd[0].GUID != test.GUID1 || readd[0].Reason != "cheating" {
		t.Fatalf("readd = %+v, want the permanent ban", readd)
	}
	if st, _ := db.GetBanState(test.GUID1); !st.Banned || !st.Perm {
		t.Fatalf("permanent ban state = %+v", st)
	}
	if st, _ := db.GetBanState(test.GUID2); st.Banned {
		t.Fatal("timed ban missing from BattlEye was kept")
	}
	list, err := db.ListAllBans()
	if err != nil || len(list) != 1 || list[0].Minutes != "perm" {
		t.Fatalf("ListAllBans = %+v, %v", list, err)
	}
}
