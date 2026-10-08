package main

import (
	"database/sql"
	"fmt"
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
		last_name TEXT NOT NULL,
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

	CREATE INDEX IF NOT EXISTS idx_session_history_guid ON session_history(guid);
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

	rows, err := tx.Query("SELECT guid, ip, connected_at FROM active_sessions")
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
    	if err := rows.Scan(&s.guid, &s.ip, &s.connectedAt); err != nil {
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
			duration := int(now.Sub(sess.connectedAt).Seconds())
			if duration < 0 {
				duration = 0
			}

			_, err = tx.Exec(`
				INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds)
				VALUES (?, ?, ?, ?, ?)`,
				guid, sess.ip, sess.connectedAt, now, duration,
			)
			if err != nil {
				return err
			}

			_, err = tx.Exec(`
				UPDATE players 
				SET last_seen = ?, total_playtime_seconds = total_playtime_seconds + ?
				WHERE guid = ?`,
				now, duration, guid,
			)
			if err != nil {
				return err
			}

			_, err = tx.Exec("DELETE FROM active_sessions WHERE guid = ?", guid)
			if err != nil {
				return err
			}
		}
	}

	for guid, p := range activeMap {
		_, err = tx.Exec(`
			INSERT INTO players (guid, last_name, last_ip, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(guid) DO UPDATE SET
				last_name = excluded.last_name,
				last_ip = excluded.last_ip,
				last_seen = excluded.last_seen`,
			guid, p.Name, p.IP, now, now,
		)
		if err != nil {
			return err
		}

		if _, exists := dbSessions[guid]; !exists {
			_, err = tx.Exec(`
				INSERT INTO active_sessions (guid, ip, connected_at)
				VALUES (?, ?, ?)`,
				guid, p.IP, now,
			)
			if err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}