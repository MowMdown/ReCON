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
//   - schema setup (OpenDatabase) and the flows table (EnsureFlowsTable)
//   - players / sessions / IP history (OpenSession, CloseSession, OfflinePlayers, ...)
//   - player details and admin notes (PlayerDetails, SaveNote)
//   - chat history (AddChatMessage, ChatHistory)
//   - SteamIDs (SteamIDByGUID, PlayersBySteamIDs, PendingSteamGUIDs, SetSteamID)
//   - player roles for the chat commands (PlayerRole, SetPlayerRole, ...)
//   - settings, such as the chat commands' (Setting, SaveSetting)
//   - bans (BanUser, SyncBans, ExpireBans, BanList, BanStates, DeleteBan)
//   - flows storage (ListFlows, GetFlow, SaveFlow, ...): steps are kept as JSON
//
// The bottom section keeps the database in step with the game server: the
// background poller, the BattlEye <-> DB ban sync, and the chat recorder.
// Those are the only parts here that call into rcon.go. Player sessions are
// driven by the server's messages, in players.go.
// ============================================================================

type Database struct {
	*sql.DB
}

// sqliteParams are applied by the driver to every connection it opens.
// database/sql keeps a pool of connections, and busy_timeout and foreign_keys
// only last for the connection they were set on, so setting them with one
// Exec would leave the others without them. _txlock=immediate takes the write
// lock when a transaction begins, so a transaction that reads and then writes
// waits its turn instead of failing with "database is locked".
const sqliteParams = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"

func OpenDatabase(filepath string) (*Database, error) {
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
		// Unix seconds when a timed ban runs out (NULL for permanent bans, or
		// until the first ban list read), and its position in BattlEye's list.
		{"bans", "expires_unix", "INTEGER"},
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
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to remove unverified-player rows: %w", err)
		}
	}
	return &Database{db}, nil
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

// OpenSession records a player coming online (players.go calls it once per
// session, when their GUID is known): their name and IP, the IP in their IP
// history, and an open session starting at connectedAt. A session already
// open for the GUID is left as it is.
func (db *Database) OpenSession(guid, name, ip string, playerID int, connectedAt time.Time) error {
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

// MarkLoaded records when a player finished loading ("(Lobby)" left their name).
func (db *Database) MarkLoaded(guid string, at time.Time) error {
	_, err := db.Exec("UPDATE active_sessions SET loaded_at = ? WHERE guid = ? AND loaded_at IS NULL", at.UTC(), guid)
	return err
}

// CloseSession ends a player's open session at `at`: it goes into
// session_history with how it ended, its length is added to their playtime,
// and last_seen becomes `at`. Nothing happens if no session is open.
func (db *Database) CloseSession(guid string, at time.Time, ended string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ip string
	var connectedAt time.Time
	var loadedAt sql.NullTime
	err = tx.QueryRow("SELECT ip, connected_at, loaded_at FROM active_sessions WHERE guid = ?", guid).Scan(&ip, &connectedAt, &loadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	at = at.UTC()
	duration := int(at.Sub(connectedAt).Seconds())
	if duration < 0 {
		duration = 0
	}
	var loaded any // NULL when they never finished loading
	if loadedAt.Valid {
		loaded = loadedAt.Time.UTC()
	}
	if _, err := tx.Exec(`
		INSERT INTO session_history (guid, ip, connected_at, disconnected_at, duration_seconds, loaded_at, ended)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		guid, ip, connectedAt.UTC(), at, duration, loaded, ended); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE players
		SET last_seen = ?, total_playtime_seconds = total_playtime_seconds + ?
		WHERE guid = ?`,
		at, duration, guid); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM active_sessions WHERE guid = ?", guid); err != nil {
		return err
	}
	return tx.Commit()
}

// OfflinePlayers returns all players from the database who are not currently connected.
func (db *Database) OfflinePlayers() ([]Player, error) {
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

// KnownPlayers returns every player ever seen, sorted by name. Offline is
// false for players with an open session (online as of the last poll).
func (db *Database) KnownPlayers() ([]Player, error) {
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

// PendingSteamGUIDs returns every GUID (lower case) in players and bans that
// has no SteamID yet: what a SteamID search looks for. Malformed GUIDs are
// left out.
func (db *Database) PendingSteamGUIDs() ([]string, error) {
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

// PlayerRole returns a player's role (0 Player, 1 Mod, 2 Admin) by GUID,
// case-insensitive. Unknown players, and errors, are Player.
func (db *Database) PlayerRole(guid string) int {
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

// Roles returns lower-case GUID -> role for every Mod and Admin.
func (db *Database) Roles() (map[string]int, error) {
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

// Setting returns a stored setting; ok is false when it was never saved.
func (db *Database) Setting(key string) (value string, ok bool, err error) {
	err = db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SaveSetting stores a setting.
func (db *Database) SaveSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// Player details and admin notes (the Players page's Details dialog)
// ---------------------------------------------------------------------------

// recentSessionsKept is how many past sessions PlayerDetails returns, newest first.
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

// PlayerDetails gathers everything stored about one player (GUID matched
// case-insensitively), or sql.ErrNoRows if the GUID was never seen.
func (db *Database) PlayerDetails(guid string) (*playerDetails, error) {
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
	if err := db.QueryRow("SELECT connected_at, ip FROM active_sessions WHERE guid = ?", g).Scan(&since, &ip); err == nil {
		c := unixOf(since)
		s := detailsSession{Connected: c, IP: asString(ip), Current: true}
		if c > 0 {
			s.Seconds = time.Now().Unix() - c
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

// PlayerKnown reports whether a GUID (case-insensitive) has ever been seen.
func (db *Database) PlayerKnown(guid string) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM players WHERE lower(guid) = lower(?)", guid).Scan(&n)
	return n > 0, err
}

// SaveNote stores a player's admin note; an empty note deletes it.
func (db *Database) SaveNote(guid, note string) error {
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

// lastChat is the newest line a player wrote in chat.
type lastChat struct {
	Text    string    // what they typed, without the "(Global) Name: " prefix
	Channel string    // Global, Side, Direct, Vehicle, ...
	At      time.Time // when it was sent
}

// LastChats returns, by lower-case GUID, the newest line each given player
// wrote in chat during their current session (the open session in
// active_sessions). Messages ReCON sent to them don't count, and neither does
// anything from an earlier session: a player who hasn't chatted since they
// connected is left out. The Players page asks only for the players online.
func (db *Database) LastChats(guids []string) (map[string]lastChat, error) {
	out := map[string]lastChat{}
	if len(guids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(guids)+1)
	args = append(args, chatIn)
	for _, g := range guids {
		args = append(args, strings.ToLower(g))
	}
	rows, err := db.Query(`
		SELECT c.guid, c.at, c.line, a.connected_at
		FROM chat_messages c
		JOIN active_sessions a ON a.guid = c.guid
		WHERE c.id IN (
			SELECT MAX(id) FROM chat_messages
			WHERE direction = ? AND guid IN (?`+strings.Repeat(",?", len(guids)-1)+`)
			GROUP BY guid
		)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g, line string
		var at, since any
		if err := rows.Scan(&g, &at, &line, &since); err != nil {
			return nil, err
		}
		// Times are compared here, not in SQL: they're stored as text that
		// doesn't sort reliably.
		sent := unixOf(at)
		if sent < unixOf(since) {
			continue // their newest line is from an earlier session
		}
		c := lastChat{Text: line, At: time.Unix(sent, 0)}
		if ev := parseServerMessage(line, c.At); ev.Kind == eventChat {
			c.Text, c.Channel = ev.Message, ev.Channel
		}
		out[g] = c
	}
	return out, rows.Err()
}

// ChatHistory returns a player's newest `limit` lines, oldest first.
func (db *Database) ChatHistory(guid string, limit int) ([]chatLine, error) {
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

// BanUser records a ban ReCON just placed. expires is when a timed ban runs
// out (ignored for a permanent one). steam_id is copied from players when the
// player's SteamID is known.
func (db *Database) BanUser(guid, playerName, ip, reason string, perm bool, expires time.Time) error {
	var exp any // NULL for a permanent ban
	if !perm {
		exp = expires.Unix()
	}
	_, err := db.Exec(`
		INSERT INTO bans (guid, player_name, ip, reason, perm, expires_unix, steam_id)
		VALUES (?, ?, ?, ?, ?, ?, (SELECT NULLIF(steam_id, '') FROM players WHERE lower(guid) = lower(?)))
		ON CONFLICT(guid) DO UPDATE SET
			player_name = excluded.player_name,
			ip = excluded.ip,
			reason = excluded.reason,
			perm = excluded.perm,
			expires_unix = excluded.expires_unix,
			steam_id = COALESCE(NULLIF(bans.steam_id, ''), excluded.steam_id)`,
		strings.ToLower(guid), playerName, ip, reason, boolInt(perm), exp, guid)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// storedBan is a bans row that SyncBans wants put back on the server.
type storedBan struct {
	GUID   string
	Reason string
}

// SyncBans brings the bans table in line with BattlEye's GUID ban list, read
// at readAt:
//   - a ban BattlEye lists is added if missing (name, IP and SteamID looked up
//     from players), or has its reason, type and list position refreshed. A
//     timed ban's expiry is readAt plus the minutes left, re-set only when it
//     moves by more than a minute (the minutes are rounded).
//   - a timed ban BattlEye no longer lists (it ran out, or was removed some
//     other way) is deleted.
//   - a permanent ban BattlEye no longer lists is kept and returned: only
//     Unban on the web pages removes a permanent ban, so the caller puts it
//     back with addBan.
//
// Callers must pass a list parsed from a complete, recognised `bans` reply.
func (db *Database) SyncBans(bans []Ban, readAt time.Time) ([]storedBan, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	live := make(map[string]Ban, len(bans))
	for _, b := range bans {
		g := strings.ToLower(b.GUID)
		if old, dup := live[g]; dup && !longerBan(b, old) {
			continue // BattlEye can list the same GUID more than once: keep the longest
		}
		live[g] = b
	}
	for g, b := range live {
		var exp any // NULL for a permanent ban
		if !b.Perm {
			m, _ := strconv.Atoi(b.Minutes)
			exp = readAt.Add(time.Duration(m) * time.Minute).Unix()
		}
		_, err := tx.Exec(`
			INSERT INTO bans (guid, player_name, ip, reason, perm, expires_unix, be_index, steam_id)
			VALUES (
				?,
				COALESCE((SELECT username FROM players WHERE lower(guid) = ?), 'Unknown'),
				COALESCE((SELECT last_ip FROM players WHERE lower(guid) = ?), ''),
				?, ?, ?, ?,
				(SELECT NULLIF(steam_id, '') FROM players WHERE lower(guid) = ?)
			)
			ON CONFLICT(guid) DO UPDATE SET
				reason = excluded.reason,
				perm = excluded.perm,
				be_index = excluded.be_index,
				expires_unix = CASE
					WHEN excluded.perm = 1 THEN NULL
					WHEN bans.expires_unix IS NULL OR abs(bans.expires_unix - excluded.expires_unix) > 60 THEN excluded.expires_unix
					ELSE bans.expires_unix
				END,
				steam_id = COALESCE(NULLIF(bans.steam_id, ''), excluded.steam_id)`,
			g, g, g, b.Reason, boolInt(b.Perm), exp, b.Index, g,
		)
		if err != nil {
			return nil, err
		}
	}

	rows, err := tx.Query("SELECT guid, perm, reason FROM bans")
	if err != nil {
		return nil, err
	}
	var gone []string
	var readd []storedBan
	for rows.Next() {
		var g, reason string
		var perm int
		if err := rows.Scan(&g, &perm, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := live[strings.ToLower(g)]; ok {
			continue
		}
		if perm == 1 {
			readd = append(readd, storedBan{GUID: strings.ToLower(g), Reason: reason})
		} else {
			gone = append(gone, g)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, g := range gone {
		if _, err := tx.Exec("DELETE FROM bans WHERE guid = ?", g); err != nil {
			return nil, err
		}
	}
	for _, b := range readd {
		if _, err := tx.Exec("UPDATE bans SET be_index = -1 WHERE guid = ?", b.GUID); err != nil {
			return nil, err
		}
	}
	return readd, tx.Commit()
}

// ExpireBans deletes the timed bans whose time ran out by `now`.
func (db *Database) ExpireBans(now time.Time) (int64, error) {
	res, err := db.Exec("DELETE FROM bans WHERE perm = 0 AND expires_unix IS NOT NULL AND expires_unix <= ?", now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NextBanExpiry is when the next timed ban runs out (Unix seconds), or 0 when
// no timed ban has a known end.
func (db *Database) NextBanExpiry() (int64, error) {
	var next sql.NullInt64
	err := db.QueryRow("SELECT MIN(expires_unix) FROM bans WHERE perm = 0 AND expires_unix IS NOT NULL").Scan(&next)
	return next.Int64, err
}

// IsBanned reports whether a GUID (case-insensitive) is in the bans table.
func (db *Database) IsBanned(guid string) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM bans WHERE lower(guid) = lower(?)", guid).Scan(&n)
	return n > 0, err
}

// DeleteBan removes a ban row by GUID (case-insensitive).
func (db *Database) DeleteBan(guid string) error {
	_, err := db.Exec("DELETE FROM bans WHERE lower(guid) = lower(?)", guid)
	return err
}

// banColumns and scanBan read one bans row for the pages.
const banColumns = "lower(guid), player_name, reason, perm, expires_unix, be_index"

func scanBan(sc interface{ Scan(...any) error }, now time.Time) (Ban, banState, error) {
	var b Ban
	var perm int
	var exp sql.NullInt64
	if err := sc.Scan(&b.GUID, &b.Name, &b.Reason, &perm, &exp, &b.Index); err != nil {
		return b, banState{}, err
	}
	b.Perm = perm == 1
	st := banState{Banned: true, Perm: b.Perm, MinutesLeft: -1, Reason: b.Reason}
	switch {
	case b.Perm:
		b.Minutes = "perm"
	case exp.Valid:
		left := int((exp.Int64 - now.Unix() + 59) / 60) // round up: 30 seconds left shows as 1 minute
		st.MinutesLeft = max(left, 0)
		b.Minutes = strconv.Itoa(st.MinutesLeft)
	default:
		b.Minutes = "?" // timed, but not read from BattlEye yet
	}
	return b, st, nil
}

// BanList returns every ban for the Bans page, in BattlEye's list order
// (bans not placed in BattlEye's list yet go last).
func (db *Database) BanList() ([]Ban, error) {
	rows, err := db.Query("SELECT " + banColumns + " FROM bans ORDER BY be_index < 0, be_index, player_name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := []Ban{}
	for rows.Next() {
		b, _, err := scanBan(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BanStates returns every banned GUID's status, by lower-case GUID.
func (db *Database) BanStates() (map[string]banState, error) {
	rows, err := db.Query("SELECT " + banColumns + " FROM bans")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := map[string]banState{}
	for rows.Next() {
		b, st, err := scanBan(rows, now)
		if err != nil {
			return nil, err
		}
		out[b.GUID] = st
	}
	return out, rows.Err()
}

// BanState is one GUID's ban status (Banned false when not banned).
func (db *Database) BanState(guid string) (banState, error) {
	_, st, err := scanBan(db.QueryRow("SELECT "+banColumns+" FROM bans WHERE lower(guid) = lower(?)", guid), time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return banState{MinutesLeft: -1}, nil
	}
	return st, err
}

// ---------------------------------------------------------------------------
// Flows storage (the Flow type and all flow logic live in flows.go)
// ---------------------------------------------------------------------------

// EnsureFlowsTable creates the flows table if it doesn't exist yet. A new
// database starts with no flows.
//
// Columns: script holds the steps as JSON, cancel the When stopped steps,
// timer the delay before step 1, and schedule the trigger: "manual",
// "on-join", "on-chat" or a cron expression (see triggerOf in flows.go).
func (db *Database) EnsureFlowsTable() error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS flows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			script TEXT NOT NULL,
			cancel TEXT NOT NULL DEFAULT '',
			timer TEXT NOT NULL DEFAULT '',
			schedule TEXT NOT NULL DEFAULT '',
			schedule_enabled INTEGER NOT NULL DEFAULT 0,
			position INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		return err
	}
	return nil
}

// ListFlows returns every flow in display order, read with flowFromRow.
func (db *Database) ListFlows() ([]Flow, error) {
	rows, err := db.Query("SELECT id, name, script, cancel, timer, schedule, schedule_enabled FROM flows ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flows := []Flow{}
	for rows.Next() {
		var f Flow
		var script, cancel string
		var en int
		if err := rows.Scan(&f.ID, &f.Name, &script, &cancel, &f.Delay, &f.Schedule, &en); err != nil {
			return nil, err
		}
		f.ScheduleEnabled = en != 0
		flowFromRow(&f, script, cancel)
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

// GetFlow returns one flow, or sql.ErrNoRows.
func (db *Database) GetFlow(id int64) (Flow, error) {
	var f Flow
	var script, cancel string
	var en int
	err := db.QueryRow("SELECT id, name, script, cancel, timer, schedule, schedule_enabled FROM flows WHERE id = ?", id).
		Scan(&f.ID, &f.Name, &script, &cancel, &f.Delay, &f.Schedule, &en)
	if err != nil {
		return f, err
	}
	f.ScheduleEnabled = en != 0
	flowFromRow(&f, script, cancel)
	return f, nil
}

// SaveFlow inserts a new flow (ID 0, placed last) or updates an existing one.
// f.Schedule must already be in stored form ("manual", "on-join", "on-chat"
// or cron); script and cancel are the steps as JSON. Updating a missing ID
// returns sql.ErrNoRows.
func (db *Database) SaveFlow(f Flow, script, cancel string) (int64, error) {
	en := 0
	if f.ScheduleEnabled {
		en = 1
	}
	if f.ID == 0 {
		res, err := db.Exec("INSERT INTO flows (name, script, cancel, timer, schedule, schedule_enabled, position) VALUES (?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM flows))",
			f.Name, script, cancel, f.Delay, f.Schedule, en)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := db.Exec("UPDATE flows SET name = ?, script = ?, cancel = ?, timer = ?, schedule = ?, schedule_enabled = ? WHERE id = ?",
		f.Name, script, cancel, f.Delay, f.Schedule, en, f.ID)
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

// ReorderFlows stores the display order: ids[0] becomes position 1, and so on.
func (db *Database) ReorderFlows(ids []int64) error {
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

// DeleteFlow removes a flow row.
func (db *Database) DeleteFlow(id int64) error {
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

// banSyncEvery is how often the poller reads BattlEye's ban list. It also
// reads it after every RCON login (bansStale), so after a server restart.
const banSyncEvery = 5 * time.Minute

// bansStale is set after each RCON login (connectShared, reloginShared in
// rcon.go); the poller then reads the ban list at its next good poll.
var bansStale atomic.Bool

// syncBanList reads BattlEye's ban list and brings the bans table in line
// with it (SyncBans), then puts back any permanent ban the server has lost
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
	readd, err := globalDatabase.SyncBans(bans, readAt)
	refreshBanExpiry()
	if err != nil {
		return err
	}
	added := 0
	for _, b := range readd {
		cmd := strings.TrimSpace(fmt.Sprintf("addBan %s 0 %s", b.GUID, cleanReason(b.Reason)))
		_, acked, err := runShared(cmd, 1500*time.Millisecond, false, "bans")
		if err != nil || !acked {
			log.Printf("bans: re-adding permanent ban %s failed (acked=%v err=%v)", b.GUID, acked, err)
			continue
		}
		log.Printf("bans: re-added permanent ban %s (missing from BattlEye's list)", b.GUID)
		added++
	}
	if added > 0 {
		runShared("writeBans", 1500*time.Millisecond, false, "bans") // persist bans.txt, best effort
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

// banStatuses returns every banned GUID's status, from the bans table.
func banStatuses() map[string]banState {
	out, err := globalDatabase.BanStates()
	if err != nil {
		log.Printf("bans: reading the bans table: %v", err)
		return map[string]banState{}
	}
	return out
}

// banStatusOf is one player's ban status (Banned false when not banned).
func banStatusOf(guid string) banState {
	st, err := globalDatabase.BanState(guid)
	if err != nil {
		log.Printf("bans: reading %s: %v", guid, err)
		return banState{MinutesLeft: -1}
	}
	return st
}

// nextBanExpiry is when the next timed ban runs out (Unix seconds, 0 for
// none), so the poller only deletes expired bans when one is due.
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
	n, err := globalDatabase.ExpireBans(now)
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

// rconPolled and rconFails let the status bar show what the poller saw:
// whether it has checked the game server yet, and how many checks in a row
// have failed (see rconStatus in web.go).
var (
	rconPolled atomic.Bool
	rconFails  atomic.Int32
)

// startBackgroundSync keeps the database in step with the game server
// without any web page open: it starts the chat recorder and the player
// tracker (players.go), then polls in the background.
//
// Every `interval`:
//   - Delete timed bans that have run out, if one is due.
//   - Poll `players` (single attempt, 3s wait).
//   - On failure, count consecutive failures. At exactly 3 in a row the server
//     is assumed down: every open session is closed once (serverLost), so
//     playtime stops accruing while it's offline. From the 3rd miss on, each
//     round logs in again on the same RCON socket (reloginShared, rcon.go).
//   - On success, reset the failure counter and hand the list to the tracker
//     (playersPolledTracker), the SteamID search (auto mode) and the flows
//     (playersPolled, which starts player-join flows).
//   - Read the ban list after an RCON login and every banSyncEvery.
func startBackgroundSync(interval time.Duration) {
	startChatRecorder()
	startPlayerTracker()
	refreshBanExpiry()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		fails := 0
		var lastBanSync time.Time
		for range t.C {
			expireDueBans(time.Now())
			sentAt := time.Now()
			out, acked, err := runShared("players", 3*time.Second, false, "")
			rconPolled.Store(true)
			if err != nil || !acked {
				fails++
				rconFails.Store(int32(fails))
				// Server probably down: close open sessions once so
				// playtime doesn't keep accruing while it's offline.
				if fails == 3 {
					serverLost()  // players.go
					playersLost() // greet everyone again when they reconnect (flows.go)
				}
				// From the 3rd miss on, log in again on the same socket each
				// round: the server may have restarted and forgotten us. This
				// is the only place that decides the server isn't answering.
				if fails >= 3 {
					reloginShared()
				}
				continue
			}
			fails = 0
			rconFails.Store(0)
			resetReloginPace() // rcon.go
			players := parsePlayers(out)
			playersPolledTracker(players, sentAt) // sessions (players.go)
			steamPlayersSeen(players)             // STEAM_SEARCH_MODE=auto: find new players' SteamIDs (steam.go)
			playersPolled(players)                // keeps the list for flows and chat commands, starts join flows (flows.go)
			if bansStale.Swap(false) || time.Since(lastBanSync) >= banSyncEvery {
				lastBanSync = time.Now()
				if err := syncBanList(); err != nil {
					log.Printf("poller: bans: %v", err)
					lastBanSync = time.Now().Add(time.Minute - banSyncEvery) // try again in a minute
				}
			}
		}
	}()
}
