package flows

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/store/storetest"
)

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
		{"INSERT INTO players (guid, username, last_ip, first_seen, last_seen, total_playtime_seconds, role) VALUES (?, 'Survivor', '1.2.3.4', ?, ?, 3600, 1)", []any{storetest.GUID1, t0, t0}},
		{"INSERT INTO players (guid, username, last_ip, first_seen, last_seen) VALUES ('-', 'New Guy (Lobby)', '5.6.7.8', ?, ?)", []any{t0, t0}},
		{"INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds) VALUES (?, '1.2.3.4', ?, ?, 3600)", []any{storetest.GUID1, t0, t0.Add(time.Hour)}},
		{"INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds) VALUES ('-', '5.6.7.8', ?, ?, 60)", []any{t0, t0.Add(time.Minute)}},
		{"INSERT INTO player_ips (guid, ip, first_seen, last_seen) VALUES ('-', '5.6.7.8', ?, ?)", []any{t0, t0}},
		{"INSERT INTO active_sessions (guid, ip, connected_at) VALUES (?, '1.2.3.4', ?)", []any{storetest.GUID1, t0}},
		{"INSERT INTO chat_messages (at, guid, direction, line) VALUES (?, '-', 'in', '(Global) New Guy: hi')", []any{t0}},
		{"INSERT INTO bans (guid, player_name, ip, reason, perm) VALUES (?, 'Cheater', '9.9.9.9', 'aimbot', 1)", []any{storetest.GUID2}},
		{"INSERT INTO bans (guid, player_name, ip, reason, perm) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'Toxic', '8.8.8.8', 'spam', 0)", nil},
	} {
		if _, err := old.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	old.Close()

	db, err := store.OpenPlayerDatabase(path)
	if err != nil {
		t.Fatalf("opening the old database: %v", err)
	}
	defer db.Close()
	SetDatabase(db)
	if err := Init(); err != nil {
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
	d, err := db.GetPlayerDetails(storetest.GUID1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Player.PlaytimeSeconds != 3600 || d.Player.Role != roleMod || d.Sessions.Count != 1 {
		t.Fatalf("details after upgrade = %+v", d)
	}
	// An old open session (no player number, no loaded_at) closes normally.
	if _, err := db.ClosePlayerSession(storetest.GUID1, t0.Add(30*time.Minute), store.EndDisconnected); err != nil {
		t.Fatal(err)
	}
	if h := storetest.History(t, db, storetest.GUID1); len(h) != 2 || h[1].Duration != 1800 || h[1].Loaded || h[1].Ended != store.EndDisconnected {
		t.Fatalf("history after upgrade = %+v", h)
	}
	// Old bans: the permanent one stays; the timed one is dropped from the
	// table (timed bans are kept in memory, from BattlEye's list).
	states, err := db.AllBanStates()
	if err != nil {
		t.Fatal(err)
	}
	if st := states[storetest.GUID2]; !st.Banned || !st.Perm {
		t.Errorf("permanent ban = %+v", st)
	}
	if st := states["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]; st.Banned {
		t.Errorf("timed ban still stored: %+v", st)
	}
	if n := storetest.CountRows(t, db, "bans"); n != 1 {
		t.Errorf("%d rows in bans, want 1", n)
	}
	// Opening it again (the next restart) changes nothing and doesn't fail.
	db2, err := store.OpenPlayerDatabase(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
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
	if f, err := Get(5); err != nil || f.Name != "MOTD" || !f.ScheduleEnabled || f.Trigger != triggerSchedule || len(f.Steps) != 1 {
		t.Fatalf("flow #5 = %+v, %v", f, err)
	}
	// New flows take the free numbers from 0.
	if id, err := Save(testFlow("new"), true); err != nil || id != 0 {
		t.Fatalf("new flow: #%d, %v", id, err)
	}
	if err := db.EnsureFlowsTable(); err != nil { // a later start: nothing to do
		t.Fatal(err)
	}
}
