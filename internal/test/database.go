package test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/store"
)

// Two valid BattlEye GUIDs for tests.
const (
	GUID1 = "0123456789abcdef0123456789abcdef"
	GUID2 = "fedcba9876543210fedcba9876543210"
)

// OpenDB opens a fresh database in a temp folder, closed when the test ends.
func OpenDB(t testing.TB) *store.Database {
	t.Helper()
	db, err := store.OpenPlayerDatabase(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// CountRows is the number of rows in table.
func CountRows(t testing.TB, db *store.Database, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Session is one row of session_history.
type Session struct {
	Ended         string
	Duration      int64
	Loaded        bool
	Connected, At time.Time
}

// History is a player's finished sessions, oldest first.
func History(t testing.TB, db *store.Database, guid string) []Session {
	t.Helper()
	rows, err := db.Query("SELECT ended, duration_seconds, loaded_at IS NOT NULL, connected_at, disconnected_at FROM session_history WHERE guid = ? ORDER BY id", guid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var r Session
		if err := rows.Scan(&r.Ended, &r.Duration, &r.Loaded, &r.Connected, &r.At); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// LastSeen is a player's last_seen.
func LastSeen(t testing.TB, db *store.Database, guid string) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow("SELECT last_seen FROM players WHERE guid = ?", guid).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}
