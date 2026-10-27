package store

import (
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// Session endings, stored in session_history.ended.
const (
	EndDisconnected = "disconnected"
	EndKicked       = "kicked"
	EndBanned       = "banned"
)

// OpenPlayerSession records a player coming online (the player tracker calls it once per
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
func (db *Database) ListOfflinePlayers() ([]battleye.Player, error) {
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

	players := []battleye.Player{}
	for rows.Next() {
		var p battleye.Player
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
func (db *Database) ListKnownPlayers() ([]battleye.Player, error) {
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

	players := []battleye.Player{}
	for rows.Next() {
		var p battleye.Player
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
func (db *Database) PlayerByGUID(guid string) (battleye.Player, error) {
	var p battleye.Player
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
		if battleye.GUIDPattern.MatchString(g) {
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
func (db *Database) PlayersBySteamIDs(ids []string) ([]SharedPlayer, error) {
	out := []SharedPlayer{}
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
			var p SharedPlayer
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
func (db *Database) PlayersByCode(code string) ([]battleye.Player, error) {
	rows, err := db.Query("SELECT username, last_ip, guid, role FROM players WHERE substr(lower(guid), -6) = lower(?)", code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []battleye.Player
	for rows.Next() {
		var p battleye.Player
		if err := rows.Scan(&p.Name, &p.IP, &p.GUID, &p.Role); err != nil {
			return nil, err
		}
		p.ID, p.Offline = -1, true
		out = append(out, p)
	}
	return out, rows.Err()
}

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
