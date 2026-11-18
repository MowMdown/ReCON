package store

import (
	"database/sql"
	"time"
)

// StartUptime opens a new uptime interval (the server answering, or not)
// and returns its id for ExtendUptime.
func (db *Database) StartUptime(up bool, at time.Time) (int64, error) {
	res, err := db.Exec("INSERT INTO server_uptime (started, ended, up) VALUES (?, ?, ?)", at, at, up)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ExtendUptime moves an interval's end to at.
func (db *Database) ExtendUptime(id int64, at time.Time) error {
	_, err := db.Exec("UPDATE server_uptime SET ended = ? WHERE id = ?", at, id)
	return err
}

// Uptime is how long the server answered and didn't between since and now
// (only what ReCON saw: times it wasn't running count as neither).
func (db *Database) Uptime(since, now time.Time) (up, down time.Duration, err error) {
	rows, err := db.Query("SELECT started, ended, up FROM server_uptime WHERE ended >= ? ORDER BY id", since)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, e time.Time
		var u bool
		if err := rows.Scan(&s, &e, &u); err != nil {
			return 0, 0, err
		}
		if s.Before(since) {
			s = since
		}
		if e.After(now) {
			e = now
		}
		if e.After(s) {
			if u {
				up += e.Sub(s)
			} else {
				down += e.Sub(s)
			}
		}
	}
	return up, down, rows.Err()
}

// Span is one player session's time on the server.
type Span struct {
	GUID       string
	Start, End time.Time
}

// SessionSpans is every session that overlaps since..now: the finished
// ones from session_history and the open ones (ending now).
func (db *Database) SessionSpans(since, now time.Time) ([]Span, error) {
	rows, err := db.Query(`
		SELECT guid, connected_at, disconnected_at FROM session_history WHERE disconnected_at >= ?
		UNION ALL
		SELECT guid, connected_at, NULL FROM active_sessions`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var sp Span
		var end sql.NullTime
		if err := rows.Scan(&sp.GUID, &sp.Start, &end); err != nil {
			return nil, err
		}
		sp.End = now
		if end.Valid {
			sp.End = end.Time
		}
		if sp.End.After(now) {
			sp.End = now
		}
		if sp.End.After(sp.Start) {
			out = append(out, sp)
		}
	}
	return out, rows.Err()
}

// FirstSessionStart is when the oldest session began (zero with none), for
// the population graph's All.
func (db *Database) FirstSessionStart() (time.Time, error) {
	// The oldest row of each table, read as the column itself: MIN() would
	// come back as text, which doesn't scan into a time.
	var first time.Time
	for _, table := range []string{"session_history", "active_sessions"} {
		var t time.Time
		err := db.QueryRow("SELECT connected_at FROM " + table + " ORDER BY connected_at LIMIT 1").Scan(&t)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first, nil
}

// ModUpdate is a mod's row in mod_updates.
type ModUpdate struct {
	WorkshopID   string
	Name         string
	SizeBytes    int64
	SteamUpdated int64 // Unix seconds
	AckedUpdated int64
}

// SaveModFromSteam records what Steam says about a mod. A mod seen for the
// first time is taken to be up to date (acknowledged as Steam's version).
func (db *Database) SaveModFromSteam(m ModUpdate) error {
	_, err := db.Exec(`
		INSERT INTO mod_updates (workshop_id, name, size_bytes, steam_updated, acked_updated)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workshop_id) DO UPDATE SET name = excluded.name,
			size_bytes = excluded.size_bytes, steam_updated = excluded.steam_updated`,
		m.WorkshopID, m.Name, m.SizeBytes, m.SteamUpdated, m.SteamUpdated)
	return err
}

// ModUpdates is every row of mod_updates, by Workshop ID.
func (db *Database) ModUpdates() (map[string]ModUpdate, error) {
	rows, err := db.Query("SELECT workshop_id, name, size_bytes, steam_updated, acked_updated FROM mod_updates")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ModUpdate{}
	for rows.Next() {
		var m ModUpdate
		if err := rows.Scan(&m.WorkshopID, &m.Name, &m.SizeBytes, &m.SteamUpdated, &m.AckedUpdated); err != nil {
			return nil, err
		}
		out[m.WorkshopID] = m
	}
	return out, rows.Err()
}

// AckModUpdate marks a mod's latest Steam update as seen. found is false
// for a mod not in the table.
func (db *Database) AckModUpdate(workshopID string) (found bool, err error) {
	res, err := db.Exec("UPDATE mod_updates SET acked_updated = steam_updated WHERE workshop_id = ?", workshopID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
