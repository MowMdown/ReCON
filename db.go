package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Database struct {
	*sql.DB
}

func OpenDatabase(filepath string) (*Database, error) {
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
	CREATE TABLE IF NOT EXISTS player_ips (
		guid TEXT NOT NULL,
		ip TEXT NOT NULL,
		first_seen DATETIME NOT NULL,
		last_seen DATETIME NOT NULL,
		PRIMARY KEY (guid, ip),
		FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_session_history_guid
		ON session_history(guid);
	DROP INDEX IF EXISTS idx_bans_player_name;

	-- Read-only views with friendlier column names. The tables above are
	-- what the app writes to; these are just for browsing in sqlite3.
	DROP VIEW IF EXISTS view_players;
	CREATE VIEW view_players AS
		SELECT
			username AS player_name,
			guid AS player_guid,
			last_ip AS last_ip_address,
			first_seen AS first_joined,
			last_seen AS last_seen,
			total_playtime_seconds AS playtime_seconds,
			printf('%dh %02dm', total_playtime_seconds / 3600, (total_playtime_seconds % 3600) / 60) AS playtime
		FROM players;

	DROP VIEW IF EXISTS view_online;
	CREATE VIEW view_online AS
		SELECT
			p.username AS player_name,
			a.guid AS player_guid,
			a.ip AS ip_address,
			a.connected_at AS connected_at
		FROM active_sessions a
		JOIN players p ON p.guid = a.guid;

	DROP VIEW IF EXISTS view_sessions;
	CREATE VIEW view_sessions AS
		SELECT
			s.id AS session_id,
			p.username AS player_name,
			s.guid AS player_guid,
			s.ip AS ip_address,
			s.connected_at AS connected_at,
			s.disconnected_at AS disconnected_at,
			s.duration_seconds AS duration_seconds,
			printf('%dh %02dm', s.duration_seconds / 3600, (s.duration_seconds % 3600) / 60) AS duration
		FROM session_history s
		JOIN players p ON p.guid = s.guid;

	DROP VIEW IF EXISTS view_player_ips;
	CREATE VIEW view_player_ips AS
		SELECT
			p.username AS player_name,
			i.guid AS player_guid,
			i.ip AS ip_address,
			i.first_seen AS first_used,
			i.last_seen AS last_used
		FROM player_ips i
		JOIN players p ON p.guid = i.guid;

	DROP VIEW IF EXISTS view_bans;
	CREATE VIEW view_bans AS
		SELECT
			player_name AS player_name,
			guid AS player_guid,
			ip AS ip_address_when_banned,
			reason AS ban_reason,
			CASE perm WHEN 1 THEN 'Permanent' ELSE 'Timed' END AS ban_type
		FROM bans;
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	// Seed player_ips with every IP already recorded in session history.
	// Safe to repeat: existing (guid, ip) rows are left alone.
	_, err = db.Exec(`
		INSERT OR IGNORE INTO player_ips (guid, ip, first_seen, last_seen)
		SELECT guid, ip, MIN(connected_at), MAX(disconnected_at)
		FROM session_history
		GROUP BY guid, ip`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to seed player_ips: %w", err)
	}
	return &Database{db}, nil
}

func (db *Database) SyncPlayers(currentPlayers []Player) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	activeMap := make(map[string]Player, len(currentPlayers))
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
		// Remember every IP this player has connected from.
		_, err = tx.Exec(`
			INSERT INTO player_ips (guid, ip, first_seen, last_seen)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(guid, ip) DO UPDATE SET
				last_seen = excluded.last_seen`,
			guid,
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

func (db *Database) BanUser(
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
func (db *Database) SyncBans(bans []Ban) error {
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
func (db *Database) BanNames() (map[string]string, error) {
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
func (db *Database) DeleteBan(guid string) error {
	_, err := db.Exec("DELETE FROM bans WHERE lower(guid) = lower(?)", guid)
	return err
}
