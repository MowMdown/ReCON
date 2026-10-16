package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
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
//   - bans (BanUser, SyncBans, BanNames, DeleteBan)
//   - flows storage (ListFlows, GetFlow, SaveFlow, ...)
//
// The bottom section keeps the database in step with the game server: the
// background poller and the BattlEye -> DB ban mirror. Those are the only
// parts here that call into rcon.go.
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
		SELECT p.username, p.last_ip, p.guid, p.total_playtime_seconds, p.last_seen
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
		if err := rows.Scan(&p.Name, &p.IP, &p.GUID, &p.TotalPlaytimeSecs, &lastSeen); err != nil {
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

// ---------------------------------------------------------------------------
// Flows storage (the Flow type and all flow logic live in flows.go)
// ---------------------------------------------------------------------------

// EnsureFlowsTable creates the flows table and, if it is empty, seeds one
// flow with the given name, script and schedule (left disabled).
//
// The schedule column also holds the flow's trigger: "manual", "on-join" or
// a cron expression (see triggerOf in flows.go).
func (db *Database) EnsureFlowsTable(seedName, seedScript, seedSchedule string) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS flows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			script TEXT NOT NULL,
			schedule TEXT NOT NULL DEFAULT '',
			schedule_enabled INTEGER NOT NULL DEFAULT 0,
			position INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		return err
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM flows").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := db.Exec("INSERT INTO flows (name, script, schedule, position) VALUES (?, ?, ?, 1)",
			seedName, seedScript, seedSchedule)
		return err
	}
	return nil
}

// ListFlows returns every flow in display order, with Trigger filled in from
// the schedule column.
func (db *Database) ListFlows() ([]Flow, error) {
	rows, err := db.Query("SELECT id, name, script, schedule, schedule_enabled FROM flows ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flows := []Flow{}
	for rows.Next() {
		var f Flow
		var en int
		if err := rows.Scan(&f.ID, &f.Name, &f.Script, &f.Schedule, &en); err != nil {
			return nil, err
		}
		f.ScheduleEnabled = en != 0
		fillTrigger(&f)
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

// GetFlow returns one flow (Trigger filled in), or sql.ErrNoRows.
func (db *Database) GetFlow(id int64) (Flow, error) {
	var f Flow
	var en int
	err := db.QueryRow("SELECT id, name, script, schedule, schedule_enabled FROM flows WHERE id = ?", id).
		Scan(&f.ID, &f.Name, &f.Script, &f.Schedule, &en)
	f.ScheduleEnabled = en != 0
	fillTrigger(&f)
	return f, err
}

// SaveFlow inserts a new flow (ID 0, placed last) or updates an existing one.
// f.Schedule must already be in stored form ("manual", "on-join" or cron);
// f.Trigger is not stored. Updating a missing ID returns sql.ErrNoRows.
func (db *Database) SaveFlow(f Flow) (int64, error) {
	en := 0
	if f.ScheduleEnabled {
		en = 1
	}
	if f.ID == 0 {
		res, err := db.Exec("INSERT INTO flows (name, script, schedule, schedule_enabled, position) VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM flows))",
			f.Name, f.Script, f.Schedule, en)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := db.Exec("UPDATE flows SET name = ?, script = ?, schedule = ?, schedule_enabled = ? WHERE id = ?",
		f.Name, f.Script, f.Schedule, en, f.ID)
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

// syncBanList reads the BattlEye list and mirrors it into the DB.
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
	if err := globalDatabase.SyncBans(bans); err != nil {
		return bans, err
	}
	return bans, nil
}

// startBackgroundSync tracks players (and periodically the ban list) in the
// background so the DB stays current without any web page open.
// It is the only caller of SyncPlayers.
//
// Every `interval`:
//   - Poll `players` (single attempt, 3s wait).
//   - On failure, count consecutive failures. At exactly 3 in a row the server
//     is assumed down and SyncPlayers(nil) closes all open sessions once, so
//     playtime stops accruing while it's offline.
//   - On success, reset the failure counter, sync the parsed list into the DB
//     and pass it to playersPolled, which starts player-join flows.
//   - On the 1st poll and every 6th after (~30s at a 5s interval), also
//     reconcile the ban list, since it changes rarely.
func startBackgroundSync(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		fails := 0
		n := 0
		for range t.C {
			n++
			out, acked, err := runShared("players", 3*time.Second, false, "")
			if err != nil || !acked {
				fails++
				// Server probably down: close open sessions once so
				// playtime doesn't keep accruing while it's offline.
				if fails == 3 {
					if err := globalDatabase.SyncPlayers(nil); err != nil {
						log.Printf("poller: close sessions: %v", err)
					}
					playersLost() // greet everyone again when they reconnect (flows.go)
				}
				continue
			}
			fails = 0
			players := parsePlayers(out)
			if err := globalDatabase.SyncPlayers(players); err != nil {
				log.Printf("poller: sync: %v", err)
			}
			playersPolled(players) // starts player-join flows for new arrivals (flows.go)
			// Ban list changes rarely: reconcile at start and every 6th poll (~30s).
			if n%6 == 1 {
				if _, err := syncBanList(); err != nil {
					log.Printf("poller: bans: %v", err)
				}
			}
		}
	}()
}
