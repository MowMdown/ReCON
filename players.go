package main

import (
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// players.go overview
// ----------------------------------------------------------------------------
// Player sessions, driven by what BattlEye tells a logged-in RCON session:
//
//	Player #3 Name (1.2.3.4:2304) connected        -> joining: number, name, IP, time
//	Player #3 Name - BE GUID: <guid>               -> GUID known: the session opens
//	Verified GUID (<guid>) of player #3 Name          (whichever line comes first)
//	`players` poll without "(Lobby)" in the name   -> finished loading
//	Player #3 Name (<guid>) has been kicked by ... -> the session will end as kicked
//	Player #3 Name disconnected                    -> the session closes
//	(Global) Name: text                            -> kept with the player for
//	                                                  this session (memory only):
//	                                                  the Players page's Last
//	                                                  Message and For each flows
//
// The database is only written when one of those happens, never for a player
// who simply stays online; offline players are history and aren't touched.
// Playtime counts from the connect line, so loading time is included.
//
// The tracker is ReCON's list of who is online: flows, chat commands and the
// Players page all read it (onlinePlayers). A `players` poll (pollPlayersNow,
// rcon.go) fills in what the messages can't tell, and is only sent when
// needed (playersPollWanted): after every RCON login, every 5 seconds while
// someone is still loading (until "(Lobby)" leaves their name), while a
// Players page is open (for pings), and right before a For each runs. A
// poll also catches anything missed while RCON wasn't listening: a player
// it doesn't list is closed, one it lists that the tracker doesn't know is
// opened.
//
// Server restarts: when the server stops answering, every session is closed
// at the time it last answered (serverLost) but remembered. A player who
// comes back within restartGrace of ReCON's next login keeps the same
// session, with the time the server was down not counted (ResumeSession).
// ============================================================================

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
	historyID   int64      // after serverLost: the session's history row, for ResumeSession
}

// maxSessionChats is how many chat lines are kept per player online.
const maxSessionChats = 100

// lastChat is a line a player wrote in chat. It is kept only in memory, on
// the player's tracker entry (their session's lines), so it goes when they
// disconnect. Messages sent
// to a player are commands ReCON sends, not chat lines from the server, so
// they never end up here. The chat history in the database is separate
// (startChatRecorder, db.go).
type lastChat struct {
	Text    string    // what they typed, without the "(Global) Name: " prefix
	Channel string    // Global, Side, Direct, Vehicle, ...
	At      time.Time // when it was sent
	Seq     int64     // the server event's number (serverEvents), increasing
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

	// Server restarts (serverLost): who was online when it stopped
	// answering, by GUID, when that was, and until when they may come back
	// to the same session (restartGrace after the next login).
	away       map[string]*trackedPlayer
	downAt     time.Time
	graceUntil time.Time
}{byID: map[int]*trackedPlayer{}, ended: map[string]time.Time{}, away: map[string]*trackedPlayer{}}

// restartGrace is how long after ReCON logs in again following an outage
// a player may reconnect and keep the session they had before it.
var restartGrace = 60 * time.Second

// startPlayerTracker feeds every server message to the tracker for as long
// as ReCON runs.
func startPlayerTracker() {
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			if p, ok := trackEvent(ev); ok {
				startJoinFlows([]Player{p}) // flows_join.go
			}
		}
	}()
}

// trackEvent updates the tracker (and the database) for one server message.
// It returns a player who has finished joining (see playersPolledTracker),
// for the rare GUID line that arrives after they have loaded in.
func trackEvent(ev ServerEvent) (joined Player, ok bool) {
	switch ev.Kind {
	case eventConnect, eventGUID, eventKick, eventDisconnect, eventChat:
	default:
		return
	}
	tracker.Lock()
	defer tracker.Unlock()
	if ev.Kind == eventChat {
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
	case eventConnect:
		if t != nil { // the number was taken by someone we never saw leave
			closeTracked(t, t.lastSeen)
		}
		tracker.byID[ev.PlayerID] = &trackedPlayer{id: ev.PlayerID, name: bareName(ev.Name), ip: ev.IP, port: ev.Port,
			connectedAt: ev.Time, lastSeen: ev.Time, lobby: true, joining: true}

	case eventGUID:
		if t == nil { // the connect line was missed: count from now
			t = &trackedPlayer{id: ev.PlayerID, name: bareName(ev.Name), connectedAt: ev.Time, lastSeen: ev.Time, lobby: true}
			tracker.byID[ev.PlayerID] = t
		}
		if t.guid == "" {
			t.guid = ev.GUID
			openTracked(t, ev.Time)
		}
		if t.joining && !t.lobby {
			t.joining = false
			return Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: t.name}, true
		}

	case eventKick:
		if t == nil {
			return
		}
		if t.guid == "" && ev.GUID != "" {
			t.guid = ev.GUID
		}
		t.kickedAt = ev.Time

	case eventDisconnect:
		if t == nil {
			return
		}
		closeTracked(t, ev.Time)
	}
	return
}

// openTracked writes a player's new session once their GUID is known, or,
// for a player back within restartGrace after an outage, opens the session
// they had again (seen at `at`). Caller holds the tracker lock.
func openTracked(t *trackedPlayer, at time.Time) {
	if t.opened || t.guid == "" {
		return
	}
	if old := tracker.away[t.guid]; old != nil && !at.After(tracker.graceUntil) {
		delete(tracker.away, t.guid)
		if err := globalDatabase.ResumeSession(t.guid, old.historyID, t.id, at); err != nil {
			log.Printf("players: resuming the session of %s: %v", t.guid, err)
		} else {
			log.Printf("players: %s is back after the restart, same session", t.guid)
			t.opened, t.chats = true, old.chats
			return
		}
	}
	if err := globalDatabase.OpenSession(t.guid, t.name, t.ip, t.id, t.connectedAt); err != nil {
		log.Printf("players: opening the session of %s: %v", t.guid, err)
		return
	}
	t.opened = true
	steamPlayerJoined(t.guid, t.name) // STEAM_SEARCH_MODE=auto (steam.go)
}

// closeTracked ends a player's session at `at` and forgets them. Caller
// holds the tracker lock.
func closeTracked(t *trackedPlayer, at time.Time) {
	delete(tracker.byID, t.id)
	if !t.opened {
		return
	}
	t.historyID = 0
	ended := endDisconnected
	if !t.kickedAt.IsZero() {
		ended = endKicked
		if banned, err := globalDatabase.IsBanned(t.guid); err == nil && banned {
			ended = endBanned
		}
	}
	id, err := globalDatabase.CloseSession(t.guid, at, ended)
	if err != nil {
		log.Printf("players: closing the session of %s: %v", t.guid, err)
	}
	t.historyID = id
	tracker.ended[t.guid] = at
}

// playersPolledTracker reconciles the tracker with a `players` poll that
// was sent at sentAt. A tracked player it doesn't list (and no disconnect
// line came for) is closed at sentAt. It returns the players who were seen
// connecting and have now finished loading in, for the join flows.
func playersPolledTracker(players []Player, sentAt time.Time) (joined []Player) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.known = true
	listed := map[int]bool{}
	for _, p := range players {
		listed[p.ID] = true
		guid := strings.ToLower(p.GUID)
		t := tracker.byID[p.ID]
		if t != nil && t.guid != "" && guid != "" && t.guid != guid {
			closeTracked(t, t.lastSeen) // the number changed hands between polls
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
			t = &trackedPlayer{id: p.ID, name: bareName(p.Name), ip: p.IP, port: p.Port, guid: guid,
				connectedAt: sentAt, lastSeen: sentAt, lobby: inLobby(p)}
			tracker.byID[p.ID] = t
		}
		if t.guid == "" && guid != "" {
			t.guid = guid // the GUID line was missed
		}
		if t.ip == "" {
			t.ip, t.port = p.IP, p.Port
		}
		openTracked(t, sentAt)
		t.lastSeen, t.ping = sentAt, p.Ping
		if t.lobby && !inLobby(p) {
			t.lobby = false
			if t.opened {
				if err := globalDatabase.MarkLoaded(t.guid, sentAt); err != nil {
					log.Printf("players: marking %s loaded: %v", t.guid, err)
				}
			}
		}
		if t.joining && !t.lobby && t.guid != "" {
			t.joining = false
			joined = append(joined, Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: t.name})
		}
	}
	for id, t := range tracker.byID {
		if listed[id] || !t.connectedAt.Before(sentAt) {
			continue // listed, or connected after this poll was sent
		}
		closeTracked(t, sentAt)
	}
	for g, at := range tracker.ended {
		if sentAt.Sub(at) > time.Minute {
			delete(tracker.ended, g)
		}
	}
	return joined
}

// lobbySuffix is what BattlEye appends to a name while the player is still on
// the loading screen.
const lobbySuffix = "(Lobby)"

func inLobby(p Player) bool {
	f := strings.Fields(p.Name)
	return len(f) > 0 && strings.EqualFold(f[len(f)-1], lobbySuffix)
}

// onlineChat is an online player's last chat line, with their GUID so the
// caller can check the player number still belongs to the same player.
type onlineChat struct {
	guid string
	chat lastChat
}

// lastChats returns the last chat line of every player online who has
// written one this session, by player number.
func lastChats() map[int]onlineChat {
	tracker.Lock()
	defer tracker.Unlock()
	out := map[int]onlineChat{}
	for id, t := range tracker.byID {
		if n := len(t.chats); n > 0 {
			out[id] = onlineChat{t.guid, t.chats[n-1]}
		}
	}
	return out
}

// latestChatOf is the newest line a player online (by GUID) wrote this session.
func latestChatOf(guid string) (lastChat, bool) {
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		if t.guid != "" && strings.EqualFold(t.guid, guid) && len(t.chats) > 0 {
			return t.chats[len(t.chats)-1], true
		}
	}
	return lastChat{}, false
}

// sessionChat is one chat line and who (by GUID) wrote it.
type sessionChat struct {
	guid string
	chat lastChat
}

// chatsAfter returns every chat line players online wrote this session with
// a Seq greater than after, oldest first.
func chatsAfter(after int64) []sessionChat {
	tracker.Lock()
	defer tracker.Unlock()
	var out []sessionChat
	for _, t := range tracker.byID {
		if t.guid == "" {
			continue
		}
		for _, c := range t.chats {
			if c.Seq > after {
				out = append(out, sessionChat{t.guid, c})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].chat.Seq < out[b].chat.Seq })
	return out
}

// serverLost closes every open session when the game server stops answering
// (the heartbeat's 3rd miss in a row), at `at`, when it last answered. The
// players are remembered in tracker.away so those who come back soon after
// it does keep the same session (openTracked).
func serverLost(at time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	tracker.away = map[string]*trackedPlayer{}
	tracker.downAt, tracker.graceUntil, tracker.known = at, time.Time{}, false
	for _, t := range tracker.byID {
		opened := t.opened
		closeTracked(t, at)
		if opened && t.historyID != 0 {
			tracker.away[t.guid] = t
		}
	}
}

// serverLoggedIn is called on every login of the shared session (rcon.go).
// After an outage, it starts the restartGrace in which players who come
// back keep their session.
func serverLoggedIn(at time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	if len(tracker.away) > 0 && tracker.graceUntil.IsZero() {
		tracker.graceUntil = at.Add(restartGrace)
	}
}

// forgetAwayPlayers drops the players who didn't come back within
// restartGrace; their sessions stay closed at the time the server went down.
// The heartbeat calls it on every beat.
func forgetAwayPlayers(now time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
	if len(tracker.away) > 0 && !tracker.graceUntil.IsZero() && now.After(tracker.graceUntil) {
		tracker.away = map[string]*trackedPlayer{}
	}
}

// onlinePlayers is who is online, by player number, as the console log and
// the last poll tell it: the Name has " (Lobby)" while they load and Ping is
// from the last poll. known is false until a poll has listed who is on since
// the server last answered.
func onlinePlayers() (players []Player, known bool) {
	tracker.Lock()
	defer tracker.Unlock()
	players = make([]Player, 0, len(tracker.byID))
	for _, t := range tracker.byID {
		name := t.name
		if t.lobby {
			name += " " + lobbySuffix
		}
		players = append(players, Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: name})
	}
	sort.Slice(players, func(a, b int) bool { return players[a].ID < players[b].ID })
	return players, tracker.known
}

// playersPageSeen is when a Players page last asked for the online list
// (Unix milliseconds); pings are polled while one is open.
var playersPageSeen atomic.Int64

// playersPollWanted reports whether the heartbeat should poll `players`
// now: someone is still loading, or a Players page asked within the last
// two heartbeats.
func playersPollWanted(now time.Time) bool {
	if now.Sub(time.UnixMilli(playersPageSeen.Load())) < 2*heartbeatEvery {
		return true
	}
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		if t.lobby {
			return true
		}
	}
	return false
}
