package battleye

import (
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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
	Seq      int64     `json:"seq"` // set by rcon.Events.publish: 1, 2, 3, ... for this run of ReCON
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
