package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
