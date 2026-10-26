package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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
	db, err := OpenPlayerDatabase(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	globalDatabase = db
	tracker.byID, tracker.ended = map[int]*trackedPlayer{}, map[string]time.Time{}
	tracker.away, tracker.known = map[string]*trackedPlayer{}, false
	tracker.downAt, tracker.graceUntil = time.Time{}, time.Time{}
	return db
}

// setOnline puts players in the tracker as if the console log and a poll
// had told it about them (no database writes).
func setOnline(players ...Player) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.byID, tracker.known = map[int]*trackedPlayer{}, true
	for _, p := range players {
		tracker.byID[p.ID] = &trackedPlayer{id: p.ID, name: bareName(p.Name), guid: p.GUID, ping: p.Ping, lobby: inLobby(p)}
	}
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
		if got := evaluateIfCondition(b, c.v); got != c.want {
			t.Errorf("{%s} %s %q against %q = %v, want %v", c.subject, c.op, c.value, c.v, got, c.want)
		}
	}
}

func TestRoleConditionUsesNumber(t *testing.T) {
	x := &job{p: &flowPlayer{role: roleMod}}
	v, _ := x.ifSubjectValue("player_role")
	if !evaluateIfCondition(Branch{Subject: "player_role", Op: "ge", Value: "1"}, v) {
		t.Fatalf("a Mod should be at least Mod (value %q)", v)
	}
	if name, _ := x.placeholderValue("player_role"); name != "Mod" {
		t.Fatalf("{player_role} placeholder = %q, want Mod", name)
	}
	if got := describeIfCondition(Branch{Subject: "player_role", Op: "ge", Value: "1"}); got != "{player_role} is at least Mod" {
		t.Fatalf("describeIfCondition = %q", got)
	}
}

func TestCheckIfOperators(t *testing.T) {
	ifStep := func(subject, op, value string) []Step {
		return []Step{{Type: "if", Branches: []Branch{{Subject: subject, Op: op, Value: value, Steps: []Step{{Type: "stop"}}}}}}
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_id", "gt", "3"), nil); err == nil {
		t.Error("{player_id} > 3 should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_guid", "eq", "nope"), nil); err == nil {
		t.Error("a malformed GUID value should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_role", "ge", "5"), nil); err == nil {
		t.Error("role 5 should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_role", "le", "1"), nil); err != nil {
		t.Errorf("role at most Mod: %v", err)
	}
	if err := checkFlowSteps(triggerChat, 0, ifStep("message", "exact", "!rules"), nil); err != nil {
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
	ps := parsePlayersReply(out)
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

// previousSchema is the tables as the previous release created them.
const previousSchema = `
CREATE TABLE players (guid TEXT PRIMARY KEY, username TEXT NOT NULL, last_ip TEXT NOT NULL,
	first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	total_playtime_seconds INTEGER NOT NULL DEFAULT 0, role INTEGER NOT NULL DEFAULT 0, steam_id TEXT);
CREATE TABLE active_sessions (guid TEXT PRIMARY KEY, ip TEXT NOT NULL,
	connected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE);
CREATE TABLE session_history (id INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT NOT NULL, ip TEXT NOT NULL,
	connected_at DATETIME NOT NULL, disconnected_at DATETIME NOT NULL, duration_seconds INTEGER NOT NULL,
	FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE);
CREATE TABLE bans (guid TEXT PRIMARY KEY, player_name TEXT NOT NULL, ip TEXT NOT NULL, reason TEXT NOT NULL,
	perm INTEGER NOT NULL DEFAULT 0, steam_id TEXT);
CREATE TABLE player_ips (guid TEXT NOT NULL, ip TEXT NOT NULL, first_seen DATETIME NOT NULL,
	last_seen DATETIME NOT NULL, PRIMARY KEY (guid, ip),
	FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE);
CREATE TABLE chat_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, at DATETIME NOT NULL, guid TEXT NOT NULL,
	direction TEXT NOT NULL, line TEXT NOT NULL);
CREATE TABLE player_notes (guid TEXT PRIMARY KEY, note TEXT NOT NULL, updated_at DATETIME NOT NULL);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE flows (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, script TEXT NOT NULL,
	cancel TEXT NOT NULL DEFAULT '', timer TEXT NOT NULL DEFAULT '', schedule TEXT NOT NULL DEFAULT '',
	schedule_enabled INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0);
`

// TestUpgradeFromPreviousRelease opens a database written by the previous
// release, as ReCON does on startup, and checks that everything still works
// with no separate migration step.
func TestUpgradeFromPreviousRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "players.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(previousSchema); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO players (guid, username, last_ip, first_seen, last_seen, total_playtime_seconds, role) VALUES (?, 'Survivor', '1.2.3.4', ?, ?, 3600, 1)", []any{testGUID, t0, t0}},
		{"INSERT INTO players (guid, username, last_ip, first_seen, last_seen) VALUES ('-', 'New Guy (Lobby)', '5.6.7.8', ?, ?)", []any{t0, t0}},
		{"INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds) VALUES (?, '1.2.3.4', ?, ?, 3600)", []any{testGUID, t0, t0.Add(time.Hour)}},
		{"INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds) VALUES ('-', '5.6.7.8', ?, ?, 60)", []any{t0, t0.Add(time.Minute)}},
		{"INSERT INTO player_ips (guid, ip, first_seen, last_seen) VALUES ('-', '5.6.7.8', ?, ?)", []any{t0, t0}},
		{"INSERT INTO active_sessions (guid, ip, connected_at) VALUES (?, '1.2.3.4', ?)", []any{testGUID, t0}},
		{"INSERT INTO chat_messages (at, guid, direction, line) VALUES (?, '-', 'in', '(Global) New Guy: hi')", []any{t0}},
		{"INSERT INTO bans (guid, player_name, ip, reason, perm) VALUES (?, 'Cheater', '9.9.9.9', 'aimbot', 1)", []any{testGUID2}},
		{"INSERT INTO bans (guid, player_name, ip, reason, perm) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'Toxic', '8.8.8.8', 'spam', 0)", nil},
	} {
		if _, err := old.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	old.Close()

	db, err := OpenPlayerDatabase(path)
	if err != nil {
		t.Fatalf("opening the old database: %v", err)
	}
	defer db.Close()
	globalDatabase = db
	tracker.byID, tracker.ended = map[int]*trackedPlayer{}, map[string]time.Time{}
	if err := initStoredFlows(); err != nil {
		t.Fatal(err)
	}

	// The unverified-player rows are gone, with everything hanging off them.
	for _, table := range []string{"players", "session_history", "player_ips", "chat_messages"} {
		var n int
		db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE guid = '-'").Scan(&n)
		if n != 0 {
			t.Errorf("%s still has %d row(s) under the GUID \"-\"", table, n)
		}
	}
	// Real players keep their history.
	d, err := db.GetPlayerDetails(testGUID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Player.PlaytimeSeconds != 3600 || d.Player.Role != roleMod || d.Sessions.Count != 1 {
		t.Fatalf("details after upgrade = %+v", d)
	}
	// An old open session (no player number, no loaded_at) closes normally.
	if _, err := db.ClosePlayerSession(testGUID, t0.Add(30*time.Minute), endDisconnected); err != nil {
		t.Fatal(err)
	}
	if h := history(t, db, testGUID); len(h) != 2 || h[1].duration != 1800 || h[1].loaded || h[1].ended != endDisconnected {
		t.Fatalf("history after upgrade = %+v", h)
	}
	// Old bans: the permanent one stays; the timed one is dropped from the
	// table (timed bans are kept in memory, from BattlEye's list).
	states, err := db.AllBanStates()
	if err != nil {
		t.Fatal(err)
	}
	if st := states[testGUID2]; !st.Banned || !st.Perm {
		t.Errorf("permanent ban = %+v", st)
	}
	if st := states["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]; st.Banned {
		t.Errorf("timed ban still stored: %+v", st)
	}
	if n := countRows(t, db, "bans"); n != 1 {
		t.Errorf("%d rows in bans, want 1", n)
	}
	// Opening it again (the next restart) changes nothing and doesn't fail.
	db2, err := OpenPlayerDatabase(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

func TestSyncBans(t *testing.T) {
	db := testDB(t)
	read := time.Unix(1_700_000_000, 0)
	readd, err := db.SyncBansFromServer([]Ban{
		{Index: 0, GUID: testGUID, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: testGUID2, Minutes: "60", Reason: "toxic"},
		{Index: 2, GUID: testGUID2, Minutes: "30", Reason: "toxic"}, // listed twice: the longer one counts
	}, read)
	if err != nil || len(readd) != 0 {
		t.Fatalf("first sync: readd=%v err=%v", readd, err)
	}
	if n := countRows(t, db, "bans"); n != 1 {
		t.Fatalf("%d rows in bans, want only the permanent ban", n)
	}
	next, _ := db.NextBanExpiry()
	if want := read.Add(time.Hour).Unix(); next != want {
		t.Fatalf("timed ban expires at %d, want %d", next, want)
	}

	// Read again 10s later: the expiry doesn't jitter.
	if _, err := db.SyncBansFromServer([]Ban{
		{Index: 0, GUID: testGUID, Minutes: "perm", Perm: true, Reason: "cheating"},
		{Index: 1, GUID: testGUID2, Minutes: "59", Reason: "toxic"},
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
	if st, _ := db.GetBanState(testGUID2); st.Banned {
		t.Fatal("expired ban still shows as banned")
	}

	// A timed ban BattlEye stops listing is deleted; a permanent one is kept
	// and handed back to be re-added.
	if err := db.RecordPlayerBan(testGUID2, "Someone", "1.2.3.4", "spam", false, read.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	readd, err = db.SyncBansFromServer(nil, read.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(readd) != 1 || readd[0].GUID != testGUID || readd[0].Reason != "cheating" {
		t.Fatalf("readd = %+v, want the permanent ban", readd)
	}
	if st, _ := db.GetBanState(testGUID); !st.Banned || !st.Perm {
		t.Fatalf("permanent ban state = %+v", st)
	}
	if st, _ := db.GetBanState(testGUID2); st.Banned {
		t.Fatal("timed ban missing from BattlEye was kept")
	}
	list, err := db.ListAllBans()
	if err != nil || len(list) != 1 || list[0].Minutes != "perm" {
		t.Fatalf("ListAllBans = %+v, %v", list, err)
	}
}

func TestEventHubKeepsNewest(t *testing.T) {
	h := &eventHub{subs: map[chan ServerEvent]struct{}{}}
	total := 3*consoleLogLimit + 7
	for i := 0; i < total; i++ {
		h.publish(ServerEvent{Kind: eventOther})
	}
	ev := h.recentEvents()
	if len(ev) != consoleLogLimit {
		t.Fatalf("kept %d events, want %d", len(ev), consoleLogLimit)
	}
	if last := ev[len(ev)-1].Seq; last != int64(total) {
		t.Fatalf("newest Seq = %d", last)
	}
	if first := ev[0].Seq; first != int64(total-consoleLogLimit+1) {
		t.Fatalf("oldest Seq = %d", first)
	}
	// after() finds its place by bisection; a limit keeps the newest.
	if got := h.after(int64(total - 3)); len(got) != 3 || got[0].Seq != int64(total-2) {
		t.Fatalf("after(total-3) = %d events starting at %d", len(got), got[0].Seq)
	}
	got, last := h.afterWithLast(0, 10)
	if len(got) != 10 || got[9].Seq != int64(total) || last != int64(total) {
		t.Fatalf("afterWithLast(0, 10) = %d events, last %d", len(got), last)
	}
}

// line feeds one server message to the tracker as if it arrived at `at`.
func line(text string, at time.Time) { trackServerEvent(parseServerMessage(text, at)) }

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
	if err := db.RecordPlayerBan(testGUID2, "B", "2.2.2.2", "cheating", true, time.Time{}); err != nil {
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
	// Gone without a disconnect line: the next poll closes them, at its time.
	playersPolledTracker(nil, t0.Add(10*time.Second))
	h := history(t, db, testGUID)
	if len(h) != 1 || !h[0].at.Equal(t0.Add(10*time.Second)) || h[0].ended != endDisconnected {
		t.Fatalf("history = %+v", h)
	}

	// Server stops answering: every open session closes when it last answered.
	playersPolledTracker(on, t0.Add(time.Minute))
	serverWentDown(t0.Add(time.Minute + 5*time.Second))
	h = history(t, db, testGUID)
	if countRows(t, db, "active_sessions") != 0 || len(h) != 2 || !h[1].at.Equal(t0.Add(time.Minute+5*time.Second)) {
		t.Fatalf("serverWentDown didn't close the session: %+v", h)
	}
}

// fakeBE is a minimal BattlEye RCON server on localhost that counts the
// login packets and commands it gets, and can be told to stop answering.
type fakeBE struct {
	conn     net.PacketConn
	mu       sync.Mutex
	silent   bool   // take packets but don't answer, like a server still starting
	mute     bool   // answer logins but not commands, like a server that's nearly up
	password string // when set, other passwords are rejected
	logins   int
	commands []string
	beats    int // empty commands (heartbeats)
}

func newFakeBE(t *testing.T) *fakeBE {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBE{conn: c}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			typ, p, ok := parseRconPacket(buf[:n])
			if !ok {
				continue
			}
			f.mu.Lock()
			silent, accept := f.silent || (f.mute && typ == 1), f.password == "" || string(p) == f.password
			switch typ {
			case 0:
				f.logins++
			case 1:
				if len(p) > 1 {
					f.commands = append(f.commands, string(p[1:]))
				} else {
					f.beats++
				}
			}
			f.mu.Unlock()
			if silent {
				continue
			}
			switch typ {
			case 0:
				ok := byte(0)
				if accept {
					ok = 1
				}
				c.WriteTo(buildRconPacket(0, []byte{ok}), from)
			case 1:
				c.WriteTo(buildRconPacket(1, p[:1]), from) // ack with the same seq
			}
		}
	}()
	return f
}

func (f *fakeBE) set(silent bool) { f.mu.Lock(); f.silent = silent; f.mu.Unlock() }

func (f *fakeBE) setMute(mute bool) { f.mu.Lock(); f.mute = mute; f.mu.Unlock() }

func (f *fakeBE) setPassword(pw string) { f.mu.Lock(); f.password = pw; f.mu.Unlock() }

func (f *fakeBE) counts() (logins, unlocks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if c == "#unlock" {
			unlocks++
		}
	}
	return f.logins, unlocks
}

// TestOneLoginPerHeartbeat checks that a server which isn't answering gets
// at most one login packet per heartbeat (each queued packet becomes another
// "RCon admin logged in" when the server catches up), that commands never
// log in, and that an accepted login is followed by exactly one #unlock and
// no new login while it settles.
func TestOneLoginPerHeartbeat(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := newFakeBE(t)
	setRconTarget(f.conn.LocalAddr().String(), "pw")
	oldBeat, oldPoll, oldReply, oldSettle := heartbeatWait, pollWait, reloginReplyWait, reloginSettle
	t.Cleanup(func() {
		heartbeatWait, pollWait, reloginReplyWait, reloginSettle = oldBeat, oldPoll, oldReply, oldSettle
		rconFails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, pollWait, reloginReplyWait = 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	rconFails.Store(0)

	// Nothing is logged in yet: commands fail without logging in.
	f.set(true)
	for i := 0; i < 5; i++ {
		if _, _, err := runWebCommand("players", 10*time.Millisecond, false, ""); !errors.Is(err, errNotLoggedIn) {
			t.Fatalf("err = %v, want errNotLoggedIn", err)
		}
	}
	if l, _ := f.counts(); l != 0 {
		t.Fatalf("commands sent %d login(s)", l)
	}

	var w serverWatch
	t0 := time.Now()
	logins := func() int { l, _ := f.counts(); return l }
	// A minute of rounds against a silent server: one login per heartbeat
	// (new sockets while none is accepted).
	for i := 0; i <= 12; i++ {
		w.check(t0.Add(time.Duration(i) * pollEvery))
	}
	if l := logins(); l != 3 {
		t.Fatalf("%d login packets in a minute of rounds; want 3 (one per heartbeat)", l)
	}
	if currentRconStatus() != "unavailable" {
		t.Fatalf("status %s", currentRconStatus())
	}

	// The server takes logins but doesn't answer commands yet (it's starting
	// up): one login, one #unlock, and no new login while it settles.
	f.set(false)
	f.setMute(true)
	for i := 6; i <= 12; i++ {
		w.check(t0.Add(time.Duration(i) * heartbeatEvery / 2))
	}
	if l, u := f.counts(); l != 4 || u != 1 {
		t.Fatalf("while settling: %d login(s), %d #unlock(s); want 4 and 1", l, u)
	}
	// Once it has settled, a heartbeat logs in again on the same socket.
	reloginSettle = 0
	w.check(t0.Add(7 * heartbeatEvery))
	if l, u := f.counts(); l != 5 || u != 2 {
		t.Fatalf("after settling: %d login(s), %d #unlock(s); want 5 and 2", l, u)
	}
	// It answers: connected, and no more logins.
	f.setMute(false)
	w.check(t0.Add(8 * heartbeatEvery))
	if currentRconStatus() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", currentRconStatus())
	}
	if l := logins(); l != 5 {
		t.Fatalf("%d login(s) once answering, want 5", l)
	}
}

func TestLastChatFromServerMessages(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Survivor (1.2.3.4:2304) connected", t0)
	line("Player #0 Survivor - BE GUID: "+testGUID, t0)
	line("Player #1 Quiet (5.6.7.8:2304) connected", t0)
	line("Player #1 Quiet - BE GUID: "+testGUID2, t0)

	line("(Global) Survivor: first", t0.Add(time.Minute))
	line("(Side) Survivor: anyone at NWAF?", t0.Add(2*time.Minute))
	// ReCON's replies are commands it sends, not lines from the server, and
	// are only written to the chat history.
	if err := db.AddChatMessage(t0.Add(3*time.Minute), testGUID, chatOut, "> say 0 hello from the admins"); err != nil {
		t.Fatal(err)
	}
	last := lastChatPerPlayer()
	if c := last[0]; c.guid != testGUID || c.chat.Text != "anyone at NWAF?" || c.chat.Channel != "Side" || !c.chat.At.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("Survivor's last chat = %+v", c)
	}
	if _, ok := last[1]; ok {
		t.Fatal("a player who hasn't chatted has a last message")
	}

	// Two players with the same name: the line can't be placed, so neither gets it.
	line("Player #2 Quiet (9.9.9.9:2304) connected", t0)
	line("(Global) Quiet: who said that?", t0.Add(4*time.Minute))
	if _, ok := lastChatPerPlayer()[1]; ok {
		t.Fatal("an ambiguous chat line was given to a player")
	}

	// It goes when they leave: a new session starts empty.
	line("Player #0 Survivor disconnected", t0.Add(5*time.Minute))
	line("Player #0 Survivor (1.2.3.4:2304) connected", t0.Add(6*time.Minute))
	line("Player #0 Survivor - BE GUID: "+testGUID, t0.Add(6*time.Minute))
	if _, ok := lastChatPerPlayer()[0]; ok {
		t.Fatal("the last session's message is still shown")
	}
}

func TestRootRedirectsToPlayers(t *testing.T) {
	mux := buildWebRoutes()
	for path, want := range map[string]int{"/": http.StatusFound, "/plyers": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusFound && rec.Header().Get("Location") != "/players" {
			t.Errorf("GET %s redirects to %q", path, rec.Header().Get("Location"))
		}
	}
}

// resetWeb starts a test with no web session and no wait before the
// next login, and closes it and restores the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := currentRconTarget()
	webConnection.Lock()
	if webConnection.connection != nil {
		webConnection.connection.close()
	}
	webConnection.connection, webConnection.lastErr, webConnection.loggedInAt = nil, nil, time.Time{}
	webConnection.Unlock()
	t.Cleanup(func() {
		setRconTarget(oldAddr, oldPass)
		webConnection.Lock()
		if webConnection.connection != nil {
			webConnection.connection.close()
		}
		webConnection.connection, webConnection.lastErr, webConnection.loggedInAt = nil, nil, time.Time{}
		webConnection.Unlock()
	})
}

func TestReconnectWebOnlySwitchesOnSuccess(t *testing.T) {
	resetWeb(t)
	a, b := newFakeBE(t), newFakeBE(t)
	a.setPassword("apw")
	b.setPassword("bpw")
	addrA, addrB := a.conn.LocalAddr().String(), b.conn.LocalAddr().String()
	setRconTarget(addrA, "apw")
	if err := loginWebSession(); err != nil {
		t.Fatal(err)
	}
	old, err := liveWebSession()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong password for B: nothing changes.
	if err := reconnectWebSession(addrB, "nope"); err == nil {
		t.Fatal("a rejected login was accepted")
	}
	if addr, _ := currentRconTarget(); addr != addrA {
		t.Fatalf("target moved to %s after a failed connect", addr)
	}
	if c, _ := liveWebSession(); c != old || old.isClosed() {
		t.Fatal("the working session was replaced or closed after a failed connect")
	}

	// Right password: B becomes the session and the target; A's session closes.
	if err := reconnectWebSession(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if addr, pass := currentRconTarget(); addr != addrB || pass != "bpw" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if c, _ := liveWebSession(); c == old {
		t.Fatal("still on the old session")
	}
	time.Sleep(50 * time.Millisecond)
	if !old.isClosed() {
		t.Fatal("the old session was left open")
	}
	if _, unlocks := b.counts(); unlocks != 1 {
		t.Fatalf("B got %d #unlock(s), want 1", unlocks)
	}

	// Connecting again to the same place does a fresh handshake.
	before, _ := b.counts()
	if err := reconnectWebSession(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.counts(); after != before+1 {
		t.Fatalf("reconnecting sent %d login(s), want 1", after-before)
	}
}

func TestRconSettingsPage(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := newFakeBE(t)
	f.setPassword("secret")
	host, port, _ := net.SplitHostPort(f.conn.LocalAddr().String())
	t.Setenv("RCON_HOST", "10.9.9.9")
	t.Setenv("RCON_PORT", "2306")
	t.Setenv("RCON_PASSWORD", "compose-password")
	mux := buildWebRoutes()
	call := func(method, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/rcon-settings", strings.NewReader(body))
		mux.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// GET shows the defaults, the password included (shown as placeholder text).
	code, got := call(http.MethodGet, "")
	d, _ := got["defaults"].(map[string]any)
	if code != 200 || d["host"] != "10.9.9.9" || d["port"] != "2306" || d["password"] != "compose-password" {
		t.Fatalf("GET = %d %v", code, got)
	}

	// Bad input and a wrong password change nothing.
	if code, _ := call(http.MethodPost, `{"port": "70000"}`); code != http.StatusBadRequest {
		t.Fatalf("port 70000: %d", code)
	}
	if code, _ := call(http.MethodPost, `{"host": "`+host+`", "port": "`+port+`", "password": "wrong"}`); code != http.StatusBadGateway {
		t.Fatalf("wrong password: %d", code)
	}
	if s := currentRconOverrides(); s != (rconSettings{}) {
		t.Fatalf("kept after a failed connect: %+v", s)
	}

	// The right settings connect, are kept (in memory only), and become the target.
	if code, got := call(http.MethodPost, `{"host": " `+host+` ", "port": "`+port+`", "password": "secret"}`); code != 200 || got["ok"] != true {
		t.Fatalf("connect = %d %v", code, got)
	}
	if s := currentRconOverrides(); s.Host != host || s.Port != port || s.Password != "secret" {
		t.Fatalf("kept = %+v", s)
	}
	if addr, pass := currentRconTarget(); addr != net.JoinHostPort(host, port) || pass != "secret" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if _, ok, _ := globalDatabase.GetStoredSetting("rcon"); ok {
		t.Fatal("RCON Settings were written to the database")
	}

	// Empty fields use the defaults.
	if addr, pass := (rconSettings{Port: "1234"}).effective(); addr != "10.9.9.9:1234" || pass != "compose-password" {
		t.Fatalf("defaults = %s %q", addr, pass)
	}
}

func TestNoPasswordDoesNotDial(t *testing.T) {
	if _, err := dialRconAddress("127.0.0.1:1", "", webSource, false); err != errNoPassword {
		t.Fatalf("err = %v, want errNoPassword", err)
	}
}

func TestStatusReportsNextRetry(t *testing.T) {
	resetWeb(t)
	oldNext, oldFails, oldPolled := rconNextPoll.Load(), rconFails.Load(), rconPolled.Load()
	t.Cleanup(func() {
		rconNextPoll.Store(oldNext)
		rconFails.Store(oldFails)
		rconPolled.Store(oldPolled)
	})
	rconPolled.Store(true)
	now := time.Now()

	// Answering, or missed one or two: the next try is the next round.
	rconFails.Store(1)
	w := serverWatch{lastHeartbeat: now.Add(-10 * time.Second)}
	w.storeNextCheck(now)
	if d := rconNextCheck().Sub(now); d < 4900*time.Millisecond || d > 5100*time.Millisecond {
		t.Fatalf("next check in %v, want the next round (5s)", d)
	}
	// Down: only the heartbeat tries, 30s after the last one (20s from now).
	rconFails.Store(downAfterMisses)
	w.storeNextCheck(now)
	if d := rconNextCheck().Sub(now); d < 19900*time.Millisecond || d > 20100*time.Millisecond {
		t.Fatalf("next check in %v, want the next heartbeat (20s)", d)
	}

	status := func() map[string]any {
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if ms, ok := status()["retryInMs"].(float64); !ok || ms < 19000 || ms > 20100 {
		t.Fatalf("retryInMs = %v while unavailable", status()["retryInMs"])
	}
	rconFails.Store(0)
	if _, ok := status()["retryInMs"]; ok {
		t.Fatal("retryInMs sent while connected")
	}
}

// testFlow is a minimal valid manual flow.
func testFlow(name string) Flow {
	return Flow{Name: name, Trigger: triggerManual, Steps: []Step{{Type: "message", To: "all", Text: "hi"}}}
}

func flowIDs(t *testing.T) []int64 {
	t.Helper()
	flows, err := globalDatabase.ListStoredFlows()
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, f := range flows {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestFlowNumbersReuseFreedOnes(t *testing.T) {
	db := testDB(t)
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	add := func(name string) int64 {
		t.Helper()
		id, err := validateAndSaveFlow(testFlow(name), true)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, name := range []string{"a", "b", "c", "d"} {
		if id := add(name); id != int64(i) {
			t.Fatalf("flow %s got #%d, want #%d", name, id, i)
		}
	}
	for _, id := range flowIDs(t) {
		if err := deleteFlowAndRuns(id); err != nil {
			t.Fatal(err)
		}
	}
	// Four deleted, four new: 0-3 again, not 4-7.
	for i, name := range []string{"e", "f", "g", "h"} {
		if id := add(name); id != int64(i) {
			t.Fatalf("after deleting all, flow %s got #%d, want #%d", name, id, i)
		}
	}
	// Deleting one in the middle leaves the others as they are; its number
	// goes to the next new flow, then numbering carries on after the last.
	if err := deleteFlowAndRuns(1); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(flowIDs(t)); got != "[0 2 3]" {
		t.Fatalf("after deleting #1: %s", got)
	}
	if id := add("i"); id != 1 {
		t.Fatalf("next flow got #%d, want #1", id)
	}
	if id := add("j"); id != 4 {
		t.Fatalf("next flow got #%d, want #4", id)
	}
	// Flow 0 is an ordinary flow: it can be edited.
	f, err := db.GetStoredFlow(0)
	if err != nil {
		t.Fatal(err)
	}
	f.Name = "renamed"
	if id, err := validateAndSaveFlow(f, false); err != nil || id != 0 {
		t.Fatalf("editing #0: id %d, %v", id, err)
	}
	if f, _ := db.GetStoredFlow(0); f.Name != "renamed" {
		t.Fatalf("flow #0 = %q", f.Name)
	}
}

func TestFlowsTableLosesAutoincrement(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`CREATE TABLE flows (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE,
		script TEXT NOT NULL, cancel TEXT NOT NULL DEFAULT '', timer TEXT NOT NULL DEFAULT '',
		schedule TEXT NOT NULL DEFAULT '', schedule_enabled INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO flows (id, name, script, schedule, schedule_enabled, position) VALUES (5, 'MOTD', '[{"type":"message","to":"all","text":"hi"}]', '*/20 * * * *', 1, 1)`,
		`INSERT INTO flows (id, name, script, schedule, position) VALUES (7, 'Restart', '[{"type":"lock"}]', 'manual', 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	var sqlText string
	db.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'flows'").Scan(&sqlText)
	if strings.Contains(strings.ToUpper(sqlText), "AUTOINCREMENT") {
		t.Fatalf("still AUTOINCREMENT: %s", sqlText)
	}
	// Existing flows keep their numbers, settings and order.
	if got := fmt.Sprint(flowIDs(t)); got != "[5 7]" {
		t.Fatalf("flows after the move: %s", got)
	}
	if f, err := db.GetStoredFlow(5); err != nil || f.Name != "MOTD" || !f.ScheduleEnabled || f.Trigger != triggerSchedule || len(f.Steps) != 1 {
		t.Fatalf("flow #5 = %+v, %v", f, err)
	}
	// New flows take the free numbers from 0.
	if id, err := validateAndSaveFlow(testFlow("new"), true); err != nil || id != 0 {
		t.Fatalf("new flow: #%d, %v", id, err)
	}
	if err := db.EnsureFlowsTable(); err != nil { // a later start: nothing to do
		t.Fatal(err)
	}
}

func TestFlowAPIUsesMissingIDForNew(t *testing.T) {
	db := testDB(t)
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	mux := buildWebRoutes()
	post := func(path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	steps := `"trigger": "manual", "steps": [{"type": "message", "to": "all", "text": "hi"}]`
	if code, out := post("/api/flows", `{"name": "First", `+steps+`}`); code != 200 || out["id"] != float64(0) {
		t.Fatalf("create without id: %d %v", code, out)
	}
	if code, out := post("/api/flows", `{"id": null, "name": "Second", `+steps+`}`); code != 200 || out["id"] != float64(1) {
		t.Fatalf("create with id null: %d %v", code, out)
	}
	// id 0 updates flow #0 rather than creating another flow.
	if code, out := post("/api/flows", `{"id": 0, "name": "First (renamed)", `+steps+`}`); code != 200 || out["id"] != float64(0) {
		t.Fatalf("update #0: %d %v", code, out)
	}
	if got := fmt.Sprint(flowIDs(t)); got != "[0 1]" {
		t.Fatalf("flows: %s", got)
	}
	if f, _ := db.GetStoredFlow(0); f.Name != "First (renamed)" {
		t.Fatalf("flow #0 = %q", f.Name)
	}
	// Flow 0 is reachable through the per-flow routes.
	if code, _ := post("/api/flows/0/delete", ""); code != 200 {
		t.Fatalf("delete #0: %d", code)
	}
	if code, _ := post("/api/flows", `{"id": -1, "name": "x", `+steps+`}`); code != http.StatusBadRequest {
		t.Fatalf("negative id: %d", code)
	}
}

// TestConnectTriesOnce checks that RCON Settings' Connect sends one login per
// press, even to a server that doesn't answer, and changes nothing.
func TestConnectTriesOnce(t *testing.T) {
	resetWeb(t)
	oldWait := reloginReplyWait
	t.Cleanup(func() { reloginReplyWait = oldWait })
	reloginReplyWait = 50 * time.Millisecond
	f := newFakeBE(t)
	f.set(true)
	setRconTarget("127.0.0.1:1", "pw")
	for press := 1; press <= 3; press++ {
		if err := reconnectWebSession(f.conn.LocalAddr().String(), "pw"); err == nil {
			t.Fatal("connected to a server that doesn't answer")
		}
		time.Sleep(20 * time.Millisecond)
		if logins, _ := f.counts(); logins != press {
			t.Fatalf("after %d press(es): %d login packets, want %d", press, logins, press)
		}
	}
	if addr, _ := currentRconTarget(); addr != "127.0.0.1:1" {
		t.Fatalf("target changed to %s", addr)
	}
}

// pingKickFlow is the example: for each player, if ping is over 500, kick them.
func pingKickFlow() []Step {
	return []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if", Branches: []Branch{{
		Subject: "player_ping", Op: "gt", Value: "500",
		Steps: []Step{{Type: "kick", Who: "player", Reason: "Ping too high ({player_ping})"}},
	}}}}}}
}

func TestForEachValidation(t *testing.T) {
	kick := []Step{{Type: "kick", Who: "player", Reason: "x"}}
	if err := checkFlowSteps(triggerSchedule, 0, kick, nil); err == nil {
		t.Error("Kick outside a For each player was accepted in a Schedule flow")
	}
	if err := checkFlowSteps(triggerSchedule, 0, pingKickFlow(), nil); err != nil {
		t.Errorf("the ping-kick example: %v", err)
	}
	if err := checkFlowSteps(triggerManual, 0, []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "hi {player_name}"}}}}, nil); err != nil {
		t.Errorf("a message to each player: %v", err)
	}
	nested := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "forEach", Of: "message", Steps: kick}}}}
	if err := checkFlowSteps(triggerSchedule, 0, nested, nil); err == nil || !strings.Contains(err.Error(), "inside another") {
		t.Errorf("a loop inside a loop: %v", err)
	}
	// {message} is the looped player's line, in any trigger.
	for _, of := range []string{"player", "message"} {
		words := []Step{{Type: "forEach", Of: of, Steps: []Step{{Type: "if", Branches: []Branch{{
			Subject: "message", Op: "has", Value: "noob", Steps: []Step{{Type: "message", To: "player", Text: "You said: {message}"}}}}}}}}
		if err := checkFlowSteps(triggerSchedule, 0, words, nil); err != nil {
			t.Errorf("For each %s with {message}: %v", of, err)
		}
	}
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "forEach", Of: "everything", Steps: kick}}, nil); err == nil {
		t.Error("For each of an unknown kind was accepted")
	}
	if got := normalizeFlowSteps([]Step{{Type: "forEach"}}); got[0].Of != "player" {
		t.Errorf("a For each without a kind reads as %q, want player", got[0].Of)
	}
	afterLoop := append(pingKickFlow(), kick...)
	if err := checkFlowSteps(triggerSchedule, 0, afterLoop, nil); err == nil {
		t.Error("a Kick after the loop was accepted")
	}
	// When stopped takes every kind of step, with the same rules as Steps.
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "lock"}}, append(pingKickFlow(), Step{Type: "wait", Time: "5s"}, Step{Type: "shutdown"})); err != nil {
		t.Errorf("When stopped with For each, If, Kick, Wait and Shut down: %v", err)
	}
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "lock"}}, kick); err == nil {
		t.Error("a Kick with no player was accepted in When stopped")
	}
	if n := countAllSteps(pingKickFlow()); n != 3 {
		t.Errorf("countAllSteps = %d, want 3", n)
	}
}

func TestForEachKicksOnlyMatchingPlayers(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	setOnline(
		Player{ID: 4, GUID: testGUID, Name: "Laggy", Ping: 820},
		Player{ID: 1, GUID: testGUID2, Name: "Fine", Ping: 40},
		Player{ID: 7, GUID: "", Name: "Loading (Lobby)", Ping: 999}, // not verified yet: skipped
		Player{ID: 2, GUID: "0000000000000000000000000000beef", Name: "Edge", Ping: 500},
		Player{ID: 9, GUID: "0000000000000000000000000000cafe", Name: "Awful", Ping: 1500},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &flowRun{name: "ping check", ctx: ctx, cancel: cancel}
	x := &job{run: run, ctx: ctx}
	if !x.execFlowSteps(pingKickFlow()) {
		t.Fatal("the run ended early")
	}
	want := []string{"kick 4 Ping too high (820)", "kick 9 Ping too high (1500)"}
	if fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	logText := strings.Join(run.log, "\n")
	if !strings.Contains(logText, "For each player: 4 online") || !strings.Contains(logText, "Laggy (#4): If {player_ping} > 500") {
		t.Fatalf("run log:\n%s", logText)
	}
	if strings.Contains(logText, "Fine") {
		t.Fatalf("a player who didn't match was logged:\n%s", logText)
	}

	// Stop flow inside the loop ends the whole run.
	sent = nil
	stopFirst := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "hi"}, {Type: "stop"}}}}
	if x.execFlowSteps(stopFirst) {
		t.Fatal("Stop flow inside the loop didn't end the run")
	}
	if len(sent) != 1 || sent[0] != "say 1 hi" {
		t.Fatalf("sent %q; the loop should stop after the first player (#1)", sent)
	}
}

func TestForEachConsidersOnlyChatSinceLastRun(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #3 Survivor (1.2.3.4:2304) connected", t0)
	line("Player #3 Survivor - BE GUID: "+testGUID, t0)
	line("Player #5 Calm (5.6.7.8:2304) connected", t0)
	line("Player #5 Calm - BE GUID: "+testGUID2, t0)
	// chat sends a line through the event hub, as the server does, so it
	// gets a real Seq, then to the tracker.
	chat := func(text string) {
		ev := parseServerMessage(text, t0)
		serverEvents.publish(ev)
		ev.Seq = serverEvents.lastSeq()
		trackServerEvent(ev)
	}
	const flowID = 42
	t.Cleanup(func() { forEachMarks.Lock(); delete(forEachMarks.byFlow, flowID); forEachMarks.Unlock() })
	// runFlow is one run of the flow: a fresh flowRun, as each launch makes.
	runFlow := func(steps []Step) {
		t.Helper()
		sent = nil
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		x := &job{run: &flowRun{flowID: flowID, name: "chat", ctx: ctx, cancel: cancel}, ctx: ctx}
		if !x.execFlowSteps(steps) {
			t.Fatal("the run ended early")
		}
	}
	warn := []Step{{Type: "forEach", Of: "message", Steps: []Step{{Type: "if", Branches: []Branch{{
		Subject: "message", Op: "has", Value: "noob",
		Steps: []Step{{Type: "message", To: "player", Text: "No insults: {message}"}}}}}}}}
	lastSaid := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "all", Text: "{player_name} said: {message}"}}}}

	chat("(Global) Survivor: hello all")
	chat("(Side) Calm: anyone noob here?")
	chat("(Global) Survivor: you noob")
	runFlow(warn)
	if want := []string{"say 5 No insults: anyone noob here?", "say 3 No insults: you noob"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("first run sent %q, want %q", sent, want)
	}
	runFlow(warn)
	if len(sent) != 0 {
		t.Fatalf("a run with no new lines sent %q", sent)
	}
	chat("(Global) Calm: noob")
	runFlow(warn)
	if want := []string{"say 5 No insults: noob"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("third run sent %q, want %q", sent, want)
	}

	// For each player: {message} is their latest line since the last run,
	// empty for a player who hasn't chatted since.
	chat("(Global) Survivor: brb")
	runFlow(lastSaid)
	if want := []string{"say -1 Survivor said: brb", "say -1 Calm said:"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("For each player sent %q, want %q", sent, want)
	}

	// Two For each in one run look at the same lines.
	chat("(Global) Calm: noob again")
	runFlow(append(append([]Step{}, lastSaid...), warn...))
	if want := []string{"say -1 Survivor said:", "say -1 Calm said: noob again", "say 5 No insults: noob again"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("two loops in one run sent %q, want %q", sent, want)
	}

	// A player's lines go when they leave.
	line("Player #3 Survivor disconnected", t0.Add(time.Hour))
	if _, ok := latestChatOf(testGUID); ok {
		t.Fatal("a player who left still has session lines")
	}
}

// TestSentCommandsGoInConsoleLog checks the master console log: a command
// sent for someone goes in once with who sent it, followed by an error when
// the server doesn't answer; polls and heartbeats don't go in at all; a new
// login logs its #unlock and asks for a players and bans poll.
func TestSentCommandsGoInConsoleLog(t *testing.T) {
	resetWeb(t)
	f := newFakeBE(t)
	setRconTarget(f.conn.LocalAddr().String(), "pw")
	loginPollDue.Store(false)
	t.Cleanup(func() { loginPollDue.Store(false) })

	start := serverEvents.lastSeq()
	if err := loginWebSession(); err != nil {
		t.Fatal(err)
	}
	if !loginPollDue.Load() {
		t.Fatal("a new login didn't ask for a poll")
	}
	runWebCommand("players", time.Second, false, "")
	runWebCommand("say -1 hello", time.Second, false, flowSource("Greeter"))
	f.set(true)
	runWebCommand("kick 3 bye", 300*time.Millisecond, false, "web")

	var got []string
	for _, ev := range serverEvents.after(start) {
		got = append(got, ev.Kind+"|"+ev.Text+"|"+ev.Source)
	}
	want := []string{
		"sent|> #unlock|login",
		`sent|> say -1 hello|flow "Greeter"`,
		"sent|> kick 3 bye|web",
		"error|(no acknowledgement from server)|web",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("console log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestServerWatch checks the server watch: `players` every round and the
// heartbeat every heartbeatEvery while the server answers, the ban list
// after a login; three unanswered rounds mark the server down without
// logging in; while it's down only the heartbeat checks it.
func TestServerWatch(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := newFakeBE(t)
	setRconTarget(f.conn.LocalAddr().String(), "pw")
	oldBeat, oldPoll, oldReply := heartbeatWait, pollWait, reloginReplyWait
	t.Cleanup(func() {
		heartbeatWait, pollWait, reloginReplyWait = oldBeat, oldPoll, oldReply
		rconFails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, pollWait, reloginReplyWait = 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	rconFails.Store(0)

	sent := func() (beats, players, bans int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.commands {
			switch c {
			case "players":
				players++
			case "bans":
				bans++
			}
		}
		return f.beats, players, bans
	}
	want := func(when string, beats, players, bans int) {
		t.Helper()
		if b, p, n := sent(); b != beats || p != players || n != bans {
			t.Fatalf("%s: %d heartbeats, %d players, %d bans; want %d, %d, %d", when, b, p, n, beats, players, bans)
		}
	}
	var w serverWatch
	t0 := time.Now()
	w.check(t0) // logs in: heartbeat, players, and the ban list after the login
	want("first round", 1, 1, 1)
	w.check(t0.Add(pollEvery))
	want("second round", 1, 2, 1)
	w.check(t0.Add(heartbeatEvery))
	want("a heartbeat later", 2, 3, 1)

	f.set(true)
	logins, _ := f.counts()
	for i := 1; i <= downAfterMisses; i++ {
		w.check(t0.Add(heartbeatEvery + time.Duration(i)*pollEvery))
	}
	if rconFails.Load() != downAfterMisses || currentRconStatus() != "unavailable" {
		t.Fatalf("fails = %d, status %s", rconFails.Load(), currentRconStatus())
	}
	if l, _ := f.counts(); l != logins {
		t.Fatalf("%d login(s) between heartbeats, want 0", l-logins)
	}
	want("three missed polls", 2, 6, 1)
	w.check(t0.Add(heartbeatEvery + 4*pollEvery))
	want("a round while down", 2, 6, 1) // no poll, no heartbeat yet

	f.set(false)
	w.check(t0.Add(2 * heartbeatEvery)) // the heartbeat finds it back
	if currentRconStatus() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", currentRconStatus())
	}
	want("back", 3, 6, 1)
	w.check(t0.Add(2*heartbeatEvery + pollEvery))
	want("polling again", 3, 7, 1)
}

// TestRestartKeepsReturningPlayersSession checks the restart grace: a player
// back within restartGrace of the next login keeps one session with the
// downtime not counted; one who isn't is left closed when the server went down.
func TestRestartKeepsReturningPlayersSession(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Back (1.1.1.1:2304) connected", t0)
	line("Player #0 Back - BE GUID: "+testGUID, t0)
	line("Player #1 Gone (2.2.2.2:2304) connected", t0)
	line("Player #1 Gone - BE GUID: "+testGUID2, t0)

	down := t0.Add(10 * time.Minute)
	serverWentDown(down)
	if countRows(t, db, "active_sessions") != 0 {
		t.Fatal("sessions still open while the server is down")
	}
	login := down.Add(5 * time.Minute)
	serverLoggedIn(login)
	back := login.Add(30 * time.Second)
	line("Player #4 Back (1.1.1.1:2304) connected", back)
	line("Player #4 Back - BE GUID: "+testGUID, back)
	if h := history(t, db, testGUID); len(h) != 0 || countRows(t, db, "active_sessions") != 1 {
		t.Fatalf("not resumed: history %+v", h)
	}
	forgetAwayPlayers(login.Add(restartGrace + time.Second))
	line("Player #5 Gone (2.2.2.2:2304) connected", login.Add(2*time.Minute))
	line("Player #5 Gone - BE GUID: "+testGUID2, login.Add(2*time.Minute))
	if h := history(t, db, testGUID2); len(h) != 1 || !h[0].at.Equal(down) || h[0].duration != 600 {
		t.Fatalf("late player's old session = %+v, want closed at the outage", h)
	}

	line("Player #4 Back disconnected", back.Add(20*time.Minute))
	h := history(t, db, testGUID)
	// 10 minutes before the outage and 20 after: the 5.5 minutes down aren't counted.
	if len(h) != 1 || h[0].duration != 30*60 || !h[0].connected.Equal(t0) {
		t.Fatalf("resumed session = %+v, want one 30 minute session from t0", h)
	}
	var playtime int
	db.QueryRow("SELECT total_playtime_seconds FROM players WHERE guid = ?", testGUID).Scan(&playtime)
	if playtime != 30*60 {
		t.Fatalf("playtime = %d, want 1800", playtime)
	}
}

// TestRestartNewIPIsNewSession checks that a player back within
// restartGrace but from another IP gets a new session on that IP, and the
// old one stays closed at the outage.
func TestRestartNewIPIsNewSession(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Mover (1.1.1.1:2304) connected", t0)
	line("Player #0 Mover - BE GUID: "+testGUID, t0)
	down := t0.Add(10 * time.Minute)
	serverWentDown(down)
	login := down.Add(time.Minute)
	serverLoggedIn(login)
	back := login.Add(time.Minute)
	line("Player #2 Mover (9.9.9.9:2304) connected", back)
	line("Player #2 Mover - BE GUID: "+testGUID, back)
	if h := history(t, db, testGUID); len(h) != 1 || !h[0].at.Equal(down) || h[0].duration != 600 {
		t.Fatalf("old session = %+v, want closed at the outage", h)
	}
	var ip string
	if err := db.QueryRow("SELECT ip FROM active_sessions WHERE guid = ?", testGUID).Scan(&ip); err != nil || ip != "9.9.9.9" {
		t.Fatalf("new session ip = %q (%v), want 9.9.9.9", ip, err)
	}
}

func TestLobbyWatching(t *testing.T) {
	testDB(t)
	now := time.Now()
	if anyoneInLobby() {
		t.Fatal("someone loading with nobody online")
	}
	line("Player #2 New (3.3.3.3:2304) connected", now)
	if !anyoneInLobby() {
		t.Fatal("a player who just connected isn't loading")
	}
	playersPolledTracker([]Player{{ID: 2, IP: "3.3.3.3", Name: "New"}}, now.Add(time.Second))
	if anyoneInLobby() {
		t.Fatal("still loading after (Lobby) left the name")
	}
	if p, known := listOnlinePlayers(); !known || len(p) != 1 || p[0].Name != "New" {
		t.Fatalf("listOnlinePlayers = %+v %v", p, known)
	}
}

// TestSchedulerWakesForNextRun checks the clock-based scheduler: nothing
// starts before a run is due, it returns the run's first step (cron time
// plus delay) as its next wake-up, and starts the run once that has come.
func TestSchedulerWakesForNextRun(t *testing.T) {
	if err := testDB(t).EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	oldSend := sendCommand
	sendCommand = func(*flowRun, string) {}
	t.Cleanup(func() { sendCommand = oldSend; lastFired = map[int64]time.Time{} })

	f := Flow{Name: "hourly", Trigger: triggerSchedule, Schedule: "0 * * * *", Delay: "30s",
		Steps: []Step{{Type: "message", To: "all", Text: "hi"}}}
	id, err := validateAndSaveFlow(f, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := globalDatabase.SetFlowScheduleEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Hour).Add(10 * time.Minute)
	due := now.Truncate(time.Hour).Add(time.Hour + 30*time.Second)
	if wake := runDueSchedules(now); !wake.Equal(due) {
		t.Fatalf("wake at %v, want %v (next hour plus the delay)", wake, due)
	}
	if wake := runDueSchedules(due); !wake.Equal(due.Add(time.Hour)) {
		t.Fatalf("after starting, wake at %v, want an hour later", wake)
	}
	// The run started, waiting for its first step at the (future) due time.
	flowRuns.Lock()
	run := flowRuns.byFlow[id]
	started := run != nil && run.active && run.anchor.Equal(due)
	flowRuns.Unlock()
	if !started {
		t.Fatal("the due run wasn't started")
	}
	stopFlowRun(id)
	if _, again := lastFired[id]; !again {
		t.Fatal("the cron time wasn't recorded")
	}
}

// TestJoinFlowsWaitForLoading checks that a player seen connecting is handed
// to the join flows once "(Lobby)" leaves their name, and only once; a
// player who was already on when ReCON started is not.
func TestJoinFlowsWaitForLoading(t *testing.T) {
	testDB(t)
	t0 := time.Now()
	playersPolledTracker([]Player{{ID: 1, IP: "1.1.1.1", GUID: testGUID2, Name: "Already"}}, t0)
	line("Player #3 New (1.2.3.4:2304) connected", t0.Add(time.Second))
	line("Player #3 New - BE GUID: "+testGUID, t0.Add(time.Second))
	both := func(lobby bool) []Player {
		name := "New"
		if lobby {
			name += " (Lobby)"
		}
		return []Player{{ID: 1, IP: "1.1.1.1", GUID: testGUID2, Name: "Already"}, {ID: 3, IP: "1.2.3.4", GUID: testGUID, Name: name}}
	}
	if j := playersPolledTracker(both(true), t0.Add(5*time.Second)); len(j) != 0 {
		t.Fatalf("joined while loading: %+v", j)
	}
	j := playersPolledTracker(both(false), t0.Add(10*time.Second))
	if len(j) != 1 || j[0].ID != 3 || j[0].GUID != testGUID {
		t.Fatalf("joined = %+v, want player 3", j)
	}
	if j := playersPolledTracker(both(false), t0.Add(15*time.Second)); len(j) != 0 {
		t.Fatalf("joined twice: %+v", j)
	}
}

func TestSteamBatchTiming(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	due := steamBatchDue(t0, time.Time{}, 1, t0)
	if !due.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first join: due %v, want a minute later", due)
	}
	due = steamBatchDue(t0, due, 2, t0.Add(10*time.Second))
	if !due.Equal(t0.Add(90 * time.Second)) {
		t.Fatalf("second join: due %v, want 30s more", due)
	}
	for n := 3; n <= 10; n++ {
		due = steamBatchDue(t0, due, n, t0.Add(20*time.Second))
	}
	if !due.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("many joins: due %v, want capped at 3 minutes", due)
	}
	now := t0.Add(30 * time.Second)
	if due := steamBatchDue(t0, due, steamBatchMaxPlayers, now); !due.Equal(now) {
		t.Fatalf("%d players: due %v, want now", steamBatchMaxPlayers, due)
	}
}

// TestWarningCommand checks !warning: a code or a player number finds the
// player, each warning is sent to them and the staff, the warning that
// reaches the threshold is followed by the kick or ban the settings name,
// and Players can't warn. It also checks !kick by player number.
func TestWarningCommand(t *testing.T) {
	db := testDB(t)
	const modGUID = "0000000000000000000000000000a11a"
	now := time.Now()
	for _, p := range []Player{{ID: 1, GUID: modGUID, Name: "Mod"}, {ID: 4, GUID: testGUID, Name: "Rowdy"}, {ID: 6, GUID: testGUID2, Name: "Other"}} {
		if err := db.OpenPlayerSession(p.GUID, p.Name, "1.2.3.4", p.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SetPlayerRole(modGUID, roleMod); err != nil {
		t.Fatal(err)
	}
	mod := Player{ID: 1, GUID: modGUID, Name: "Mod"}
	setOnline(mod, Player{ID: 4, GUID: testGUID, Name: "Rowdy"}, Player{ID: 6, GUID: testGUID2, Name: "Other"})

	var mu sync.Mutex
	var said, kicked, banned []string
	oldSay, oldKick, oldBan, oldDelay := chatSay, chatKick, banStep, warningActionDelay
	t.Cleanup(func() {
		chatSay, chatKick, banStep, warningActionDelay = oldSay, oldKick, oldBan, oldDelay
		chatState.Lock()
		chatState.loaded = false
		chatState.Unlock()
	})
	chatSay = func(to Player, everyone bool, text string) bool {
		mu.Lock()
		defer mu.Unlock()
		if everyone {
			said = append(said, "all: "+text)
		} else {
			said = append(said, fmt.Sprintf("%d: %s", to.ID, text))
		}
		return true
	}
	chatKick = func(p Player, reason string) bool {
		mu.Lock()
		kicked = append(kicked, fmt.Sprintf("%d %s", p.ID, reason))
		mu.Unlock()
		return true
	}
	banStep = func(p Player, online bool, minutes int, reason, logAs string) banResult {
		mu.Lock()
		banned = append(banned, fmt.Sprintf("%s %d %s", p.GUID, minutes, reason))
		mu.Unlock()
		return banResult{OK: true}
	}
	warningActionDelay = 10 * time.Millisecond
	settings := defaultChatSettings()
	w := settings["warning"]
	w.Announce = false
	settings["warning"] = w
	chatState.Lock()
	chatState.settings, chatState.loaded = settings, true
	chatState.Unlock()
	take := func() (s, k, b []string) {
		mu.Lock()
		defer mu.Unlock()
		s, k, b, said, kicked, banned = said, kicked, banned, nil, nil, nil
		return
	}
	wait := func() { time.Sleep(100 * time.Millisecond) }

	handleChatCommand(mod, roleMod, "!warning 4 spamming chat")
	wait()
	s, k, b := take()
	if fmt.Sprint(s) != fmt.Sprint([]string{"4: Warning 1/2 from Mod: spamming chat", "1: Rowdy was warned by Mod (1/2): spamming chat"}) || k != nil || b != nil {
		t.Fatalf("first warning: said %q, kicked %q, banned %q", s, k, b)
	}
	handleChatCommand(mod, roleMod, "!warning #4 still spamming")
	wait()
	s, k, b = take()
	if len(s) != 3 || s[0] != "4: Warning 2/2 from Mod: still spamming" || s[2] != "1: Rowdy reached 2 warnings and has been banned: still spamming" {
		t.Fatalf("second warning said %q", s)
	}
	if fmt.Sprint(b) != fmt.Sprint([]string{testGUID + " 0 still spamming"}) || k != nil {
		t.Fatalf("at the threshold: banned %q, kicked %q; want a permanent ban with the last reason", b, k)
	}
	// The count starts again after the threshold.
	handleChatCommand(mod, roleMod, "!warning 4 again")
	wait()
	if s, _, b := take(); len(s) == 0 || s[0] != "4: Warning 1/2 from Mod: again" || b != nil {
		t.Fatalf("after the ban the count didn't start again: %q", s)
	}

	// Kick at a threshold of 1, by code; Players can't warn.
	w = settings["warning"]
	w.Threshold, w.Action = 1, "kick"
	settings["warning"] = w
	handleChatCommand(Player{ID: 6, GUID: testGUID2, Name: "Other"}, rolePlayer, "!warning 1 abuse")
	handleChatCommand(mod, roleMod, "!warning "+codeOf(testGUID2)+" teamkilling")
	wait()
	if _, k, b := take(); fmt.Sprint(k) != fmt.Sprint([]string{"6 teamkilling"}) || b != nil {
		t.Fatalf("threshold 1, kick: kicked %q, banned %q", k, b)
	}

	// !kick by player number.
	handleChatCommand(mod, roleMod, "!kick 4 bye")
	if _, k, _ := take(); fmt.Sprint(k) != fmt.Sprint([]string{"4 bye"}) {
		t.Fatalf("!kick 4: kicked %q", k)
	}
}

func TestWarningSettingsChecked(t *testing.T) {
	for _, c := range []struct {
		threshold int
		action    string
		ok        bool
	}{{2, "ban", true}, {1, "kick", true}, {0, "ban", false}, {-1, "ban", false}, {101, "ban", false}, {2, "mute", false}} {
		s := defaultChatSettings()
		w := s["warning"]
		w.Threshold, w.Action = c.threshold, c.action
		s["warning"] = w
		if err := checkChatSettings(s); (err == nil) != c.ok {
			t.Errorf("threshold %d, action %q: err = %v", c.threshold, c.action, err)
		}
	}
}

// pingCounterFlow is the ping-warning example: ++ on a high ping, kick at
// the threshold, otherwise a warning; reset when the ping is fine.
func pingCounterFlow() []Step {
	inner := Step{Type: "if", Branches: []Branch{{Subject: "counter", Op: "ge", Value: "{threshold}",
		Steps: []Step{{Type: "kick", Who: "player", Reason: "Ping {player_ping} over the limit"}}}},
		Else: &[]Step{{Type: "message", To: "player", Text: "Warning {counter}/{threshold}"}}}
	return []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if",
		Branches:    []Branch{{Subject: "player_ping", Op: "ge", Value: "500", Counter: "inc", Steps: []Step{inner}}},
		Else:        &[]Step{},
		ElseCounter: "reset"}}}}
}

// TestFlowCounter checks the per-player counter of a run: ++ on each high
// ping, reset when it's fine, {counter} and {threshold} in messages
// and Ifs, and a fresh run starting at 0.
func TestFlowCounter(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	if err := checkFlowSteps(triggerSchedule, 0, pingCounterFlow(), nil); err == nil || !strings.Contains(err.Error(), "Counter threshold") {
		t.Fatalf("{threshold} without a Counter threshold: err = %v", err)
	}
	if err := checkFlowSteps(triggerSchedule, 3, pingCounterFlow(), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &flowRun{name: "ping", ctx: ctx, cancel: cancel, counterThreshold: 3}
	x := &job{run: run, ctx: ctx}
	pass := func(lagPing, flakyPing int) []string {
		sent = nil
		setOnline(Player{ID: 4, GUID: testGUID, Name: "Laggy", Ping: lagPing}, Player{ID: 6, GUID: testGUID2, Name: "Flaky", Ping: flakyPing})
		if !x.execFlowSteps(pingCounterFlow()) {
			t.Fatal("the run ended early")
		}
		return sent
	}
	steps := [][2]int{{600, 700}, {650, 40}, {700, 800}, {700, 900}}
	want := [][]string{
		{"say 4 Warning 1/3", "say 6 Warning 1/3"},
		{"say 4 Warning 2/3"},                                   // Flaky's ping is fine: reset
		{"kick 4 Ping 700 over the limit", "say 6 Warning 1/3"}, // Laggy's 3rd in a row
		{"kick 4 Ping 700 over the limit", "say 6 Warning 2/3"}, // still over: the counter stays past the threshold
	}
	for i, p := range steps {
		if got := pass(p[0], p[1]); fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Fatalf("pass %d sent %q, want %q", i+1, got, want[i])
		}
	}
	if !strings.Contains(strings.Join(run.log, "\n"), "Laggy (#4): counter ++, now 3/3") {
		t.Fatalf("run log:\n%s", strings.Join(run.log, "\n"))
	}

	// A new run starts every counter at 0.
	run2 := &flowRun{name: "ping", ctx: ctx, cancel: cancel, counterThreshold: 3}
	x = &job{run: run2, ctx: ctx}
	if got := pass(600, 40); fmt.Sprint(got) != fmt.Sprint([]string{"say 4 Warning 1/3"}) {
		t.Fatalf("new run sent %q", got)
	}
}

func TestFlowCounterChecked(t *testing.T) {
	// A counter needs a player: not outside a For each in a Schedule flow.
	top := []Step{{Type: "if", Branches: []Branch{{Subject: "players_online", Op: "gt", Value: "0", Counter: "inc", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerSchedule, 0, top, nil); err == nil || !strings.Contains(err.Error(), "needs a player") {
		t.Fatalf("counter without a player: err = %v", err)
	}
	if err := checkFlowSteps(triggerJoin, 0, top, nil); err != nil {
		t.Fatalf("counter in a join flow: %v", err)
	}
	bad := []Step{{Type: "if", Branches: []Branch{{Subject: "player_ping", Op: "gt", Value: "0", Counter: "double", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerJoin, 0, bad, nil); err == nil {
		t.Fatal("an unknown counter change was accepted")
	}
	word := []Step{{Type: "if", Branches: []Branch{{Subject: "player_ping", Op: "gt", Value: "{player_ping}", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerJoin, 0, word, nil); err == nil {
		t.Fatal("a number If compared with a placeholder other than {threshold}")
	}
	for _, n := range []int{-2, 1001} {
		f := Flow{Name: "x", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}, CounterThreshold: n}
		if err := checkFlowDefinition(f); err == nil {
			t.Errorf("counter threshold %d accepted", n)
		}
	}
	// No threshold (0) is fine, and so is a counter compared with a number.
	noThreshold := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if",
		Branches: []Branch{{Subject: "counter", Op: "ge", Value: "3", Counter: "inc", Steps: []Step{}}}}}}}
	if err := checkFlowDefinition(Flow{Name: "x", Trigger: triggerManual, Steps: noThreshold}); err != nil {
		t.Errorf("no threshold: %v", err)
	}
	msg := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "{counter}/{threshold}"}}}}
	if err := checkFlowSteps(triggerManual, 0, msg, nil); err == nil {
		t.Error("{threshold} in a message accepted without a Counter threshold")
	}
	if err := checkFlowSteps(triggerManual, 2, msg, nil); err != nil {
		t.Errorf("{threshold} with a threshold: %v", err)
	}
}

// TestFlowCounterThresholdStored checks the threshold is saved and read
// back, is none (0) when not sent, and that a flows table from before
// counters gets the column.
func TestFlowCounterThresholdStored(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`CREATE TABLE flows (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, script TEXT NOT NULL,
		cancel TEXT NOT NULL DEFAULT '', timer TEXT NOT NULL DEFAULT '', schedule TEXT NOT NULL DEFAULT '',
		schedule_enabled INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO flows (id, name, script, schedule) VALUES (0, 'old', '[{"type":"lock"}]', 'manual')`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	if f, err := db.GetStoredFlow(0); err != nil || f.CounterThreshold != 0 {
		t.Fatalf("old flow: threshold %d, err %v; want none", f.CounterThreshold, err)
	}
	id, err := validateAndSaveFlow(Flow{Name: "four", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}, CounterThreshold: 4}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := db.GetStoredFlow(id); f.CounterThreshold != 4 {
		t.Fatalf("saved threshold %d, want 4", f.CounterThreshold)
	}
	id, err = validateAndSaveFlow(Flow{Name: "unsent", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := db.GetStoredFlow(id); f.CounterThreshold != 0 {
		t.Fatalf("threshold not sent: %d, want none", f.CounterThreshold)
	}
}

// TestStopEndsWhenStopped checks that the When stopped steps run after Stop,
// and that pressing Stop again ends them, even during a Wait.
func TestStopEndsWhenStopped(t *testing.T) {
	testDB(t)
	var mu sync.Mutex
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { mu.Lock(); sent = append(sent, cmd); mu.Unlock() }
	t.Cleanup(func() { sendCommand = oldSend })
	f := Flow{ID: 77, Name: "restart", Trigger: triggerManual,
		Steps:  []Step{{Type: "wait", Time: "1h"}},
		OnStop: []Step{{Type: "unlock"}, {Type: "wait", Time: "1h"}, {Type: "lock"}}}
	if err := launchFlowRun(f, time.Now(), "test", nil); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatal(what)
	}
	if err := stopFlowRun(f.ID); err != nil {
		t.Fatal(err)
	}
	waitFor("When stopped didn't start", func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) == 1 && sent[0] == "#unlock" })
	if err := stopFlowRun(f.ID); err != nil { // still running: the When stopped Wait
		t.Fatalf("second Stop: %v", err)
	}
	waitFor("the second Stop didn't end the run", func() bool {
		flowRuns.Lock()
		defer flowRuns.Unlock()
		return !flowRuns.byFlow[f.ID].active
	})
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(sent) != fmt.Sprint([]string{"#unlock"}) {
		t.Fatalf("sent %q; the Lock after the Wait should not go out", sent)
	}
}
