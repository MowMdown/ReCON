package store

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

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
func (db *Database) SyncBansFromServer(bans []battleye.Ban, readAt time.Time) ([]storedBan, error) {
	live := make(map[string]battleye.Ban, len(bans))
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
func (db *Database) listPermanentBans(where string, args ...any) ([]battleye.Ban, error) {
	rows, err := db.Query("SELECT lower(guid), player_name, reason, be_index FROM bans "+where+
		" ORDER BY be_index < 0, be_index, player_name COLLATE NOCASE", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []battleye.Ban{}
	for rows.Next() {
		b := battleye.Ban{Perm: true, Minutes: "perm"}
		if err := rows.Scan(&b.GUID, &b.Name, &b.Reason, &b.Index); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// timedBanList is the timed bans as the pages show them, at `now`.
func (db *Database) timedBanList(now time.Time) []battleye.Ban {
	db.timed.Lock()
	defer db.timed.Unlock()
	out := make([]battleye.Ban, 0, len(db.timed.m))
	for g, tb := range db.timed.m {
		out = append(out, battleye.Ban{GUID: g, Name: tb.name, Reason: tb.reason, Index: tb.index, Minutes: strconv.Itoa(minutesLeft(tb, now))})
	}
	return out
}

// minutesLeft rounds up: 30 seconds left shows as 1 minute.
func minutesLeft(tb timedBan, now time.Time) int {
	return max(int((tb.expires.Unix()-now.Unix()+59)/60), 0)
}

// stateOf is a ban as BanState.
func stateOf(b battleye.Ban) BanState {
	st := BanState{Banned: true, Perm: b.Perm, MinutesLeft: -1, Reason: b.Reason}
	if !b.Perm {
		st.MinutesLeft, _ = strconv.Atoi(b.Minutes)
	}
	return st
}

// ListAllBans returns every ban for the Bans page, in BattlEye's list order
// (bans not placed in BattlEye's list yet go last).
func (db *Database) ListAllBans() ([]battleye.Ban, error) {
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
func (db *Database) AllBanStates() (map[string]BanState, error) {
	list, err := db.ListAllBans()
	if err != nil {
		return nil, err
	}
	out := make(map[string]BanState, len(list))
	for _, b := range list {
		out[b.GUID] = stateOf(b)
	}
	return out, nil
}

// GetBanState is one GUID's ban status (Banned false when not banned).
func (db *Database) GetBanState(guid string) (BanState, error) {
	g := strings.ToLower(guid)
	db.timed.Lock()
	tb, ok := db.timed.m[g]
	db.timed.Unlock()
	if ok {
		return BanState{Banned: true, MinutesLeft: minutesLeft(tb, time.Now()), Reason: tb.reason}, nil
	}
	list, err := db.listPermanentBans("WHERE lower(guid) = ?", g)
	if err != nil || len(list) == 0 {
		return BanState{MinutesLeft: -1}, err
	}
	return stateOf(list[0]), nil
}

// BanState is one player's ban as the pages show it. MinutesLeft is -1 when
// it isn't known (a permanent ban, or a timed one not read from BattlEye yet).
type BanState struct {
	Banned      bool   `json:"banned"`
	Perm        bool   `json:"perm"`
	MinutesLeft int    `json:"minutesLeft"`
	Reason      string `json:"reason"`
}

// longerBan reports whether a outlasts b.
func longerBan(a, b battleye.Ban) bool {
	if a.Perm != b.Perm {
		return a.Perm
	}
	am, _ := strconv.Atoi(a.Minutes)
	bm, _ := strconv.Atoi(b.Minutes)
	return am > bm
}

// PermanentBan is a permanent ban with everything ReCON knows about it: what
// Export saves and Import reads (bans.json).
type PermanentBan struct {
	GUID    string `json:"guid"`
	Name    string `json:"name"`
	IP      string `json:"ip,omitempty"`
	Reason  string `json:"reason"`
	SteamID string `json:"steamId,omitempty"`
}

// PermanentBans returns every permanent ban, in BattlEye's list order (bans
// not in BattlEye's list yet go last).
func (db *Database) PermanentBans() ([]PermanentBan, error) {
	rows, err := db.Query(`SELECT lower(guid), player_name, ip, reason, COALESCE(steam_id, '') FROM bans
		ORDER BY be_index < 0, be_index, player_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PermanentBan{}
	for rows.Next() {
		var b PermanentBan
		if err := rows.Scan(&b.GUID, &b.Name, &b.IP, &b.Reason, &b.SteamID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddPermanentBan records an imported permanent ban. A GUID that is already
// banned permanently is left as it is and added is false; one with a timed
// ban becomes permanent. The SteamID is kept when given, else copied from
// players when known.
func (db *Database) AddPermanentBan(b PermanentBan) (added bool, err error) {
	g := strings.ToLower(b.GUID)
	res, err := db.Exec(`
		INSERT INTO bans (guid, player_name, ip, reason, perm, steam_id)
		VALUES (?, ?, ?, ?, 1, COALESCE(NULLIF(?, ''), (SELECT NULLIF(steam_id, '') FROM players WHERE lower(guid) = ?)))
		ON CONFLICT(guid) DO NOTHING`,
		g, b.Name, b.IP, b.Reason, b.SteamID, g)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	db.timed.Lock()
	delete(db.timed.m, g)
	db.timed.Unlock()
	return true, nil
}
