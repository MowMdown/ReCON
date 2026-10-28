// Package store is the SQLite player database. Every SQL statement in ReCON
// lives here:
//
//   - schema setup and upgrades (OpenPlayerDatabase, EnsureFlowsTable)
//   - players, sessions and IP history (OpenPlayerSession, ClosePlayerSession, ...)
//   - player details and admin notes (GetPlayerDetails, SavePlayerNote)
//   - chat history (AddChatMessage, GetChatHistory)
//   - SteamIDs (SteamIDByGUID, PlayersBySteamIDs, ListPendingSteamGUIDs, SetSteamID)
//   - roles for the chat commands (GetPlayerRole, SetPlayerRole, ...)
//   - settings, such as the chat commands' (GetStoredSetting, SaveStoredSetting)
//   - bans: permanent ones in the bans table, timed ones in memory
//     (RecordPlayerBan, SyncBansFromServer, ExpireTimedBans, ListAllBans, ...)
//   - flows (ListStoredFlows, GetStoredFlow, SaveStoredFlow, ...), as
//     FlowRecords with the steps as JSON
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Database struct {
	*sql.DB
	timed timedBans // timed bans live here only, never in the bans table
}

// sqliteParams are applied by the driver to every connection it opens.
// database/sql keeps a pool of connections, and busy_timeout and foreign_keys
// only last for the connection they were set on, so setting them with one
// Exec would leave the others without them. _txlock=immediate takes the write
// lock when a transaction begins, so a transaction that reads and then writes
// waits its turn instead of failing with "database is locked".
const sqliteParams = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"

func OpenPlayerDatabase(filepath string) (*Database, error) {
	db, err := sql.Open("sqlite", filepath+sqliteParams)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	schema := `
	CREATE TABLE IF NOT EXISTS players (
		guid TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		last_ip TEXT NOT NULL,
		first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		total_playtime_seconds INTEGER NOT NULL DEFAULT 0,
		-- Role for the chat commands: 0 Player (everyone starts here), 1 Mod, 2 Admin.
		role INTEGER NOT NULL DEFAULT 0,
		-- SteamID64 as decimal text, worked out from the GUID by the Steam
		-- Profile tab's search (steam.go). NULL until then.
		steam_id TEXT
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
		perm INTEGER NOT NULL DEFAULT 0,
		-- As players.steam_id. Copied from players when the ban is recorded,
		-- else NULL until a search finds it.
		steam_id TEXT
	);
	CREATE TABLE IF NOT EXISTS player_ips (
		guid TEXT NOT NULL,
		ip TEXT NOT NULL,
		first_seen DATETIME NOT NULL,
		last_seen DATETIME NOT NULL,
		PRIMARY KEY (guid, ip),
		FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
	);
	-- Each player's chat lines and the private messages ReCON sent them,
	-- by GUID (lower case). direction is 'in' (they wrote it) or 'out'.
	CREATE TABLE IF NOT EXISTS chat_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at DATETIME NOT NULL,
		guid TEXT NOT NULL,
		direction TEXT NOT NULL,
		line TEXT NOT NULL
	);
	-- One admin note per player (the Details dialog's Notes tab), by GUID
	-- (lower case), up to maxNoteLen characters.
	CREATE TABLE IF NOT EXISTS player_notes (
		guid TEXT PRIMARY KEY,
		note TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);
	-- Small settings kept as text by key (the chat commands' settings are
	-- one JSON value under 'chat_commands').
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_session_history_guid
		ON session_history(guid);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_guid
		ON chat_messages(guid, id);
	-- The same names the old guidsearch tool used, so databases it touched
	-- don't get a second index.
	CREATE INDEX IF NOT EXISTS idx_players_steam_id
		ON players(steam_id);
	CREATE INDEX IF NOT EXISTS idx_bans_steam_id
		ON bans(steam_id);
	-- GUID lookups are written as lower(guid) = lower(?) so case never
	-- matters; these let them use an index instead of reading every row.
	CREATE INDEX IF NOT EXISTS idx_players_guid_lower
		ON players(lower(guid));
	CREATE INDEX IF NOT EXISTS idx_bans_guid_lower
		ON bans(lower(guid));

	-- Read-only views with friendlier column names. The tables above are
	-- what the app writes to; these are just for browsing in sqlite3.
	DROP VIEW IF EXISTS view_players;
	CREATE VIEW view_players AS
		SELECT
			username AS player_name,
			guid AS player_guid,
			steam_id AS steam_id,
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
			steam_id AS steam_id,
			ip AS ip_address_when_banned,
			reason AS ban_reason,
			CASE perm WHEN 1 THEN 'Permanent' ELSE 'Timed' END AS ban_type
		FROM bans;
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS
	// leaves an existing table as it is, so they are added here when missing.
	for _, c := range []struct{ table, column, decl string }{
		// The player's number while connected, and when "(Lobby)" left their name.
		{"active_sessions", "player_id", "INTEGER NOT NULL DEFAULT -1"},
		{"active_sessions", "loaded_at", "DATETIME"},
		{"session_history", "loaded_at", "DATETIME"},
		// How the session ended: disconnected, kicked or banned ('' before this was kept).
		{"session_history", "ended", "TEXT NOT NULL DEFAULT ''"},
		// Seconds of the session the server was down for (a restart the
		// player came back from): not counted in its length or playtime.
		{"active_sessions", "paused_seconds", "INTEGER NOT NULL DEFAULT 0"},
		{"session_history", "paused_seconds", "INTEGER NOT NULL DEFAULT 0"},
		// A permanent ban's position in BattlEye's list.
		{"bans", "be_index", "INTEGER NOT NULL DEFAULT -1"},
	} {
		if err := addColumnIfMissing(db, c.table, c.column, c.decl); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to add %s.%s: %w", c.table, c.column, err)
		}
	}
	// Earlier releases stored players BattlEye hadn't verified yet under the
	// GUID "-" (every such player merged into one row). That isn't a player:
	// drop it, with its sessions and IPs (ON DELETE CASCADE), chat and note.
	// Does nothing once they're gone.
	for _, q := range []string{
		"DELETE FROM players WHERE guid IN ('-', '')",
		"DELETE FROM chat_messages WHERE guid IN ('-', '')",
		"DELETE FROM player_notes WHERE guid IN ('-', '')",
		// Timed bans are kept in memory only now (timedBans): BattlEye's
		// list has them, and the first read after startup fills them in.
		"DELETE FROM bans WHERE perm = 0",
		// RCON Settings used to be saved here; they are memory only now.
		"DELETE FROM settings WHERE key = 'rcon'",
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to clean up old rows: %w", err)
		}
	}
	return &Database{DB: db, timed: timedBans{m: map[string]timedBan{}}}, nil
}

// addColumnIfMissing adds a column to an existing table unless it is there.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}

// ClearActiveSessions drops sessions left open by a previous run so restart
// gaps aren't counted as playtime. Called once at server start.
func (db *Database) ClearActiveSessions() error {
	_, err := db.Exec("DELETE FROM active_sessions")
	return err
}
