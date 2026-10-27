package players

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// trackedPlayer is one player on the server, by their player number.
type trackedPlayer struct {
	id          int
	name        string // without "(Lobby)"
	ip          string
	port        string
	ping        int    // from the last poll that listed them
	guid        string // lower case; "" until BattlEye sends it
	connectedAt time.Time
	lastSeen    time.Time  // the connect line, or the last poll that listed them
	lobby       bool       // still loading
	opened      bool       // the session is in the database
	kickedAt    time.Time  // set by a kick line; the session ends as kicked (or banned)
	chats       []lastChat // what they wrote in chat this session, oldest first (up to maxSessionChats)
	joining     bool       // seen connecting: join flows start once they have loaded in
	historyID   int64      // after the session closed: its history row, for ResumePlayerSession
	closedAt    time.Time  // when the session closed (tracker.recent)
	warnings    int        // !warning count this session (Warn)
}

// maxSessionChats is how many chat lines are kept per player online.
const maxSessionChats = 100

// lastChat is a line a player wrote in chat. It is kept only in memory, on
// the player's tracker entry (their session's lines), so it goes when they
// disconnect. Messages sent
// to a player are commands ReCON sends, not chat lines from the server, so
// they never end up here. The chat history in the database is separate
// (StartChatRecorder).
type lastChat struct {
	Text    string    // what they typed, without the "(Global) Name: " prefix
	Channel string    // Global, Side, Direct, Vehicle, ...
	At      time.Time // when it was sent
	Seq     int64     // the server event's number (rcon.Events), increasing
}

// tracker is every player the server has told us about. ended remembers when
// each GUID's last session closed, so a poll sent before a disconnect line
// arrived can't open the session again. One mutex covers the maps and the
// database writes, so the event and poll paths never interleave.
var tracker = struct {
	sync.Mutex
	byID  map[int]*trackedPlayer
	ended map[string]time.Time
	known bool // a poll has listed who is online since the server last answered

	// Server restarts (ServerWentDown): who was online when it stopped
	// answering or left just before (by GUID), when that was, and until when
	// they may come back to the same session (restartGrace after the next
	// login). recent is who left lately and wasn't banned, by GUID, for that.
	away       map[string]*trackedPlayer
	recent     map[string]*trackedPlayer
	downAt     time.Time
	graceUntil time.Time
}{byID: map[int]*trackedPlayer{}, ended: map[string]time.Time{}, away: map[string]*trackedPlayer{}, recent: map[string]*trackedPlayer{}}

// restartGrace is how long after ReCON logs in again following an outage
// a player may reconnect and keep the session they had before it.
var restartGrace = 5 * time.Minute

// restartLookback is how long before the server stops answering a player
// may have left and still keep their session after the restart: #shutdown,
// and a restart flow's Kick everyone, disconnect everyone just before.
var restartLookback = 2 * time.Minute

// What the tracker tells the rest of ReCON (set at startup, may be nil):
// players who have finished joining (the join flows), and every session it
// opens (the SteamID lookup).
var (
	onPlayersJoined func([]battleye.Player)
	onSessionOpened func(guid, name string)
)

// SetHooks sets what the tracker calls when players finish joining
// and when it opens a session.
func SetHooks(joined func([]battleye.Player), opened func(guid, name string)) {
	onPlayersJoined, onSessionOpened = joined, opened
}

// playersJoined hands players who have finished joining to onPlayersJoined.
func playersJoined(joined []battleye.Player) {
	if len(joined) > 0 && onPlayersJoined != nil {
		onPlayersJoined(joined)
	}
}

// StartTracker feeds every server message to the tracker for as long
// as ReCON runs, and starts the restart grace on every login.
func StartTracker() {
	rcon.OnLogin(serverLoggedIn)
	events, _ := rcon.Events.Subscribe(256)
	go func() {
		for ev := range events {
			if p, ok := trackServerEvent(ev); ok {
				playersJoined([]battleye.Player{p})
			}
			if ev.Kind == battleye.EventConnect {
				wakeLobbyWatcher()
			}
		}
	}()
	go watchLobbyPlayers()
}

// trackServerEvent updates the tracker (and the database) for one server message.
// It returns a player who has finished joining (see playersPolledTracker),
// for the rare GUID line that arrives after they have loaded in.
func trackServerEvent(ev battleye.ServerEvent) (joined battleye.Player, ok bool) {
	switch ev.Kind {
	case battleye.EventConnect, battleye.EventGUID, battleye.EventKick, battleye.EventDisconnect, battleye.EventChat:
	default:
		return
	}
	tracker.Lock()
	defer tracker.Unlock()
	if ev.Kind == battleye.EventChat {
		// Chat lines carry only the name: the line goes to the one tracked
		// player with that name, and is dropped if two players share it.
		var sender *trackedPlayer
		for _, t := range tracker.byID {
			if t.name == ev.Name {
				if sender != nil {
					return
				}
				sender = t
			}
		}
		if sender != nil {
			sender.chats = append(sender.chats, lastChat{Text: ev.Message, Channel: ev.Channel, At: ev.Time, Seq: ev.Seq})
			if len(sender.chats) > maxSessionChats {
				sender.chats = append([]lastChat(nil), sender.chats[len(sender.chats)-maxSessionChats:]...)
			}
		}
		return
	}
	t := tracker.byID[ev.PlayerID]
	switch ev.Kind {
	case battleye.EventConnect:
		if t != nil { // the number was taken by someone we never saw leave
			closeTrackedSession(t, t.lastSeen)
		}
		tracker.byID[ev.PlayerID] = &trackedPlayer{id: ev.PlayerID, name: battleye.BareName(ev.Name), ip: ev.IP, port: ev.Port,
			connectedAt: ev.Time, lastSeen: ev.Time, lobby: true, joining: true}

	case battleye.EventGUID:
		if t == nil { // the connect line was missed: count from now
			t = &trackedPlayer{id: ev.PlayerID, name: battleye.BareName(ev.Name), connectedAt: ev.Time, lastSeen: ev.Time, lobby: true}
			tracker.byID[ev.PlayerID] = t
		}
		if t.guid == "" {
			t.guid = ev.GUID
			openTrackedSession(t, ev.Time)
		}
		if t.joining && !t.lobby {
			t.joining = false
			return battleye.Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: t.name}, true
		}

	case battleye.EventKick:
		if t == nil {
			return
		}
		if t.guid == "" && ev.GUID != "" {
			t.guid = ev.GUID
		}
		t.kickedAt = ev.Time

	case battleye.EventDisconnect:
		if t == nil {
			return
		}
		closeTrackedSession(t, ev.Time)
	}
	return
}

// openTrackedSession writes a player's new session once their GUID is known, or,
// for a player back from the same IP within restartGrace after an outage,
// opens the session they had again (seen at `at`). Caller holds the tracker
// lock.
func openTrackedSession(t *trackedPlayer, at time.Time) {
	if t.opened || t.guid == "" {
		return
	}
	if old := tracker.away[t.guid]; old != nil && !at.After(tracker.graceUntil) {
		delete(tracker.away, t.guid)
		if t.ip != old.ip {
			// A new IP is a new session: the old one stays closed at the outage.
			log.Printf("players: %s is back after the restart from a new IP, new session", t.guid)
		} else if err := database.ResumePlayerSession(t.guid, old.historyID, t.id, at); err != nil {
			log.Printf("players: resuming the session of %s: %v", t.guid, err)
		} else {
			log.Printf("players: %s is back after the restart, same session", t.guid)
			t.opened, t.chats = true, old.chats
			return
		}
	}
	if err := database.OpenPlayerSession(t.guid, t.name, t.ip, t.id, t.connectedAt); err != nil {
		log.Printf("players: opening the session of %s: %v", t.guid, err)
		return
	}
	t.opened = true
	if onSessionOpened != nil {
		onSessionOpened(t.guid, t.name)
	}
}

// closeTrackedSession ends a player's session at `at` and forgets them. Caller
// holds the tracker lock.
func closeTrackedSession(t *trackedPlayer, at time.Time) {
	delete(tracker.byID, t.id)
	if !t.opened {
		return
	}
	t.historyID = 0
	ended := store.EndDisconnected
	if !t.kickedAt.IsZero() {
		ended = store.EndKicked
		if banned, err := database.IsPlayerBanned(t.guid); err == nil && banned {
			ended = store.EndBanned
		}
	}
	id, err := database.ClosePlayerSession(t.guid, at, ended)
	if err != nil {
		log.Printf("players: closing the session of %s: %v", t.guid, err)
	}
	t.historyID = id
	tracker.ended[t.guid] = at
	// Remembered for a while in case the server is going down (a #shutdown
	// disconnects everyone first): see ServerWentDown.
	for g, r := range tracker.recent {
		if at.Sub(r.closedAt) > restartLookback {
			delete(tracker.recent, g)
		}
	}
	if id != 0 && ended != store.EndBanned {
		t.closedAt = at
		tracker.recent[t.guid] = t
	}
}

// playersPolledTracker reconciles the tracker with a `players` poll that
// was sent at sentAt. A tracked player it doesn't list (and no disconnect
// line came for) is closed at sentAt. It returns the players who were seen
// connecting and have now finished loading in, for the join flows.
func playersPolledTracker(players []battleye.Player, sentAt time.Time) (joined []battleye.Player) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.known = true
	listed := map[int]bool{}
	for _, p := range players {
		listed[p.ID] = true
		guid := strings.ToLower(p.GUID)
		t := tracker.byID[p.ID]
		if t != nil && t.guid != "" && guid != "" && t.guid != guid {
			closeTrackedSession(t, t.lastSeen) // the number changed hands between polls
			t = nil
		}
		if t == nil {
			if guid == "" {
				continue // not verified yet: wait for BattlEye's GUID line
			}
			if end, ok := tracker.ended[guid]; ok && !end.Before(sentAt) {
				continue // left after this poll was sent: don't reopen them
			}
			// Online, but no lines seen for them (ReCON just started, or RCON
			// wasn't listening when they connected): count from now.
			t = &trackedPlayer{id: p.ID, name: battleye.BareName(p.Name), ip: p.IP, port: p.Port, guid: guid,
				connectedAt: sentAt, lastSeen: sentAt, lobby: battleye.InLobby(p)}
			tracker.byID[p.ID] = t
		}
		if t.guid == "" && guid != "" {
			t.guid = guid // the GUID line was missed
		}
		if t.ip == "" {
			t.ip, t.port = p.IP, p.Port
		}
		openTrackedSession(t, sentAt)
		t.lastSeen, t.ping = sentAt, p.Ping
		if t.lobby && !battleye.InLobby(p) {
			t.lobby = false
			if t.opened {
				if err := database.MarkSessionLoaded(t.guid, sentAt); err != nil {
					log.Printf("players: marking %s loaded: %v", t.guid, err)
				}
			}
		}
		if t.joining && !t.lobby && t.guid != "" {
			t.joining = false
			joined = append(joined, battleye.Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: t.name})
		}
	}
	for id, t := range tracker.byID {
		if listed[id] || !t.connectedAt.Before(sentAt) {
			continue // listed, or connected after this poll was sent
		}
		closeTrackedSession(t, sentAt)
	}
	for g, at := range tracker.ended {
		if sentAt.Sub(at) > time.Minute {
			delete(tracker.ended, g)
		}
	}
	return joined
}

// ServerWentDown closes every open session when the game server stops answering
// (the server watch's 3rd miss in a row), at `at`, when it last answered. The
// players are remembered in tracker.away, with those who left (not banned)
// in the restartLookback before, so those who come back soon after it does
// keep the same session (openTrackedSession).
func ServerWentDown(at time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.away = map[string]*trackedPlayer{}
	tracker.downAt, tracker.graceUntil, tracker.known = at, time.Time{}, false
	for _, t := range tracker.byID {
		closeTrackedSession(t, at)
	}
	for g, t := range tracker.recent {
		if !t.closedAt.Before(at.Add(-restartLookback)) {
			tracker.away[g] = t
		}
	}
	tracker.recent = map[string]*trackedPlayer{}
}

// serverLoggedIn is called on every login of the web session (rcon.OnLogin).
// After an outage, it starts the restartGrace in which players who come
// back keep their session.
func serverLoggedIn(at time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	if len(tracker.away) > 0 && tracker.graceUntil.IsZero() {
		tracker.graceUntil = at.Add(restartGrace)
	}
}

// ForgetAway drops the players who didn't come back within
// restartGrace; their sessions stay closed at the time the server went down.
// The server watch calls it every round.
func ForgetAway(now time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	if len(tracker.away) > 0 && !tracker.graceUntil.IsZero() && now.After(tracker.graceUntil) {
		tracker.away = map[string]*trackedPlayer{}
	}
}

// Warn adds a !warning to an online player's count for this session
// (it goes when they leave). When the count reaches threshold it starts
// again at 0 and reached is true, so two warnings at once can't both act.
// ok is false if they aren't online.
func Warn(guid string, threshold int) (count int, reached, ok bool) {
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		if strings.EqualFold(t.guid, guid) {
			t.warnings++
			count = t.warnings
			if count >= threshold {
				t.warnings, reached = 0, true
			}
			return count, reached, true
		}
	}
	return 0, false, false
}
