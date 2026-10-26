package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ============================================================================
// db.go overview
// ----------------------------------------------------------------------------
// Every SQL statement in the program lives in this file:
//
//   - schema setup (OpenPlayerDatabase) and the flows table (EnsureFlowsTable)
//   - players / sessions / IP history (OpenPlayerSession, ClosePlayerSession, ListOfflinePlayers, ...)
//   - player details and admin notes (GetPlayerDetails, SavePlayerNote)
//   - chat history (AddChatMessage, GetChatHistory)
//   - SteamIDs (SteamIDByGUID, PlayersBySteamIDs, ListPendingSteamGUIDs, SetSteamID)
//   - player roles for the chat commands (GetPlayerRole, SetPlayerRole, ...)
//   - settings, such as the chat commands' (Setting, SaveStoredSetting)
//   - bans (RecordPlayerBan, SyncBansFromServer, ExpireTimedBans, ListAllBans, AllBanStates, DeletePlayerBan)
//   - flows storage (ListStoredFlows, GetStoredFlow, SaveStoredFlow, ...): steps are kept as JSON
//
// The bottom section keeps the database in step with the game server: the
// BattlEye <-> DB ban sync (run by the server watch, rcon.go) and the chat
// recorder. Those are the only parts here that call into rcon.go. Player
// sessions are driven by the server's messages, in players.go.
// ============================================================================

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

// ---------------------------------------------------------------------------
// Players and sessions
// ---------------------------------------------------------------------------

// ClearActiveSessions drops sessions left open by a previous run so restart
// gaps aren't counted as playtime. Called once at server start.
func (db *Database) ClearActiveSessions() error {
	_, err := db.Exec("DELETE FROM active_sessions")
	return err
}

// Session endings, stored in session_history.ended.
const (
	endDisconnected = "disconnected"
	endKicked       = "kicked"
	endBanned       = "banned"
)

// OpenPlayerSession records a player coming online (players.go calls it once per
// session, when their GUID is known): their name and IP, the IP in their IP
// history, and an open session starting at connectedAt. A session already
// open for the GUID is left as it is.
func (db *Database) OpenPlayerSession(guid, name, ip string, playerID int, connectedAt time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := connectedAt.UTC()
	if _, err := tx.Exec(`
		INSERT INTO players (guid, username, last_ip, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(guid) DO UPDATE SET
			username = excluded.username,
			last_ip = excluded.last_ip,
			last_seen = excluded.last_seen`,
		guid, name, ip, at, at); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO player_ips (guid, ip, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(guid, ip) DO UPDATE SET
			last_seen = excluded.last_seen`,
		guid, ip, at, at); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO active_sessions (guid, ip, connected_at, player_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(guid) DO NOTHING`,
		guid, ip, at, playerID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSessionLoaded records when a player finished loading ("(Lobby)" left their name).
func (db *Database) MarkSessionLoaded(guid string, at time.Time) error {
	_, err := db.Exec("UPDATE active_sessions SET loaded_at = ? WHERE guid = ? AND loaded_at IS NULL", at.UTC(), guid)
	return err
}

// ClosePlayerSession ends a player's open session at `at`: it goes into
// session_history with how it ended, its length (less any paused time) is
// added to their playtime, and last_seen becomes `at`. It returns the
// history row's id, or 0 if no session was open.
func (db *Database) ClosePlayerSession(guid string, at time.Time, ended string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var ip string
	var connectedAt time.Time
	var loadedAt sql.NullTime
	var paused int
	err = tx.QueryRow("SELECT ip, connected_at, loaded_at, paused_seconds FROM active_sessions WHERE guid = ?", guid).
		Scan(&ip, &connectedAt, &loadedAt, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	at = at.UTC()
	duration := max(int(at.Sub(connectedAt).Seconds())-paused, 0)
	var loaded any // NULL when they never finished loading
	if loadedAt.Valid {
		loaded = loadedAt.Time.UTC()
	}
	res, err := tx.Exec(`
		INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds, loaded_at, ended, paused_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		guid, ip, connectedAt.UTC(), at, duration, loaded, ended, paused)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
		UPDATE players
		SET last_seen = ?, total_playtime_seconds = total_playtime_seconds + ?
		WHERE guid = ?`,
		at, duration, guid); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM active_sessions WHERE guid = ?", guid); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// ResumePlayerSession opens again a session ClosePlayerSession ended (history row
// historyID) when the server went down, for a player back at `at` after the
// restart: the history row goes, its length comes off their playtime, and
// the session is open again as player number playerID, with the time from
// the old end to `at` paused (not counted). It does nothing if the row is
// gone or the player already has a session open.
func (db *Database) ResumePlayerSession(guid string, historyID int64, playerID int, at time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ip string
	var connectedAt, endedAt time.Time
	var loadedAt sql.NullTime
	var duration, paused int
	err = tx.QueryRow(`SELECT ip, connected_at, disconnected_at, loaded_at, duration_seconds, paused_seconds
		FROM session_history WHERE id = ? AND guid = ?`, historyID, guid).
		Scan(&ip, &connectedAt, &endedAt, &loadedAt, &duration, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var loaded any
	if loadedAt.Valid {
		loaded = loadedAt.Time.UTC()
	}
	paused += max(int(at.Sub(endedAt).Seconds()), 0)
	res, err := tx.Exec(`
		INSERT INTO active_sessions (guid, ip, connected_at, player_id, loaded_at, paused_seconds)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(guid) DO NOTHING`,
		guid, ip, connectedAt.UTC(), playerID, loaded, paused)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // a session is already open
	}
	if _, err := tx.Exec("DELETE FROM session_history WHERE id = ?", historyID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE players
		SET last_seen = ?, total_playtime_seconds = max(total_playtime_seconds - ?, 0)
		WHERE guid = ?`,
		at.UTC(), duration, guid); err != nil {
		return err
	}
	return tx.Commit()
}

// ListOfflinePlayers returns all players from the database who are not currently connected.
func (db *Database) ListOfflinePlayers() ([]Player, error) {
	rows, err := db.Query(`
		SELECT p.username, p.last_ip, p.guid, p.total_playtime_seconds, p.last_seen, p.role
		FROM players p
		WHERE NOT EXISTS (
			SELECT 1 FROM active_sessions a WHERE a.guid = p.guid
		)
		ORDER BY p.username COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	players := []Player{}
	for rows.Next() {
		var p Player
		var lastSeen time.Time
		if err := rows.Scan(&p.Name, &p.IP, &p.GUID, &p.TotalPlaytimeSecs, &lastSeen, &p.Role); err != nil {
			return nil, err
		}
		p.LastSeenUnix = lastSeen.Unix()
		p.Offline = true
		players = append(players, p)
	}
	return players, rows.Err()
}

// ListKnownPlayers returns every player ever seen, sorted by name. Offline is
// false for players with an open session (online as of the last poll).
func (db *Database) ListKnownPlayers() ([]Player, error) {
	rows, err := db.Query(`
		SELECT p.username, p.last_ip, p.guid, p.total_playtime_seconds, p.last_seen,
			EXISTS (SELECT 1 FROM active_sessions a WHERE a.guid = p.guid)
		FROM players p
		ORDER BY p.username COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	players := []Player{}
	for rows.Next() {
		var p Player
		var lastSeen time.Time
		var online int
		if err := rows.Scan(&p.Name, &p.IP, &p.GUID, &p.TotalPlaytimeSecs, &lastSeen, &online); err != nil {
			return nil, err
		}
		p.LastSeenUnix = lastSeen.Unix()
		p.Offline = online == 0
		players = append(players, p)
	}
	return players, rows.Err()
}

// PlayerByGUID returns the stored name and last IP for a GUID
// (case-insensitive), or sql.ErrNoRows if that GUID was never seen.
func (db *Database) PlayerByGUID(guid string) (Player, error) {
	var p Player
	err := db.QueryRow("SELECT username, last_ip, guid FROM players WHERE lower(guid) = lower(?)", guid).
		Scan(&p.Name, &p.IP, &p.GUID)
	return p, err
}

// SteamIDByGUID returns a player's SteamID64 from players.steam_id ("" until
// a search has found it). known is false if the GUID was never seen.
func (db *Database) SteamIDByGUID(guid string) (steamID string, known bool, err error) {
	var s sql.NullString
	err = db.QueryRow("SELECT steam_id FROM players WHERE lower(guid) = lower(?)", guid).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(s.String), true, nil
}

// ListPendingSteamGUIDs returns every GUID (lower case) in players and bans that
// has no SteamID yet: what a SteamID search looks for. Malformed GUIDs are
// left out.
func (db *Database) ListPendingSteamGUIDs() ([]string, error) {
	rows, err := db.Query(`
		SELECT lower(guid) FROM players WHERE steam_id IS NULL OR steam_id = ''
		UNION
		SELECT lower(guid) FROM bans WHERE steam_id IS NULL OR steam_id = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		if guidPattern.MatchString(g) {
			out = append(out, g)
		}
	}
	return out, rows.Err()
}

// SetSteamID stores the SteamID found for a GUID in players and bans, in one
// transaction. Only empty steam_id fields are filled.
func (db *Database) SetSteamID(guid, steamID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"players", "bans"} {
		if _, err := tx.Exec("UPDATE "+table+" SET steam_id = ? WHERE lower(guid) = lower(?) AND (steam_id IS NULL OR steam_id = '')",
			steamID, guid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlayersBySteamIDs returns the known players whose steam_id is one of ids,
// sorted by name (the Steam Profile tab's friends who have played here).
func (db *Database) PlayersBySteamIDs(ids []string) ([]sharedPlayer, error) {
	out := []sharedPlayer{}
	const chunk = 500 // stay well under SQLite's limit on ? placeholders
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		rows, err := db.Query("SELECT guid, username FROM players WHERE steam_id IN (?"+
			strings.Repeat(",?", len(part)-1)+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p sharedPlayer
			if err := rows.Scan(&p.GUID, &p.Name); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// GUIDByName returns the GUID of the most recently seen player with this
// exact name, or "" if there is none.
func (db *Database) GUIDByName(name string) (string, error) {
	var guid string
	err := db.QueryRow("SELECT guid FROM players WHERE username = ? ORDER BY last_seen DESC LIMIT 1", name).Scan(&guid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return guid, err
}

// ---------------------------------------------------------------------------
// Roles (the chat commands' permissions, set on the Players page)
// ---------------------------------------------------------------------------

// GetPlayerRole returns a player's role (0 Player, 1 Mod, 2 Admin) by GUID,
// case-insensitive. Unknown players, and errors, are Player.
func (db *Database) GetPlayerRole(guid string) int {
	var role int
	if err := db.QueryRow("SELECT role FROM players WHERE lower(guid) = lower(?)", guid).Scan(&role); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("roles: reading %s: %v", guid, err)
		}
		return 0
	}
	return role
}

// SetPlayerRole stores a player's role. found is false if the GUID was never seen.
func (db *Database) SetPlayerRole(guid string, role int) (found bool, err error) {
	res, err := db.Exec("UPDATE players SET role = ? WHERE lower(guid) = lower(?)", role, guid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AllPlayerRoles returns lower-case GUID -> role for every Mod and Admin.
func (db *Database) AllPlayerRoles() (map[string]int, error) {
	rows, err := db.Query("SELECT guid, role FROM players WHERE role > 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var g string
		var r int
		if err := rows.Scan(&g, &r); err != nil {
			return nil, err
		}
		out[strings.ToLower(g)] = r
	}
	return out, rows.Err()
}

// PlayersByCode returns every known player whose GUID ends with code (the
// last 6 characters, what /code shows), for /ban on a player who has left.
func (db *Database) PlayersByCode(code string) ([]Player, error) {
	rows, err := db.Query("SELECT username, last_ip, guid, role FROM players WHERE substr(lower(guid), -6) = lower(?)", code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Player
	for rows.Next() {
		var p Player
		if err := rows.Scan(&p.Name, &p.IP, &p.GUID, &p.Role); err != nil {
			return nil, err
		}
		p.ID, p.Offline = -1, true
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// GetStoredSetting returns a stored setting; ok is false when it was never saved.
func (db *Database) GetStoredSetting(key string) (value string, ok bool, err error) {
	err = db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SaveStoredSetting stores a setting.
func (db *Database) SaveStoredSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// Player details and admin notes (the Players page's Details dialog)
// ---------------------------------------------------------------------------

// recentSessionsKept is how many past sessions GetPlayerDetails returns, newest first.
const recentSessionsKept = 100

type detailsPlayer struct {
	GUID            string `json:"guid"`
	Name            string `json:"name"`
	LastIP          string `json:"lastIp"`
	FirstSeen       int64  `json:"firstSeen"`
	LastSeen        int64  `json:"lastSeen"`
	PlaytimeSeconds int64  `json:"playtimeSeconds"`
	Online          bool   `json:"online"`
	ID              int    `json:"id"`   // player number while online, else -1
	Role            int    `json:"role"` // 0 Player, 1 Mod, 2 Admin
}

type detailsSession struct {
	Connected    int64  `json:"connected"`
	Disconnected int64  `json:"disconnected"` // 0 for the session in progress
	Seconds      int64  `json:"seconds"`
	IP           string `json:"ip"`
	Current      bool   `json:"current"`
	Ended        string `json:"ended,omitempty"` // disconnected, kicked or banned ("" for older sessions)
}

type sharedPlayer struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type detailsIP struct {
	IP        string         `json:"ip"`
	FirstUsed int64          `json:"firstUsed"`
	LastUsed  int64          `json:"lastUsed"`
	Others    []sharedPlayer `json:"others"` // other players seen on this IP
}

// playerDetails is everything stored about one player, as GET
// /api/players/{guid}/details returns it. Times are Unix seconds. Online, ID
// and Ban are filled in by the handler (web.go).
type playerDetails struct {
	Player   detailsPlayer `json:"player"`
	Sessions struct {
		Count      int              `json:"count"` // finished sessions
		AvgSeconds int64            `json:"avgSeconds"`
		Recent     []detailsSession `json:"recent"`
	} `json:"sessions"`
	IPs  []detailsIP `json:"ips"`
	Ban  banState    `json:"ban"`
	Note struct {
		Text    string `json:"text"`
		Updated int64  `json:"updated"`
	} `json:"note"`
}

// unixOf reads a DATETIME column scanned into `any`. Drivers hand these back
// as time.Time or as text depending on the driver and how the value was
// written, so both are handled. 0 when empty or unreadable.
func unixOf(v any) int64 {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	case int64:
		return t
	case []byte:
		return unixOf(string(t))
	case string:
		s := strings.TrimSpace(t)
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999 -0700 MST",
			"2006-01-02 15:04:05.999999999Z07:00",
			"2006-01-02T15:04:05.999999999Z07:00",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
		} {
			if p, err := time.Parse(layout, s); err == nil {
				return p.Unix()
			}
		}
		// Go's time.Time.String() adds " m=+1.23" when it carries a monotonic reading.
		if i := strings.Index(s, " m="); i > 0 {
			return unixOf(s[:i])
		}
	}
	return 0
}

// asString reads a TEXT column scanned into `any`.
func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

// GetPlayerDetails gathers everything stored about one player (GUID matched
// case-insensitively), or sql.ErrNoRows if the GUID was never seen.
func (db *Database) GetPlayerDetails(guid string) (*playerDetails, error) {
	d := &playerDetails{}
	var first, last any
	err := db.QueryRow(`SELECT guid, username, last_ip, first_seen, last_seen, total_playtime_seconds, role
		FROM players WHERE lower(guid) = lower(?)`, guid).
		Scan(&d.Player.GUID, &d.Player.Name, &d.Player.LastIP, &first, &last, &d.Player.PlaytimeSeconds, &d.Player.Role)
	if err != nil {
		return nil, err
	}
	g := d.Player.GUID // the stored spelling, for the other tables
	d.Player.FirstSeen, d.Player.LastSeen, d.Player.ID = unixOf(first), unixOf(last), -1

	var avg sql.NullFloat64
	if err := db.QueryRow("SELECT COUNT(*), AVG(duration_seconds) FROM session_history WHERE guid = ?", g).
		Scan(&d.Sessions.Count, &avg); err != nil {
		return nil, err
	}
	d.Sessions.AvgSeconds = int64(avg.Float64 + 0.5)

	// The session in progress (if any) first, then the finished ones.
	d.Sessions.Recent = []detailsSession{}
	var since, ip any
	var paused int64
	if err := db.QueryRow("SELECT connected_at, ip, paused_seconds FROM active_sessions WHERE guid = ?", g).Scan(&since, &ip, &paused); err == nil {
		c := unixOf(since)
		s := detailsSession{Connected: c, IP: asString(ip), Current: true}
		if c > 0 {
			s.Seconds = max(time.Now().Unix()-c-paused, 0)
		}
		d.Sessions.Recent = append(d.Sessions.Recent, s)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := db.Query(`SELECT connected_at, disconnected_at, duration_seconds, ip, ended
		FROM session_history WHERE guid = ? ORDER BY id DESC LIMIT ?`, g, recentSessionsKept)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c, x any
		var s detailsSession
		if err := rows.Scan(&c, &x, &s.Seconds, &s.IP, &s.Ended); err != nil {
			rows.Close()
			return nil, err
		}
		s.Connected, s.Disconnected = unixOf(c), unixOf(x)
		d.Sessions.Recent = append(d.Sessions.Recent, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	d.IPs = []detailsIP{}
	byIP := map[string]int{}
	rows, err = db.Query("SELECT ip, first_seen, last_seen FROM player_ips WHERE guid = ? ORDER BY last_seen DESC", g)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, b any
		var e detailsIP
		if err := rows.Scan(&e.IP, &a, &b); err != nil {
			rows.Close()
			return nil, err
		}
		e.FirstUsed, e.LastUsed, e.Others = unixOf(a), unixOf(b), []sharedPlayer{}
		byIP[e.IP] = len(d.IPs)
		d.IPs = append(d.IPs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Everyone else seen on any of those IPs.
	rows, err = db.Query(`
		SELECT DISTINCT other.ip, p.guid, p.username
		FROM player_ips mine
		JOIN player_ips other ON other.ip = mine.ip AND other.guid != mine.guid
		JOIN players p ON p.guid = other.guid
		WHERE mine.guid = ?
		ORDER BY p.username COLLATE NOCASE`, g)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ipText string
		var o sharedPlayer
		if err := rows.Scan(&ipText, &o.GUID, &o.Name); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byIP[ipText]; ok {
			d.IPs[i].Others = append(d.IPs[i].Others, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var upd any
	err = db.QueryRow("SELECT note, updated_at FROM player_notes WHERE guid = ?", strings.ToLower(g)).Scan(&d.Note.Text, &upd)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	d.Note.Updated = unixOf(upd)
	return d, nil
}

// IsPlayerKnown reports whether a GUID (case-insensitive) has ever been seen.
func (db *Database) IsPlayerKnown(guid string) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM players WHERE lower(guid) = lower(?)", guid).Scan(&n)
	return n > 0, err
}

// SavePlayerNote stores a player's admin note; an empty note deletes it.
func (db *Database) SavePlayerNote(guid, note string) error {
	guid = strings.ToLower(guid)
	if strings.TrimSpace(note) == "" {
		_, err := db.Exec("DELETE FROM player_notes WHERE guid = ?", guid)
		return err
	}
	_, err := db.Exec(`
		INSERT INTO player_notes (guid, note, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(guid) DO UPDATE SET note = excluded.note, updated_at = excluded.updated_at`,
		guid, note, time.Now().UTC())
	return err
}

// ---------------------------------------------------------------------------
// Chat history (each player's chat and the private messages sent to them)
// ---------------------------------------------------------------------------

// Chat directions.
const (
	chatIn  = "in"  // the player wrote it
	chatOut = "out" // ReCON sent it to them
)

// chatLine is one row of a player's history. Line is ready to show:
// "(Global) Name: text" for chat in, "> say 3 text" for messages out.
type chatLine struct {
	Time      time.Time `json:"time"`
	Direction string    `json:"direction"`
	Line      string    `json:"line"`
}

// AddChatMessage stores one line of a player's history.
func (db *Database) AddChatMessage(at time.Time, guid, direction, line string) error {
	_, err := db.Exec("INSERT INTO chat_messages (at, guid, direction, line) VALUES (?, ?, ?, ?)",
		at.UTC(), strings.ToLower(guid), direction, line)
	return err
}

// GetChatHistory returns a player's newest `limit` lines, oldest first.
func (db *Database) GetChatHistory(guid string, limit int) ([]chatLine, error) {
	rows, err := db.Query(`
		SELECT at, direction, line FROM (
			SELECT id, at, direction, line FROM chat_messages
			WHERE guid = ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, strings.ToLower(guid), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []chatLine{}
	for rows.Next() {
		var l chatLine
		var at any
		if err := rows.Scan(&at, &l.Direction, &l.Line); err != nil {
			return nil, err
		}
		l.Time = time.Unix(unixOf(at), 0)
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// ---------------------------------------------------------------------------
// Bans
// ---------------------------------------------------------------------------

// Permanent bans are stored in the bans table; only Unban on the web pages
// removes one, and one BattlEye loses is put back (syncBanList). Timed bans
// are kept in memory only (timedBans), filled from BattlEye's list after
// every login and every banSyncEvery, and gone when ReCON restarts until the
// next read.

// timedBans is every timed ban, by lower-case GUID.
type timedBans struct {
	sync.Mutex
	m map[string]timedBan
}

// timedBan is one timed ban and when it runs out.
type timedBan struct {
	name, reason string
	expires      time.Time
	index        int // its position in BattlEye's list, -1 until read
}

// RecordPlayerBan records a ban ReCON just placed: a permanent one in the bans table
// (with the player's SteamID when known), a timed one in memory, ending at
// expires.
func (db *Database) RecordPlayerBan(guid, playerName, ip, reason string, perm bool, expires time.Time) error {
	g := strings.ToLower(guid)
	db.timed.Lock()
	delete(db.timed.m, g)
	if !perm {
		db.timed.m[g] = timedBan{name: playerName, reason: reason, expires: expires, index: -1}
		db.timed.Unlock()
		_, err := db.Exec("DELETE FROM bans WHERE lower(guid) = ?", g) // now timed, not permanent
		return err
	}
	db.timed.Unlock()
	_, err := db.Exec(`
		INSERT INTO bans (guid, player_name, ip, reason, perm, steam_id)
		VALUES (?, ?, ?, ?, 1, (SELECT NULLIF(steam_id, '') FROM players WHERE lower(guid) = lower(?)))
		ON CONFLICT(guid) DO UPDATE SET
			player_name = excluded.player_name,
			ip = excluded.ip,
			reason = excluded.reason,
			steam_id = COALESCE(NULLIF(bans.steam_id, ''), excluded.steam_id)`,
		g, playerName, ip, reason, guid)
	return err
}

// storedBan is a permanent ban that SyncBansFromServer wants put back on the server.
type storedBan struct {
	GUID   string
	Reason string
}

// SyncBansFromServer brings the bans in line with BattlEye's GUID ban list, read at
// readAt:
//   - a permanent ban BattlEye lists is added to the bans table if missing
//     (name, IP and SteamID looked up from players), or has its reason and
//     list position refreshed.
//   - the timed bans in memory become the timed ones BattlEye lists, each
//     ending readAt plus its minutes left. A ban already known keeps its end
//     unless that moves by more than a minute (the minutes are rounded).
//   - a permanent ban BattlEye no longer lists is kept and returned: only
//     Unban on the web pages removes a permanent ban, so the caller puts it
//     back with addBan.
//
// Callers must pass a list parsed from a complete, recognised `bans` reply.
func (db *Database) SyncBansFromServer(bans []Ban, readAt time.Time) ([]storedBan, error) {
	live := make(map[string]Ban, len(bans))
	for _, b := range bans {
		g := strings.ToLower(b.GUID)
		if old, dup := live[g]; dup && !longerBan(b, old) {
			continue // BattlEye can list the same GUID more than once: keep the longest
		}
		live[g] = b
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	timed := map[string]timedBan{}
	for g, b := range live {
		if !b.Perm {
			m, _ := strconv.Atoi(b.Minutes)
			tb := timedBan{reason: b.Reason, expires: readAt.Add(time.Duration(m) * time.Minute), index: b.Index}
			if err := tx.QueryRow("SELECT username FROM players WHERE lower(guid) = ?", g).Scan(&tb.name); err != nil {
				tb.name = "Unknown"
			}
			timed[g] = tb
			continue
		}
		_, err := tx.Exec(`
			INSERT INTO bans (guid, player_name, ip, reason, perm, be_index, steam_id)
			VALUES (
				?,
				COALESCE((SELECT username FROM players WHERE lower(guid) = ?), 'Unknown'),
				COALESCE((SELECT last_ip FROM players WHERE lower(guid) = ?), ''),
				?, 1, ?,
				(SELECT NULLIF(steam_id, '') FROM players WHERE lower(guid) = ?)
			)
			ON CONFLICT(guid) DO UPDATE SET
				reason = excluded.reason,
				be_index = excluded.be_index,
				steam_id = COALESCE(NULLIF(bans.steam_id, ''), excluded.steam_id)`,
			g, g, g, b.Reason, b.Index, g,
		)
		if err != nil {
			return nil, err
		}
	}

	rows, err := tx.Query("SELECT lower(guid), reason FROM bans")
	if err != nil {
		return nil, err
	}
	var readd []storedBan
	for rows.Next() {
		var g, reason string
		if err := rows.Scan(&g, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if b, ok := live[g]; !ok || !b.Perm {
			readd = append(readd, storedBan{GUID: g, Reason: reason})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, b := range readd {
		if _, err := tx.Exec("UPDATE bans SET be_index = -1 WHERE lower(guid) = ?", b.GUID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	db.timed.Lock()
	for g, tb := range timed {
		if old, ok := db.timed.m[g]; ok && old.expires.Sub(tb.expires).Abs() <= time.Minute {
			tb.expires = old.expires
		}
		if old, ok := db.timed.m[g]; ok && tb.name == "Unknown" {
			tb.name = old.name
		}
		timed[g] = tb
	}
	db.timed.m = timed
	db.timed.Unlock()
	return readd, nil
}

// ExpireTimedBans forgets the timed bans whose time ran out by `now`, and
// returns how many.
func (db *Database) ExpireTimedBans(now time.Time) (int64, error) {
	db.timed.Lock()
	defer db.timed.Unlock()
	var n int64
	for g, tb := range db.timed.m {
		if !tb.expires.After(now) {
			delete(db.timed.m, g)
			n++
		}
	}
	return n, nil
}

// NextBanExpiry is when the next timed ban runs out (Unix seconds), or 0 when
// there is none.
func (db *Database) NextBanExpiry() (int64, error) {
	db.timed.Lock()
	defer db.timed.Unlock()
	var next int64
	for _, tb := range db.timed.m {
		if u := tb.expires.Unix(); next == 0 || u < next {
			next = u
		}
	}
	return next, nil
}

// IsPlayerBanned reports whether a GUID (case-insensitive) is banned.
func (db *Database) IsPlayerBanned(guid string) (bool, error) {
	st, err := db.GetBanState(guid)
	return st.Banned, err
}

// DeletePlayerBan removes a ban by GUID (case-insensitive), permanent or timed.
func (db *Database) DeletePlayerBan(guid string) error {
	db.timed.Lock()
	delete(db.timed.m, strings.ToLower(guid))
	db.timed.Unlock()
	_, err := db.Exec("DELETE FROM bans WHERE lower(guid) = lower(?)", guid)
	return err
}

// listPermanentBans reads the permanent bans, in BattlEye's list order (ones not in
// BattlEye's list go last), with an optional WHERE condition.
func (db *Database) listPermanentBans(where string, args ...any) ([]Ban, error) {
	rows, err := db.Query("SELECT lower(guid), player_name, reason, be_index FROM bans "+where+
		" ORDER BY be_index < 0, be_index, player_name COLLATE NOCASE", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ban{}
	for rows.Next() {
		b := Ban{Perm: true, Minutes: "perm"}
		if err := rows.Scan(&b.GUID, &b.Name, &b.Reason, &b.Index); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// timedBanList is the timed bans as the pages show them, at `now`.
func (db *Database) timedBanList(now time.Time) []Ban {
	db.timed.Lock()
	defer db.timed.Unlock()
	out := make([]Ban, 0, len(db.timed.m))
	for g, tb := range db.timed.m {
		out = append(out, Ban{GUID: g, Name: tb.name, Reason: tb.reason, Index: tb.index, Minutes: strconv.Itoa(minutesLeft(tb, now))})
	}
	return out
}

// minutesLeft rounds up: 30 seconds left shows as 1 minute.
func minutesLeft(tb timedBan, now time.Time) int {
	return max(int((tb.expires.Unix()-now.Unix()+59)/60), 0)
}

// stateOf is a ban as banState.
func stateOf(b Ban) banState {
	st := banState{Banned: true, Perm: b.Perm, MinutesLeft: -1, Reason: b.Reason}
	if !b.Perm {
		st.MinutesLeft, _ = strconv.Atoi(b.Minutes)
	}
	return st
}

// ListAllBans returns every ban for the Bans page, in BattlEye's list order
// (bans not placed in BattlEye's list yet go last).
func (db *Database) ListAllBans() ([]Ban, error) {
	out, err := db.listPermanentBans("")
	if err != nil {
		return nil, err
	}
	out = append(out, db.timedBanList(time.Now())...)
	sort.SliceStable(out, func(a, b int) bool {
		ia, ib := out[a].Index, out[b].Index
		if (ia < 0) != (ib < 0) {
			return ib < 0
		}
		if ia != ib {
			return ia < ib
		}
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out, nil
}

// AllBanStates returns every banned GUID's status, by lower-case GUID.
func (db *Database) AllBanStates() (map[string]banState, error) {
	list, err := db.ListAllBans()
	if err != nil {
		return nil, err
	}
	out := make(map[string]banState, len(list))
	for _, b := range list {
		out[b.GUID] = stateOf(b)
	}
	return out, nil
}

// GetBanState is one GUID's ban status (Banned false when not banned).
func (db *Database) GetBanState(guid string) (banState, error) {
	g := strings.ToLower(guid)
	db.timed.Lock()
	tb, ok := db.timed.m[g]
	db.timed.Unlock()
	if ok {
		return banState{Banned: true, MinutesLeft: minutesLeft(tb, time.Now()), Reason: tb.reason}, nil
	}
	list, err := db.listPermanentBans("WHERE lower(guid) = ?", g)
	if err != nil || len(list) == 0 {
		return banState{MinutesLeft: -1}, err
	}
	return stateOf(list[0]), nil
}

// ---------------------------------------------------------------------------
// Flows storage (the Flow type and all flow logic live in flows.go)
// ---------------------------------------------------------------------------

// flowsTableSQL creates the flows table. The id is a plain INTEGER PRIMARY
// KEY, not AUTOINCREMENT: SaveStoredFlow gives a new flow the lowest free number
// (from 0), so deleting flows frees their numbers for the next ones.
//
// Columns: script holds the steps as JSON, cancel the When stopped steps,
// timer the delay before step 1, schedule the trigger ("manual", "on-join",
// "on-chat" or a cron expression; see triggerFromSchedule in flows.go), and
// counter_threshold the flow's {threshold}.
const flowsTableSQL = `CREATE TABLE %s (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	script TEXT NOT NULL,
	cancel TEXT NOT NULL DEFAULT '',
	timer TEXT NOT NULL DEFAULT '',
	schedule TEXT NOT NULL DEFAULT '',
	schedule_enabled INTEGER NOT NULL DEFAULT 0,
	position INTEGER NOT NULL DEFAULT 0,
	counter_threshold INTEGER NOT NULL DEFAULT 0
)`

// EnsureFlowsTable creates the flows table if it doesn't exist yet (a new
// database starts with no flows). A table from an earlier release, made with
// AUTOINCREMENT (which never hands a number out twice), is copied into a new
// one without it. Every flow keeps its number; only new flows reuse the
// numbers of deleted ones.
func (db *Database) EnsureFlowsTable() error {
	var existing string
	err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'flows'").Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.Exec(fmt.Sprintf(flowsTableSQL, "flows"))
		return err
	} else if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(existing), "AUTOINCREMENT") {
		if !strings.Contains(existing, "counter_threshold") { // added with flow counters
			_, err = db.Exec("ALTER TABLE flows ADD COLUMN counter_threshold INTEGER NOT NULL DEFAULT 0")
		}
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		fmt.Sprintf(flowsTableSQL, "flows_new"),
		`INSERT INTO flows_new (id, name, script, cancel, timer, schedule, schedule_enabled, position)
			SELECT id, name, script, cancel, timer, schedule, schedule_enabled, position FROM flows`,
		"DROP TABLE flows",
		"ALTER TABLE flows_new RENAME TO flows",
		"DELETE FROM sqlite_sequence WHERE name = 'flows'",
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("moving flows to the new table: %w", err)
		}
	}
	return tx.Commit()
}

// ListStoredFlows returns every flow in display order, read with flowFromRow.
func (db *Database) ListStoredFlows() ([]Flow, error) {
	rows, err := db.Query("SELECT id, name, script, cancel, timer, schedule, schedule_enabled, counter_threshold FROM flows ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flows := []Flow{}
	for rows.Next() {
		var f Flow
		var script, cancel string
		var en int
		if err := rows.Scan(&f.ID, &f.Name, &script, &cancel, &f.Delay, &f.Schedule, &en, &f.CounterThreshold); err != nil {
			return nil, err
		}
		f.ScheduleEnabled = en != 0
		flowFromRow(&f, script, cancel)
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

// GetStoredFlow returns one flow, or sql.ErrNoRows.
func (db *Database) GetStoredFlow(id int64) (Flow, error) {
	var f Flow
	var script, cancel string
	var en int
	err := db.QueryRow("SELECT id, name, script, cancel, timer, schedule, schedule_enabled, counter_threshold FROM flows WHERE id = ?", id).
		Scan(&f.ID, &f.Name, &script, &cancel, &f.Delay, &f.Schedule, &en, &f.CounterThreshold)
	if err != nil {
		return f, err
	}
	f.ScheduleEnabled = en != 0
	flowFromRow(&f, script, cancel)
	return f, nil
}

// SaveStoredFlow inserts a new flow (isNew; placed last, with the lowest free
// number from 0) or updates flow f.ID. f.Schedule must already be in stored
// form ("manual", "on-join", "on-chat" or cron); script and cancel are the
// steps as JSON. Updating a missing ID returns sql.ErrNoRows.
func (db *Database) SaveStoredFlow(f Flow, isNew bool, script, cancel string) (int64, error) {
	en := 0
	if f.ScheduleEnabled {
		en = 1
	}
	if isNew {
		// One statement, so two flows saved at once can't take the same number.
		res, err := db.Exec(`
			INSERT INTO flows (id, name, script, cancel, timer, schedule, schedule_enabled, position, counter_threshold)
			VALUES (
				(SELECT CASE
					WHEN NOT EXISTS (SELECT 1 FROM flows WHERE id = 0) THEN 0
					ELSE (SELECT MIN(f.id) + 1 FROM flows f WHERE NOT EXISTS (SELECT 1 FROM flows g WHERE g.id = f.id + 1))
				END),
				?, ?, ?, ?, ?, ?,
				(SELECT COALESCE(MAX(position), 0) + 1 FROM flows),
				?
			)`,
			f.Name, script, cancel, f.Delay, f.Schedule, en, f.CounterThreshold)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := db.Exec("UPDATE flows SET name = ?, script = ?, cancel = ?, timer = ?, schedule = ?, schedule_enabled = ?, counter_threshold = ? WHERE id = ?",
		f.Name, script, cancel, f.Delay, f.Schedule, en, f.CounterThreshold, f.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return f.ID, nil
}

// SetFlowScheduleEnabled turns a flow's trigger (schedule or join) on or off.
func (db *Database) SetFlowScheduleEnabled(id int64, on bool) error {
	en := 0
	if on {
		en = 1
	}
	_, err := db.Exec("UPDATE flows SET schedule_enabled = ? WHERE id = ?", en, id)
	return err
}

// ReorderStoredFlows stores the display order: ids[0] becomes position 1, and so on.
func (db *Database) ReorderStoredFlows(ids []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec("UPDATE flows SET position = ? WHERE id = ?", i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteStoredFlow removes a flow row.
func (db *Database) DeleteStoredFlow(id int64) error {
	_, err := db.Exec("DELETE FROM flows WHERE id = ?", id)
	return err
}

// ---------------------------------------------------------------------------
// Keeping the database in step with the game server
// ---------------------------------------------------------------------------

// banListLock serialises everything that changes or mirrors the ban list
// (ban, unban, and BattlEye->DB sync) so a sync can't act on a stale
// list and delete a ban that was just added.
var banListLock sync.Mutex

// banSyncEvery is how often the server watch (rcon.go) reads BattlEye's ban
// list. It also reads it after every RCON login, so after a server restart.
const banSyncEvery = 5 * time.Minute

// syncBanList reads BattlEye's ban list and brings the bans table in line
// with it (SyncBansFromServer), then puts back any permanent ban the server has lost
// (`addBan <guid> 0 <reason>`) and saves the list with writeBans. Holds
// banListLock so it can't interleave with a ban or unban.
func syncBanList() error {
	banListLock.Lock()
	defer banListLock.Unlock()
	readAt := time.Now()
	bans, err := readBanList()
	if err != nil {
		return err
	}
	readd, err := globalDatabase.SyncBansFromServer(bans, readAt)
	refreshBanExpiry()
	if err != nil {
		return err
	}
	added := 0
	for _, b := range readd {
		cmd := strings.TrimSpace(fmt.Sprintf("addBan %s 0 %s", b.GUID, cleanReason(b.Reason)))
		_, acked, err := runWebCommand(cmd, 1500*time.Millisecond, false, "bans")
		if err != nil || !acked {
			log.Printf("bans: re-adding permanent ban %s failed (acked=%v err=%v)", b.GUID, acked, err)
			continue
		}
		log.Printf("bans: re-added permanent ban %s (missing from BattlEye's list)", b.GUID)
		added++
	}
	if added > 0 {
		runWebCommand("writeBans", 1500*time.Millisecond, false, "bans") // persist bans.txt, best effort
	}
	return nil
}

// banState is one player's ban as the pages show it. MinutesLeft is -1 when
// it isn't known (a permanent ban, or a timed one not read from BattlEye yet).
type banState struct {
	Banned      bool   `json:"banned"`
	Perm        bool   `json:"perm"`
	MinutesLeft int    `json:"minutesLeft"`
	Reason      string `json:"reason"`
}

// longerBan reports whether a outlasts b.
func longerBan(a, b Ban) bool {
	if a.Perm != b.Perm {
		return a.Perm
	}
	am, _ := strconv.Atoi(a.Minutes)
	bm, _ := strconv.Atoi(b.Minutes)
	return am > bm
}

// allBanStatuses returns every banned GUID's status, from the bans table.
func allBanStatuses() map[string]banState {
	out, err := globalDatabase.AllBanStates()
	if err != nil {
		log.Printf("bans: reading the bans table: %v", err)
		return map[string]banState{}
	}
	return out
}

// banStatusOf is one player's ban status (Banned false when not banned).
func banStatusOf(guid string) banState {
	st, err := globalDatabase.GetBanState(guid)
	if err != nil {
		log.Printf("bans: reading %s: %v", guid, err)
		return banState{MinutesLeft: -1}
	}
	return st
}

// nextBanExpiry is when the next timed ban runs out (Unix seconds, 0 for
// none), so the server watch only deletes expired bans when one is due.
var nextBanExpiry atomic.Int64

// refreshBanExpiry re-reads nextBanExpiry after the bans table changed.
func refreshBanExpiry() {
	next, err := globalDatabase.NextBanExpiry()
	if err != nil {
		log.Printf("bans: reading the next expiry: %v", err)
		return
	}
	nextBanExpiry.Store(next)
}

// expireDueBans deletes timed bans whose time has run out, if one is due.
// BattlEye drops them by itself, and nothing ever adds one back.
func expireDueBans(now time.Time) {
	if next := nextBanExpiry.Load(); next == 0 || now.Unix() < next {
		return
	}
	banListLock.Lock()
	defer banListLock.Unlock()
	n, err := globalDatabase.ExpireTimedBans(now)
	if err != nil {
		log.Printf("bans: deleting expired bans: %v", err)
		return
	}
	if n > 0 {
		log.Printf("bans: %d timed ban(s) ran out", n)
	}
	refreshBanExpiry()
}

// guidForChat finds who wrote a chat line: an online player with that name,
// else the last player seen with it (someone who chatted and left before the
// next poll). "" when nobody matches.
func guidForChat(name string) string {
	if p, ok := onlineByName(name); ok && p.GUID != "" {
		return p.GUID
	}
	guid, err := globalDatabase.GUIDByName(name)
	if err != nil {
		log.Printf("chat: looking up %q: %v", name, err)
	}
	return guid
}

// startChatRecorder files every incoming chat line (serverEvents, rcon.go)
// under its writer's GUID for as long as ReCON runs. A line from a name no
// player has had is logged and left out of the history (it still shows in
// the console).
func startChatRecorder() {
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind != eventChat {
				continue
			}
			guid := guidForChat(ev.Name)
			if guid == "" {
				log.Printf("chat: no player named %q, line not kept in a history: %s", ev.Name, ev.Text)
				continue
			}
			if err := globalDatabase.AddChatMessage(ev.Time, guid, chatIn, ev.Text); err != nil {
				log.Printf("chat: store: %v", err)
			}
		}
	}()
}

// startDatabaseSync keeps the database in step with the game server without
// any web page open: it starts the chat recorder and the player tracker
// (players.go), which both read the master console log, and loads when the
// next timed ban runs out. The server watch (rcon.go) does the polling.
func startDatabaseSync() {
	startChatRecorder()
	startPlayerTracker()
	refreshBanExpiry()
}
