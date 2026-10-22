package main

import (
	"log"
	"strings"
	"sync"
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
//
// The database is only written when one of those happens, never for a player
// who simply stays online; offline players are history and aren't touched.
// Playtime counts from the connect line, so loading time is included.
//
// The 5-second `players` poll (db.go) fills in what the messages can't tell:
// when "(Lobby)" leaves a name, and anything missed while RCON wasn't
// listening (ReCON starting with players already on, a lost line). A tracked
// player missing from two polls in a row with no disconnect line is closed at
// the last time they were seen.
// ============================================================================

// trackedPlayer is one player on the server, by their player number.
type trackedPlayer struct {
	id          int
	name        string // without "(Lobby)"
	ip          string
	guid        string // lower case; "" until BattlEye sends it
	connectedAt time.Time
	lastSeen    time.Time // the connect line, or the last poll that listed them
	lobby       bool      // still loading
	opened      bool      // the session is in the database
	kickedAt    time.Time // set by a kick line; the session ends as kicked (or banned)
	misses      int       // polls in a row that didn't list them
}

// tracker is every player the server has told us about. ended remembers when
// each GUID's last session closed, so a poll sent before a disconnect line
// arrived can't open the session again. One mutex covers the map and the
// database writes, so the event and poll paths never interleave.
var tracker = struct {
	sync.Mutex
	byID  map[int]*trackedPlayer
	ended map[string]time.Time
}{byID: map[int]*trackedPlayer{}, ended: map[string]time.Time{}}

// trackerMissesToClose is how many polls in a row may miss a player (with no
// disconnect line) before their session is closed anyway.
const trackerMissesToClose = 2

// startPlayerTracker feeds every server message to the tracker for as long
// as ReCON runs.
func startPlayerTracker() {
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			trackEvent(ev)
		}
	}()
}

// trackEvent updates the tracker (and the database) for one server message.
func trackEvent(ev ServerEvent) {
	switch ev.Kind {
	case eventConnect, eventGUID, eventKick, eventDisconnect:
	default:
		return
	}
	tracker.Lock()
	defer tracker.Unlock()
	t := tracker.byID[ev.PlayerID]
	switch ev.Kind {
	case eventConnect:
		if t != nil { // the number was taken by someone we never saw leave
			closeTracked(t, t.lastSeen)
		}
		tracker.byID[ev.PlayerID] = &trackedPlayer{id: ev.PlayerID, name: bareName(ev.Name), ip: ev.IP,
			connectedAt: ev.Time, lastSeen: ev.Time, lobby: true}

	case eventGUID:
		if t == nil { // the connect line was missed: count from now
			t = &trackedPlayer{id: ev.PlayerID, name: bareName(ev.Name), connectedAt: ev.Time, lastSeen: ev.Time, lobby: true}
			tracker.byID[ev.PlayerID] = t
		}
		if t.guid == "" {
			t.guid = ev.GUID
			openTracked(t)
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
}

// openTracked writes a player's new session once their GUID is known.
func openTracked(t *trackedPlayer) {
	if t.opened || t.guid == "" {
		return
	}
	if err := globalDatabase.OpenSession(t.guid, t.name, t.ip, t.id, t.connectedAt); err != nil {
		log.Printf("players: opening the session of %s: %v", t.guid, err)
		return
	}
	t.opened = true
}

// closeTracked ends a player's session at `at` and forgets them. Caller
// holds the tracker lock.
func closeTracked(t *trackedPlayer, at time.Time) {
	delete(tracker.byID, t.id)
	if !t.opened {
		return
	}
	ended := endDisconnected
	if !t.kickedAt.IsZero() {
		ended = endKicked
		if banned, err := globalDatabase.IsBanned(t.guid); err == nil && banned {
			ended = endBanned
		}
	}
	if err := globalDatabase.CloseSession(t.guid, at, ended); err != nil {
		log.Printf("players: closing the session of %s: %v", t.guid, err)
	}
	tracker.ended[t.guid] = at
}

// playersPolledTracker reconciles the tracker with a `players` poll that
// was sent at sentAt.
func playersPolledTracker(players []Player, sentAt time.Time) {
	tracker.Lock()
	defer tracker.Unlock()
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
			t = &trackedPlayer{id: p.ID, name: bareName(p.Name), ip: p.IP, guid: guid,
				connectedAt: sentAt, lastSeen: sentAt, lobby: inLobby(p)}
			tracker.byID[p.ID] = t
		}
		if t.guid == "" && guid != "" {
			t.guid = guid // the GUID line was missed
		}
		if t.ip == "" {
			t.ip = p.IP
		}
		openTracked(t)
		t.lastSeen, t.misses = sentAt, 0
		if t.lobby && !inLobby(p) {
			t.lobby = false
			if t.opened {
				if err := globalDatabase.MarkLoaded(t.guid, sentAt); err != nil {
					log.Printf("players: marking %s loaded: %v", t.guid, err)
				}
			}
		}
	}
	for id, t := range tracker.byID {
		if listed[id] || !t.connectedAt.Before(sentAt) {
			continue // listed, or connected after this poll was sent
		}
		if t.misses++; t.misses >= trackerMissesToClose {
			closeTracked(t, t.lastSeen)
		}
	}
	for g, at := range tracker.ended {
		if sentAt.Sub(at) > time.Minute {
			delete(tracker.ended, g)
		}
	}
}

// serverLost closes every open session when the game server stops answering
// (the poller's 3rd miss in a row), each at the last time the player was seen.
func serverLost() {
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		closeTracked(t, t.lastSeen)
	}
}
