package battleye

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LobbySuffix is what BattlEye appends to a name while the player is still on
// the loading screen.
const LobbySuffix = "(Lobby)"

func InLobby(p Player) bool {
	f := strings.Fields(p.Name)
	return len(f) > 0 && strings.EqualFold(f[len(f)-1], LobbySuffix)
}

// Player is one row of the BattlEye `players` output, also the JSON shape
// returned by /api/players (offline players come from the DB).
type Player struct {
	ID                int    `json:"id"`
	IP                string `json:"ip"`
	Port              string `json:"port"`
	Ping              int    `json:"ping"`
	GUID              string `json:"guid"`
	Name              string `json:"name"`
	Offline           bool   `json:"offline"`
	TotalPlaytimeSecs int64  `json:"totalPlaytimeSeconds"`
	LastSeenUnix      int64  `json:"lastSeenUnix"` // offline players: when they were last seen (Unix seconds)
	Role              int    `json:"role"`         // 0 Player, 1 Mod, 2 Admin (from the database; filled in by the pages' API)
	// Online players: the newest line they wrote in chat this session (not messages sent
	// to them), its channel and when (Unix seconds). Filled in by /api/players.
	LastMessage        string `json:"lastMessage,omitempty"`
	LastMessageChannel string `json:"lastMessageChannel,omitempty"`
	LastMessageUnix    int64  `json:"lastMessageUnix,omitempty"`
}

// Ban is one entry of the GUID section of the BattlEye `bans` output, also the
// JSON shape returned by /api/bans.
type Ban struct {
	Index   int    `json:"index"`   // position in the BattlEye list (shifts after each removal!)
	GUID    string `json:"guid"`    // banned GUID, lower-cased
	Name    string `json:"name"`    // last-known player name, filled from the DB (BattlEye doesn't store it)
	Minutes string `json:"minutes"` // minutes left as text, or "perm"
	Perm    bool   `json:"perm"`    // true for permanent bans
	Reason  string `json:"reason"`
}

// CleanReason trims whitespace and replaces CR/LF with spaces so user-supplied
// text can't break out of a single RCON command line.
func CleanReason(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// ParsePlayersReply turns `players` output into Player structs. Each data line is
//
//	12   203.0.113.5:2304   47   a1b2c3...(OK)   Some Name
//
// i.e. id, ip:port, ping, guid(status), then the name (which may contain
// spaces). Header, separator and footer lines fail a check and are skipped.
func ParsePlayersReply(out string) []Player {
	players := []Player{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		host, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ping, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		guid, _, _ := strings.Cut(fields[3], "(") // strip the (OK)/(?) suffix
		if !GUIDPattern.MatchString(guid) {
			guid = "" // "-": not verified yet; picked up on a later poll
		}
		name := strings.Join(fields[4:], " ")
		players = append(players, Player{ID: id, IP: host, Port: port, Ping: ping, GUID: guid, Name: name})
	}
	return players
}

// findPlayerByID returns the connected player with the given ID, if any.
func findPlayerByID(out string, id int) (Player, bool) {
	for _, p := range ParsePlayersReply(out) {
		if p.ID == id {
			return p, true
		}
	}
	return Player{}, false
}

// BareName drops the "(Lobby)" tag BattlEye adds while a player is loading.
func BareName(name string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), LobbySuffix))
}

// playerIDs returns every player ID in `players` output, highest first (the
// order kickall kicks in).
func playerIDs(out string) []int {
	ids := []int{}
	for _, p := range ParsePlayersReply(out) {
		ids = append(ids, p.ID)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	return ids
}

// GUIDPattern matches a BattlEye GUID: exactly 32 hex characters.
var GUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// ParseBansReply parses BattlEye `bans` output, e.g.
//
//	GUID Bans:
//	[#] [GUID] [Minutes left] [Reason]
//	----------------------------------------
//	0  49aacf68461f67163d0a2ecb6a4cee94 perm testing
//
//	IP Bans:
//	...
//
// Only the GUID section is read. ok is false when the "GUID Bans" header
// wasn't found, so an unrecognised reply is never mistaken for an empty list.
func ParseBansReply(out string) (bans []Ban, ok bool) {
	bans = []Ban{}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "guid bans"):
			section, ok = "guid", true
			continue
		case strings.HasPrefix(low, "ip bans"):
			section = "ip"
			continue
		}
		if section != "guid" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || !GUIDPattern.MatchString(fields[1]) {
			continue
		}
		perm := strings.EqualFold(fields[2], "perm")
		if !perm {
			if _, err := strconv.Atoi(fields[2]); err != nil {
				continue
			}
		}
		bans = append(bans, Ban{
			Index:   idx,
			GUID:    strings.ToLower(fields[1]),
			Minutes: strings.ToLower(fields[2]),
			Perm:    perm,
			Reason:  strings.Join(fields[3:], " "),
		})
	}
	return bans, ok
}
