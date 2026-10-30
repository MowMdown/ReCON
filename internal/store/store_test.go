package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/store"
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

// TestDeletePlayer checks the Danger tab's Delete Player: a wrong name
// deletes nothing; the exact name removes the player and everything
// recorded about them, but not their ban or other players' rows.
func TestDeletePlayer(t *testing.T) {
	db := test.OpenDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, g := range []string{test.GUID1, test.GUID2} {
		if err := db.OpenPlayerSession(g, "Name "+g[:4], "1.2.3.4", 3, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ClosePlayerSession(g, t0.Add(time.Hour), "disconnected"); err != nil {
			t.Fatal(err)
		}
		if err := db.OpenPlayerSession(g, "Name "+g[:4], "5.6.7.8", 3, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := db.AddChatMessage(t0, g, "in", "hello"); err != nil {
			t.Fatal(err)
		}
		if err := db.SavePlayerNote(g, "a note"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordPlayerBan(test.GUID1, "Name "+test.GUID1[:4], "1.2.3.4", "cheating", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	tables := []string{"players", "active_sessions", "session_history", "player_ips", "chat_messages", "player_notes"}
	count := func() (n []int) {
		for _, table := range tables {
			n = append(n, test.CountRows(t, db, table))
		}
		return n
	}
	before := count()

	if err := db.DeletePlayer(test.GUID1, "name "+test.GUID1[:4]); !errors.Is(err, store.ErrNameMismatch) {
		t.Fatalf("wrong case: err = %v, want ErrNameMismatch", err)
	}
	if got := count(); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("a wrong name deleted rows: %v, was %v", got, before)
	}
	if err := db.DeletePlayer(strings.ToUpper(test.GUID1), "Name "+test.GUID1[:4]); err != nil {
		t.Fatal(err)
	}
	for i, table := range tables {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE guid = ?", test.GUID1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) for the deleted player", table, n)
		}
		if got := count()[i]; got == 0 {
			t.Errorf("%s lost the other player's rows too", table)
		}
	}
	if test.CountRows(t, db, "bans") != 1 {
		t.Error("the ban was deleted; it should stay")
	}
	if err := db.DeletePlayer(test.GUID1, "Name "+test.GUID1[:4]); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("deleting again: err = %v, want sql.ErrNoRows", err)
	}
}
