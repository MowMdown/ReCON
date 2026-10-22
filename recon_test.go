package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testGUID  = "0123456789abcdef0123456789abcdef"
	testGUID2 = "fedcba9876543210fedcba9876543210"
)

// testDB opens a fresh database in a temp folder as globalDatabase and
// resets the player tracker.
func testDB(t *testing.T) *Database {
	t.Helper()
	db, err := OpenDatabase(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	globalDatabase = db
	tracker.byID, tracker.ended = map[int]*trackedPlayer{}, map[string]time.Time{}
	return db
}

func TestEvalCond(t *testing.T) {
	cases := []struct {
		subject, op, value, v string
		want                  bool
	}{
		{"player_role", "ge", "1", "1", true}, // Mod is at least Mod
		{"player_role", "ge", "1", "0", false},
		{"player_role", "le", "1", "2", false},
		{"player_role", "eq", "2", "2", true},
		{"player_role", "ne", "0", "1", true},
		{"player_id", "eq", "3", "03", true},
		{"player_id", "ne", "3", "4", true},
		{"player_guid", "eq", strings.ToUpper(testGUID), testGUID, true},
		{"player_guid", "ne", testGUID, testGUID, false},
		{"player_name", "eq", "survivor", "Survivor", true},
		{"player_name", "has", "ADMIN", "TheAdminGuy", true},
		{"player_name", "wild", "*admin*", "TheAdminGuy", true},
		{"player_ip", "ne", "1.2.3.4", "1.2.3.4", false},
		{"player_ping", "gt", "500", "501", true},
		{"players_online", "le", "0", "0", true},
		{"message", "has", "noob", "you noob!", true},
		{"message", "has", "noob", "noobish", false},
		{"message", "starts", "!discord", "!discord please", true},
		{"message", "exact", "!rules", "  !RULES ", true},
		{"message", "exact", "!rules", "!rules please", false},
	}
	for _, c := range cases {
		b := Branch{Subject: c.subject, Op: c.op, Value: c.value}
		if got := evalCond(b, c.v); got != c.want {
			t.Errorf("{%s} %s %q against %q = %v, want %v", c.subject, c.op, c.value, c.v, got, c.want)
		}
	}
}

func TestRoleConditionUsesNumber(t *testing.T) {
	x := &job{p: &flowPlayer{role: roleMod}}
	v, _ := x.condValue("player_role")
	if !evalCond(Branch{Subject: "player_role", Op: "ge", Value: "1"}, v) {
		t.Fatalf("a Mod should be at least Mod (value %q)", v)
	}
	if name, _ := x.value("player_role"); name != "Mod" {
		t.Fatalf("{player_role} placeholder = %q, want Mod", name)
	}
	if got := describeCond(Branch{Subject: "player_role", Op: "ge", Value: "1"}); got != "{player_role} is at least Mod" {
		t.Fatalf("describeCond = %q", got)
	}
}

func TestCheckIfOperators(t *testing.T) {
	ifStep := func(subject, op, value string) []Step {
		return []Step{{Type: "if", Branches: []Branch{{Subject: subject, Op: op, Value: value, Steps: []Step{{Type: "stop"}}}}}}
	}
	if err := checkSteps(triggerJoin, ifStep("player_id", "gt", "3"), nil); err == nil {
		t.Error("{player_id} > 3 should be rejected")
	}
	if err := checkSteps(triggerJoin, ifStep("player_guid", "eq", "nope"), nil); err == nil {
		t.Error("a malformed GUID value should be rejected")
	}
	if err := checkSteps(triggerJoin, ifStep("player_role", "ge", "5"), nil); err == nil {
		t.Error("role 5 should be rejected")
	}
	if err := checkSteps(triggerJoin, ifStep("player_role", "le", "1"), nil); err != nil {
		t.Errorf("role at most Mod: %v", err)
	}
	if err := checkSteps(triggerChat, ifStep("message", "exact", "!rules"), nil); err != nil {
		t.Errorf("message is exactly: %v", err)
	}
}

func TestParsePlayersUnverifiedGUID(t *testing.T) {
	out := "Players on server:\n" +
		"[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n" +
		"--------------------------------------------------\n" +
		"0   1.2.3.4:2304   47   " + testGUID + "(OK)   Some Name\n" +
		"1   5.6.7.8:2304   0    -   New Guy (Lobby)\n" +
		"(2 players in total)\n"
	ps := parsePlayers(out)
	if len(ps) != 2 {
		t.Fatalf("parsed %d players, want 2", len(ps))
	}
	if ps[0].GUID != testGUID || ps[0].Name != "Some Name" {
		t.Errorf("player 0 = %+v", ps[0])
	}
	if ps[1].GUID != "" {
		t.Errorf("unverified player's GUID = %q, want empty", ps[1].GUID)
	}
}

func TestOpenDatabasePragmasOnEveryConnection(t *testing.T) {
	db := testDB(t)
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
	rows, err := db.Query("EXPLAIN QUERY PLAN SELECT role FROM players WHERE lower(guid) = lower(?)", testGUID)
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

func TestOpenDatabaseAddsColumnsToOldTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE bans (guid TEXT PRIMARY KEY, player_name TEXT NOT NULL, ip TEXT NOT NULL,
		reason TEXT NOT NULL, perm INTEGER NOT NULL DEFAULT 0, steam_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE bans SET expires_unix = 1, be_index = 0"); err != nil {
		t.Fatalf("new ban columns missing: %v", err)
	}
}

func TestSyncBans(t *testing.T) {
	db := testDB(t)
	read := time.Unix(1_700_000_000, 0)
	readd, err := db.SyncBans([]Ban{
		{Index: 0, GUID: testGUID, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: testGUID2, Minutes: "60", Reason: "toxic"},
		{Index: 2, GUID: testGUID2, Minutes: "30", Reason: "toxic"}, // listed twice: the longer one counts
	}, read)
	if err != nil || len(readd) != 0 {
		t.Fatalf("first sync: readd=%v err=%v", readd, err)
	}
	next, _ := db.NextBanExpiry()
	if want := read.Add(time.Hour).Unix(); next != want {
		t.Fatalf("timed ban expires at %d, want %d", next, want)
	}

	// Read again 10s later: the expiry doesn't jitter.
	if _, err := db.SyncBans([]Ban{
		{Index: 0, GUID: testGUID, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: testGUID2, Minutes: "59", Reason: "toxic"},
	}, read.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.NextBanExpiry(); n != next {
		t.Fatalf("expiry moved from %d to %d", next, n)
	}

	// Nothing has run out yet; then the timed ban has.
	if n, _ := db.ExpireBans(read.Add(59 * time.Minute)); n != 0 {
		t.Fatalf("expired %d bans early", n)
	}
	if n, _ := db.ExpireBans(read.Add(time.Hour)); n != 1 {
		t.Fatalf("expired %d bans at the hour, want 1", n)
	}
	if st, _ := db.BanState(testGUID2); st.Banned {
		t.Fatal("expired ban still shows as banned")
	}

	// A timed ban BattlEye stops listing is deleted; a permanent one is kept
	// and handed back to be re-added.
	if err := db.BanUser(testGUID2, "Someone", "1.2.3.4", "spam", false, read.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	readd, err = db.SyncBans(nil, read.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(readd) != 1 || readd[0].GUID != testGUID || readd[0].Reason != "cheating" {
		t.Fatalf("readd = %+v, want the permanent ban", readd)
	}
	if st, _ := db.BanState(testGUID); !st.Banned || !st.Perm {
		t.Fatalf("permanent ban state = %+v", st)
	}
	if st, _ := db.BanState(testGUID2); st.Banned {
		t.Fatal("timed ban missing from BattlEye was kept")
	}
	list, err := db.BanList()
	if err != nil || len(list) != 1 || list[0].Minutes != "perm" {
		t.Fatalf("BanList = %+v, %v", list, err)
	}
}

func TestEventHubKeepsNewest(t *testing.T) {
	h := &eventHub{subs: map[chan ServerEvent]struct{}{}}
	for i := 0; i < 3*recentEventsKept+7; i++ {
		h.publish(ServerEvent{Kind: eventOther})
	}
	ev := h.recentEvents()
	if len(ev) != recentEventsKept {
		t.Fatalf("kept %d events, want %d", len(ev), recentEventsKept)
	}
	if last := ev[len(ev)-1].Seq; last != int64(3*recentEventsKept+7) {
		t.Fatalf("newest Seq = %d", last)
	}
	if first := ev[0].Seq; first != int64(2*recentEventsKept+8) {
		t.Fatalf("oldest Seq = %d", first)
	}
}

// line feeds one server message to the tracker as if it arrived at `at`.
func line(text string, at time.Time) { trackEvent(parseServerMessage(text, at)) }

type sessionRow struct {
	ended         string
	duration      int64
	loaded        bool
	connected, at time.Time
}

func history(t *testing.T, db *Database, guid string) []sessionRow {
	t.Helper()
	rows, err := db.Query("SELECT ended, duration_seconds, loaded_at IS NOT NULL, connected_at, disconnected_at FROM session_history WHERE guid = ? ORDER BY id", guid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.ended, &r.duration, &r.loaded, &r.connected, &r.at); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func lastSeen(t *testing.T, db *Database, guid string) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow("SELECT last_seen FROM players WHERE guid = ?", guid).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func countRows(t *testing.T, db *Database, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTrackerSessionFromServerMessages(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	line("Player #3 Survivor (1.2.3.4:2304) connected", t0)
	if countRows(t, db, "active_sessions") != 0 {
		t.Fatal("session opened before the GUID was known")
	}
	line("Player #3 Survivor - BE GUID: "+testGUID, t0.Add(time.Second))
	if countRows(t, db, "active_sessions") != 1 {
		t.Fatal("session not opened on the GUID line")
	}
	line("Verified GUID ("+testGUID+") of player #3 Survivor", t0.Add(2*time.Second)) // no second session

	lobby := []Player{{ID: 3, IP: "1.2.3.4", GUID: testGUID, Name: "Survivor (Lobby)"}}
	inGame := []Player{{ID: 3, IP: "1.2.3.4", GUID: testGUID, Name: "Survivor"}}
	playersPolledTracker(lobby, t0.Add(5*time.Second))
	playersPolledTracker(inGame, t0.Add(30*time.Second))
	var loaded sql.NullTime
	if err := db.QueryRow("SELECT loaded_at FROM active_sessions WHERE guid = ?", testGUID).Scan(&loaded); err != nil || !loaded.Valid {
		t.Fatalf("loaded_at not set: %v %v", loaded, err)
	}
	seen := lastSeen(t, db, testGUID)
	playersPolledTracker(inGame, t0.Add(35*time.Second)) // nothing changed: nothing written
	if !lastSeen(t, db, testGUID).Equal(seen) {
		t.Fatal("a poll with nothing new wrote to the database")
	}

	left := t0.Add(10 * time.Minute)
	line("Player #3 Survivor disconnected", left)
	h := history(t, db, testGUID)
	if len(h) != 1 || h[0].ended != endDisconnected || h[0].duration != 600 || !h[0].loaded || !h[0].connected.Equal(t0) {
		t.Fatalf("history = %+v", h)
	}
	if !lastSeen(t, db, testGUID).Equal(left) {
		t.Fatalf("last_seen = %v, want the disconnect line's time %v", lastSeen(t, db, testGUID), left)
	}
	// A poll sent before the disconnect line arrived must not reopen them.
	playersPolledTracker(inGame, left.Add(-2*time.Second))
	if countRows(t, db, "active_sessions") != 0 {
		t.Fatal("a stale poll reopened the session")
	}
}

func TestTrackerKickAndBan(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #1 A (1.1.1.1:2304) connected", t0)
	line("Player #1 A - BE GUID: "+testGUID, t0)
	line("Player #1 A ("+testGUID+") has been kicked by BattlEye: Admin Kick (bye)", t0.Add(time.Minute))
	line("Player #1 A disconnected", t0.Add(time.Minute))
	if h := history(t, db, testGUID); len(h) != 1 || h[0].ended != endKicked {
		t.Fatalf("kick history = %+v", h)
	}

	line("Player #2 B (2.2.2.2:2304) connected", t0)
	line("Player #2 B - BE GUID: "+testGUID2, t0)
	if err := db.BanUser(testGUID2, "B", "2.2.2.2", "cheating", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	line("Player #2 B ("+testGUID2+") has been kicked by BattlEye: Admin Ban (cheating)", t0.Add(time.Minute))
	line("Player #2 B disconnected", t0.Add(time.Minute))
	if h := history(t, db, testGUID2); len(h) != 1 || h[0].ended != endBanned {
		t.Fatalf("ban history = %+v", h)
	}
}

func TestTrackerMissedLines(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	on := []Player{{ID: 4, IP: "4.4.4.4", GUID: testGUID, Name: "Quiet"}}
	// Already on when ReCON started: the first poll opens the session.
	playersPolledTracker(on, t0)
	if countRows(t, db, "active_sessions") != 1 {
		t.Fatal("poll didn't open a session for an untracked player")
	}
	playersPolledTracker(on, t0.Add(5*time.Second))
	// Gone without a disconnect line: closed after two polls, at the last sighting.
	playersPolledTracker(nil, t0.Add(10*time.Second))
	if countRows(t, db, "active_sessions") != 1 {
		t.Fatal("closed after a single missed poll")
	}
	playersPolledTracker(nil, t0.Add(15*time.Second))
	h := history(t, db, testGUID)
	if len(h) != 1 || !h[0].at.Equal(t0.Add(5*time.Second)) || h[0].ended != endDisconnected {
		t.Fatalf("history = %+v", h)
	}

	// Server stops answering: every open session closes at the last sighting.
	playersPolledTracker(on, t0.Add(time.Minute+20*time.Second))
	serverLost()
	if countRows(t, db, "active_sessions") != 0 || len(history(t, db, testGUID)) != 2 {
		t.Fatal("serverLost didn't close the session")
	}
}
