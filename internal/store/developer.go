package store

import (
	"strings"
	"time"
)

// DevPlayer is one row of the Developer page's player list.
type DevPlayer struct {
	GUID    string   `json:"guid"`
	Name    string   `json:"name"`
	SteamID string   `json:"steamId"`
	IPs     []string `json:"ips"` // every IP they used, for the search
	Online  bool     `json:"online"`
}

// DevPlayers lists every player in the players table, by name, with what the
// Developer page's search matches on.
func (db *Database) DevPlayers() ([]DevPlayer, error) {
	rows, err := db.Query(`
		SELECT p.guid, p.username, COALESCE(p.steam_id, ''), p.last_ip,
			(SELECT group_concat(ip, ' ') FROM player_ips i WHERE i.guid = p.guid),
			EXISTS (SELECT 1 FROM active_sessions a WHERE a.guid = p.guid)
		FROM players p
		ORDER BY p.username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DevPlayer{}
	for rows.Next() {
		var p DevPlayer
		var last string
		var ips *string
		var online int
		if err := rows.Scan(&p.GUID, &p.Name, &p.SteamID, &last, &ips, &online); err != nil {
			return nil, err
		}
		p.IPs = []string{last}
		if ips != nil {
			for _, ip := range strings.Fields(*ips) {
				if ip != last {
					p.IPs = append(p.IPs, ip)
				}
			}
		}
		p.Online = online == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// DevTable is every row one table holds for a player, as column -> value.
type DevTable struct {
	Table string           `json:"table"`
	Rows  []map[string]any `json:"rows"`
}

// devTables are the tables with a guid column, and how their rows are ordered.
var devTables = []struct{ name, order string }{
	{"players", ""},
	{"active_sessions", ""},
	{"session_history", " ORDER BY id DESC"},
	{"player_ips", " ORDER BY last_seen DESC"},
	{"chat_messages", " ORDER BY id DESC"},
	{"player_notes", ""},
	{"bans", ""},
}

// DevPlayerRows reads every row every table holds for guid, all columns,
// nothing left out (the Developer page).
func (db *Database) DevPlayerRows(guid string) ([]DevTable, error) {
	out := make([]DevTable, 0, len(devTables))
	for _, t := range devTables {
		rows, err := db.Query("SELECT * FROM "+t.name+" WHERE lower(guid) = lower(?)"+t.order, guid)
		if err != nil {
			return nil, err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		dt := DevTable{Table: t.name, Rows: []map[string]any{}}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			row := make(map[string]any, len(cols))
			for i, c := range cols {
				if b, ok := vals[i].([]byte); ok {
					vals[i] = string(b)
				}
				row[c] = vals[i]
			}
			dt.Rows = append(dt.Rows, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, dt)
	}
	return out, nil
}

// DevTimedBan is a timed ban as it is held in memory.
type DevTimedBan struct {
	Name        string    `json:"name"`
	Reason      string    `json:"reason"`
	Expires     time.Time `json:"expires"`
	MinutesLeft int       `json:"minutesLeft"`
	Index       int       `json:"beIndex"` // position in BattlEye's list, -1 until read
}

// DevTimedBanOf is guid's timed ban, if one is held.
func (db *Database) DevTimedBanOf(guid string) (DevTimedBan, bool) {
	db.timed.Lock()
	defer db.timed.Unlock()
	tb, ok := db.timed.m[strings.ToLower(guid)]
	if !ok {
		return DevTimedBan{}, false
	}
	return DevTimedBan{Name: tb.name, Reason: tb.reason, Expires: tb.expires, MinutesLeft: minutesLeft(tb, time.Now()), Index: tb.index}, true
}
