package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func InitDB(filepath string) (*DB, error) {
	db, err := sql.Open("sqlite", filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to apply pragma %q: %w", pragma, err)
		}
	}
	schema := `
	CREATE TABLE IF NOT EXISTS players (
		guid TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		last_ip TEXT NOT NULL,
		first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		total_playtime_seconds INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS active_sessions (
		guid TEXT PRIMARY KEY,
		ip TEXT NOT NULL,
		connected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS session_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guid TEXT NOT NULL,
		ip TEXT NOT NULL,
		connected_at DATETIME NOT NULL,
		disconnected_at DATETIME NOT NULL,
		duration_seconds INTEGER NOT NULL,
		FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS bans (
		guid TEXT PRIMARY KEY,
		player_name TEXT NOT NULL,
		ip TEXT NOT NULL,
		reason TEXT NOT NULL,
		perm INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_session_history_guid
		ON session_history(guid);
	CREATE INDEX IF NOT EXISTS idx_bans_player_name
		ON bans(player_name);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	return &DB{db}, nil
}

func (db *DB) SyncPlayers(currentPlayers []webPlayer) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	activeMap := make(map[string]webPlayer, len(currentPlayers))
	for _, p := range currentPlayers {
		if p.GUID == "" {
			continue
		}
		activeMap[p.GUID] = p
	}
	rows, err := tx.Query(
		"SELECT guid, ip, connected_at FROM active_sessions",
	)
	if err != nil {
		return err
	}
	type existingSession struct {
		guid        string
		ip          string
		connectedAt time.Time
	}
	dbSessions := make(map[string]existingSession)
	for rows.Next() {
		var s existingSession
		if err := rows.Scan(
			&s.guid,
			&s.ip,
			&s.connectedAt,
		); err != nil {
			rows.Close()
			return err
		}
		dbSessions[s.guid] = s
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for guid, sess := range dbSessions {
		if _, stillOnline := activeMap[guid]; !stillOnline {
			duration := int(
				now.Sub(sess.connectedAt).Seconds(),
			)
			if duration < 0 {
				duration = 0
			}
			_, err = tx.Exec(`
				INSERT INTO session_history (
					guid,
					ip,
					connected_at,
					disconnected_at,
					duration_seconds
				)
				VALUES (?, ?, ?, ?, ?)`,
				guid,
				sess.ip,
				sess.connectedAt,
				now,
				duration,
			)
			if err != nil {
				return err
			}
			_, err = tx.Exec(`
				UPDATE players
				SET
					last_seen = ?,
					total_playtime_seconds =
						total_playtime_seconds + ?
				WHERE guid = ?`,
				now,
				duration,
				guid,
			)
			if err != nil {
				return err
			}
			_, err = tx.Exec(
				"DELETE FROM active_sessions WHERE guid = ?",
				guid,
			)
			if err != nil {
				return err
			}
		}
	}
	for guid, p := range activeMap {
		_, err = tx.Exec(`
			INSERT INTO players (
				guid,
				username,
				last_ip,
				first_seen,
				last_seen
			)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(guid) DO UPDATE SET
				username = excluded.username,
				last_ip = excluded.last_ip,
				last_seen = excluded.last_seen`,
			guid,
			p.Name,
			p.IP,
			now,
			now,
		)
		if err != nil {
			return err
		}
		if _, exists := dbSessions[guid]; !exists {
			_, err = tx.Exec(`
				INSERT INTO active_sessions (
					guid,
					ip,
					connected_at
				)
				VALUES (?, ?, ?)`,
				guid,
				p.IP,
				now,
			)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (db *DB) BanUser(
	guid string,
	playerName string,
	ip string,
	reason string,
	perm int,
) error {
	_, err := db.Exec(`
		INSERT INTO bans (
			guid,
			player_name,
			ip,
			reason,
			perm
		)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(guid) DO UPDATE SET
			player_name = excluded.player_name,
			ip = excluded.ip,
			reason = excluded.reason,
			perm = excluded.perm`,
		guid,
		playerName,
		ip,
		reason,
		perm,
	)
	return err
}

// SyncBans mirrors the BattlEye GUID ban list into the bans table:
// bans missing from the DB are inserted (name/IP looked up from players),
// existing rows get their reason/perm refreshed, and rows no longer in
// BattlEye (expired or removed) are deleted. Callers must pass a list
// parsed from a complete, recognised `bans` reply.
func (db *DB) SyncBans(bans []webBan) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	live := make(map[string]struct{}, len(bans))
	for _, b := range bans {
		g := strings.ToLower(b.GUID)
		if _, dup := live[g]; dup {
			continue // BattlEye can list the same GUID more than once
		}
		live[g] = struct{}{}
		perm := 0
		if b.Perm {
			perm = 1
		}
		_, err := tx.Exec(`
			INSERT INTO bans (guid, player_name, ip, reason, perm)
			VALUES (
				?,
				COALESCE((SELECT username FROM players WHERE guid = ?), 'Unknown'),
				COALESCE((SELECT last_ip FROM players WHERE guid = ?), ''),
				?, ?
			)
			ON CONFLICT(guid) DO UPDATE SET
				reason = excluded.reason,
				perm = excluded.perm`,
			g, g, g, b.Reason, perm,
		)
		if err != nil {
			return err
		}
	}

	rows, err := tx.Query("SELECT guid FROM bans")
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return err
		}
		if _, ok := live[strings.ToLower(g)]; !ok {
			stale = append(stale, g)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, g := range stale {
		if _, err := tx.Exec("DELETE FROM bans WHERE guid = ?", g); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BanNames returns guid (lowercase) -> player_name for every row in bans.
func (db *DB) BanNames() (map[string]string, error) {
	rows, err := db.Query("SELECT guid, player_name FROM bans")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var g, n string
		if err := rows.Scan(&g, &n); err != nil {
			return nil, err
		}
		names[strings.ToLower(g)] = n
	}
	return names, rows.Err()
}

// DeleteBan removes a ban row by GUID (case-insensitive).
func (db *DB) DeleteBan(guid string) error {
	_, err := db.Exec("DELETE FROM bans WHERE lower(guid) = lower(?)", guid)
	return err
}
