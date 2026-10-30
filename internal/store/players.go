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

// ErrNameMismatch is DeletePlayer's error when the name given isn't the
// player's stored name exactly.
var ErrNameMismatch = errors.New("the name doesn't match the player's name")

// DeletePlayer removes a player and everything recorded about them: their
// row (name, role, playtime, SteamID), open session, session history, IPs,
// chat history and note, in one transaction. name must be their stored name
// exactly (ErrNameMismatch), the safety check the Danger tab asks for. Their
// ban record, if any, stays: bans belong to the server's ban list. It
// returns sql.ErrNoRows if the GUID was never seen.
func (db *Database) DeletePlayer(guid, name string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stored string
	if err := tx.QueryRow("SELECT username FROM players WHERE lower(guid) = lower(?)", guid).Scan(&stored); err != nil {
		return err
	}
	if name != stored {
		return ErrNameMismatch
	}
	for _, table := range []string{"active_sessions", "session_history", "player_ips", "chat_messages", "player_notes", "players"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE lower(guid) = lower(?)", guid); err != nil {
			return err
		}
	}
	return tx.Commit()
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

type SharedPlayer struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type detailsIP struct {
	IP        string         `json:"ip"`
	FirstUsed int64          `json:"firstUsed"`
	LastUsed  int64          `json:"lastUsed"`
	Others    []SharedPlayer `json:"others"` // other players seen on this IP
}

// playerDetails is everything stored about one player, as GET
// /api/players/{guid}/details returns it. Times are Unix seconds. Online, ID
// and Ban are filled in by the web handler.
type playerDetails struct {
	Player   detailsPlayer `json:"player"`
	Sessions struct {
		Count      int              `json:"count"` // finished sessions
		AvgSeconds int64            `json:"avgSeconds"`
		Recent     []detailsSession `json:"recent"`
	} `json:"sessions"`
	IPs  []detailsIP `json:"ips"`
	Ban  BanState    `json:"ban"`
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
		e.FirstUsed, e.LastUsed, e.Others = unixOf(a), unixOf(b), []SharedPlayer{}
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
		var o SharedPlayer
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

// Chat directions.
const (
	ChatIn  = "in"  // the player wrote it
	ChatOut = "out" // ReCON sent it to them
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
