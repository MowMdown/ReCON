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
//   - players / sessions / IP history (SyncPlayers, OfflinePlayers, ...)
//   - player details and admin notes (PlayerDetails, SaveNote)
//   - chat history (AddChatMessage, ChatHistory)
//   - SteamIDs filled in by guidsearch (SteamIDByGUID, PlayersBySteamIDs)
//   - player roles for the chat commands (PlayerRole, SetPlayerRole, ...)
//   - settings, such as the chat commands' (Setting, SaveSetting)
//   - bans (BanUser, SyncBans, BanNames, DeleteBan)
//   - flows storage (ListFlows, GetFlow, SaveFlow, ...): steps are kept as JSON
//
// The bottom section keeps the database in step with the game server: the
// background poller, the BattlEye -> DB ban mirror (and the in-memory ban
// status it leaves behind), and the chat recorder. Those are the only parts
// here that call into rcon.go.
// ============================================================================

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
		total_playtime_seconds INTEGER NOT NULL DEFAULT 0,
		-- Role for the chat commands: 0 Player (everyone starts here), 1 Mod, 2 Admin.
		role INTEGER NOT NULL DEFAULT 0,
		-- SteamID64 as decimal text. ReCON never writes it: it stays NULL
		-- until guidsearch fills it in from the GUID.
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
		-- As players.steam_id: NULL until guidsearch fills it in.
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
	-- Same names guidsearch uses, so neither program makes a second index.
	CREATE INDEX IF NOT EXISTS idx_players_steam_id
		ON players(steam_id);
	CREATE INDEX IF NOT EXISTS idx_bans_steam_id
		ON bans(steam_id);

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
	return &Database{db}, nil
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

// SteamIDByGUID returns a player's SteamID64 as guidsearch stored it in
// players.steam_id ("" until it has). known is false if the GUID was never
// seen. ReCON reads this column but never writes it.
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
	rows, err := db.Query(`SELECT connected_at, disconnected_at, duration_seconds, ip
		FROM session_history WHERE guid = ? ORDER BY id DESC LIMIT ?`, g, recentSessionsKept)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c, x any
		var s detailsSession
		if err := rows.Scan(&c, &x, &s.Seconds, &s.IP); err != nil {
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

// storedBans reads the bans table as ban states (permanent vs timed is
// known, the minutes left are not). Used before the first BattlEye read.
func (db *Database) storedBans() (map[string]banState, error) {
	rows, err := db.Query("SELECT guid, reason, perm FROM bans")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]banState{}
	for rows.Next() {
		var g, reason string
		var perm int
		if err := rows.Scan(&g, &reason, &perm); err != nil {
			return nil, err
		}
		out[strings.ToLower(g)] = banState{Banned: true, Perm: perm == 1, MinutesLeft: -1, Reason: reason}
	}
	return out, rows.Err()
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

// syncBanList reads the BattlEye list and mirrors it into the DB, and keeps
// it in memory for the ban status the pages show (rememberBans).
// Holds banListLock for the whole read+mirror so it can't interleave with a
// ban/unban. Return values matter to callers:
//   - (nil, err)   -> couldn't read the live list at all
//   - (bans, err)  -> list was read fine, but the DB mirror failed
//   - (bans, nil)  -> fully synced
func syncBanList() ([]Ban, error) {
	banListLock.Lock()
	defer banListLock.Unlock()
	bans, err := readBanList()
	if err != nil {
		return nil, err
	}
	rememberBans(bans)
	if err := globalDatabase.SyncBans(bans); err != nil {
		return bans, err
	}
	return bans, nil
}

// banState is one player's ban as the pages show it. MinutesLeft is -1 when
// it isn't known (a permanent ban, or only the database copy is available).
type banState struct {
	Banned      bool   `json:"banned"`
	Perm        bool   `json:"perm"`
	MinutesLeft int    `json:"minutesLeft"`
	Reason      string `json:"reason"`
}

// banCache is the BattlEye ban list as last read by syncBanList, by
// lower-case GUID, so the pages can show "Banned - 2d 4h left" without
// asking the server each time.
var banCache = struct {
	sync.Mutex
	ok     bool
	readAt time.Time
	byGUID map[string]Ban
}{}

// rememberBans keeps a freshly read ban list. A GUID listed more than once
// keeps its longest ban.
func rememberBans(bans []Ban) {
	m := make(map[string]Ban, len(bans))
	for _, b := range bans {
		g := strings.ToLower(b.GUID)
		if old, dup := m[g]; dup && !longerBan(b, old) {
			continue
		}
		m[g] = b
	}
	banCache.Lock()
	banCache.ok, banCache.readAt, banCache.byGUID = true, time.Now(), m
	banCache.Unlock()
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

// stateOf turns a ban list entry into a banState, counting down the minutes
// since the list was read.
func stateOf(b Ban, readAt time.Time) banState {
	st := banState{Banned: true, Perm: b.Perm, MinutesLeft: -1, Reason: b.Reason}
	if !b.Perm {
		if m, err := strconv.Atoi(b.Minutes); err == nil {
			left := m - int(time.Since(readAt)/time.Minute)
			if left < 0 {
				left = 0 // expired since the last read; gone at the next sync
			}
			st.MinutesLeft = left
		}
	}
	return st
}

// banStatuses returns every banned GUID's status. Before the first ban list
// read it falls back to the bans table.
func banStatuses() map[string]banState {
	banCache.Lock()
	if banCache.ok {
		out := make(map[string]banState, len(banCache.byGUID))
		for g, b := range banCache.byGUID {
			out[g] = stateOf(b, banCache.readAt)
		}
		banCache.Unlock()
		return out
	}
	banCache.Unlock()
	out, err := globalDatabase.storedBans()
	if err != nil {
		log.Printf("bans: reading the bans table: %v", err)
		return map[string]banState{}
	}
	return out
}

// banStatusOf is one player's ban status (Banned false when not banned).
func banStatusOf(guid string) banState {
	if st, ok := banStatuses()[strings.ToLower(guid)]; ok {
		return st
	}
	return banState{MinutesLeft: -1}
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

// startBackgroundSync tracks players (and periodically the ban list) in the
// background so the DB stays current without any web page open, and starts
// the chat recorder. It is the only caller of SyncPlayers.
//
// Every `interval`:
//   - Poll `players` (single attempt, 3s wait).
//   - On failure, count consecutive failures. At exactly 3 in a row the server
//     is assumed down and SyncPlayers(nil) closes all open sessions once, so
//     playtime stops accruing while it's offline. From the 3rd miss on, each
//     round logs in again on the same RCON socket (reloginShared, rcon.go).
//   - On success, reset the failure counter, sync the parsed list into the DB
//     and pass it to playersPolled, which starts player-join flows.
//   - On the 1st poll and every 6th after (~30s at a 5s interval), also
//     reconcile the ban list, since it changes rarely.
func startBackgroundSync(interval time.Duration) {
	startChatRecorder()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		fails := 0
		n := 0
		for range t.C {
			n++
			out, acked, err := runShared("players", 3*time.Second, false, "")
			rconPolled.Store(true)
			if err != nil || !acked {
				fails++
				rconFails.Store(int32(fails))
				// Server probably down: close open sessions once so
				// playtime doesn't keep accruing while it's offline.
				if fails == 3 {
					if err := globalDatabase.SyncPlayers(nil); err != nil {
						log.Printf("poller: close sessions: %v", err)
					}
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
			players := parsePlayers(out)
			if err := globalDatabase.SyncPlayers(players); err != nil {
				log.Printf("poller: sync: %v", err)
			}
			playersPolled(players) // keeps the list for flows and chat commands, starts join flows (flows.go)
			// Ban list changes rarely: reconcile at start and every 6th poll (~30s).
			if n%6 == 1 {
				if _, err := syncBanList(); err != nil {
					log.Printf("poller: bans: %v", err)
				}
			}
		}
	}()
}
