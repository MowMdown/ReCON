package battleye

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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

// Server event kinds.
const (
	EventConnect    = "connect"    // Player #3 Name (1.2.3.4:2304) connected
	EventGUID       = "guid"       // Verified GUID (...) of player #3 Name / Player #3 Name - BE GUID: ...
	EventDisconnect = "disconnect" // Player #3 Name disconnected
	EventKick       = "kick"       // Player #3 Name (guid) has been kicked by BattlEye: ...
	EventChat       = "chat"       // (Global) Name: text
	EventAdmin      = "admin"      // RCon admin #0 (1.2.3.4:5678) logged in
	EventOther      = "other"      // anything not recognised; Text still has the line

	// Not from BattlEye: lines ReCON adds so the console shows both sides.
	EventSent  = "sent"  // > say -1 hello   (a command typed in the console page)
	EventReply = "reply" // the server's answer to a console command
	EventError = "error" // a console command that failed or got no answer
)

// ServerEvent is one parsed server message. Fields a kind doesn't have stay
// empty; PlayerID is -1 when the line names no player number (chat, admin,
// other).
type ServerEvent struct {
	Seq      int64     `json:"seq"` // set by game.Events.publish: 1, 2, 3, ... for this run of ReCON
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Text     string    `json:"text"` // the line exactly as BattlEye sent it
	PlayerID int       `json:"playerId"`
	Name     string    `json:"name,omitempty"`
	IP       string    `json:"ip,omitempty"`
	Port     string    `json:"port,omitempty"`
	GUID     string    `json:"guid,omitempty"`    // lower-cased
	Channel  string    `json:"channel,omitempty"` // chat: Global, Side, Direct, Vehicle, ...
	Message  string    `json:"message,omitempty"` // chat text, or the kick reason
	Source   string    `json:"source,omitempty"`  // sent: who asked ("web", "console", a flow's name)
}

var (
	connectRe    = regexp.MustCompile(`^Player #(\d+) (.+) \(([^()]+):(\d+)\) connected$`)
	verifiedRe   = regexp.MustCompile(`^Verified GUID \(([0-9a-fA-F]{32})\) of player #(\d+) (.+)$`)
	beGUIDRe     = regexp.MustCompile(`^Player #(\d+) (.+) - (?:BE )?GUID: ([0-9a-fA-F]{32})$`)
	disconnectRe = regexp.MustCompile(`^Player #(\d+) (.+) disconnected$`)
	kickRe       = regexp.MustCompile(`^Player #(\d+) (.+) \(([0-9a-fA-F]{32}|-)\) has been kicked by BattlEye: (.*)$`)
	adminRe      = regexp.MustCompile(`^RCon admin #(\d+) \(([^()]+):(\d+)\) logged in$`)
	chatRe       = regexp.MustCompile(`^\(([A-Za-z]+)\) (.+?): (.*)$`)
)

// ParseServerMessage turns one server message into a ServerEvent. Lines it
// doesn't recognise come back as EventOther with Text set, so nothing is lost
// if BattlEye's wording differs from what's expected here.
func ParseServerMessage(text string, at time.Time) ServerEvent {
	ev := ServerEvent{Time: at, Kind: EventOther, Text: text, PlayerID: -1}
	id := func(s string) int { n, _ := strconv.Atoi(s); return n }
	// Kick before disconnect/connect: its name part can contain anything.
	if m := kickRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.Message = EventKick, id(m[1]), m[2], m[4]
		if m[3] != "-" {
			ev.GUID = strings.ToLower(m[3])
		}
	} else if m := connectRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.IP, ev.Port = EventConnect, id(m[1]), m[2], m[3], m[4]
	} else if m := verifiedRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.GUID, ev.PlayerID, ev.Name = EventGUID, strings.ToLower(m[1]), id(m[2]), m[3]
	} else if m := beGUIDRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.GUID = EventGUID, id(m[1]), m[2], strings.ToLower(m[3])
	} else if m := disconnectRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name = EventDisconnect, id(m[1]), m[2]
	} else if m := adminRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.IP, ev.Port = EventAdmin, m[2], m[3]
	} else if m := chatRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.Channel, ev.Name, ev.Message = EventChat, m[1], m[2], m[3]
	}
	return ev
}

// serverMessage is called by readServerPackets() once per new server
// message: it logs it and hands it to onMessage.
func (l *Session) serverMessage(text string) {
	text = strings.TrimRight(text, "\r\n ")
	if text == "" {
		return
	}
	log.Printf("[%s] < %s", l.src, text)
	if l.onMessage != nil {
		l.onMessage(text)
	}
}

// KickReport summarises what a kickall run did (returned as JSON by /kickall).
type KickReport struct {
	Order     []int  `json:"order"`         // IDs in the order they were kicked
	Unacked   []int  `json:"unacked"`       // kick commands the server never acknowledged
	Remaining []int  `json:"remaining"`     // IDs still connected after the sweep
	Raw       string `json:"raw,omitempty"` // raw `players` output, only set when nothing could be parsed
}

// summary is the one log line for a kickall, e.g.
//
//	kickall "Server restarting": kicked [3 0], no ack for [], still connected []
func (rep *KickReport) Summary(reason string) string {
	return fmt.Sprintf("kickall %q: kicked %v, no ack for %v, still connected %v", CleanReason(reason), rep.Order, rep.Unacked, rep.Remaining)
}

// commandRunner is what KickAll needs: a Session (CLI,
// console) or webRunner (server mode).
type commandRunner interface {
	Run(command string, wait time.Duration) (string, bool, error)
}

// KickAll kicks every connected player and nothing else (#lock,
// #shutdown and the like are separate commands, sent before or after):
//  1. "players", then "kick <id> <reason>" for each, highest ID first
//  2. wait 500ms and re-read "players" to report who is still connected
//
// A transport error in step 1 aborts with an error. Unacknowledged kicks and
// a failed verification are reported in the report.
func KickAll(s commandRunner, reason string) (*KickReport, error) {
	reason = CleanReason(reason)
	out, _, err := s.Run("players", 3*time.Second)
	if err != nil {
		return nil, err
	}
	rep := &KickReport{Order: playerIDs(out), Unacked: []int{}, Remaining: []int{}}
	if len(rep.Order) == 0 {
		rep.Raw = out // help the caller debug why nothing was parsed
	}
	for _, id := range rep.Order {
		cmd := strings.TrimSpace(fmt.Sprintf("kick %d %s", id, reason))
		if _, acked, err := s.Run(cmd, time.Second); err != nil {
			return nil, err
		} else if !acked {
			rep.Unacked = append(rep.Unacked, id)
		}
	}
	if len(rep.Order) > 0 {
		time.Sleep(500 * time.Millisecond) // let the server process the kicks
		if out, _, err := s.Run("players", 3*time.Second); err == nil {
			rep.Remaining = playerIDs(out)
		}
	}
	return rep, nil
}
