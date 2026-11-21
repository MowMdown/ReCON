package game

import (
	"strings"
	"time"
)

// DevTracked is a tracker entry with every field, for the Developer page.
type DevTracked struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	IP          string     `json:"ip"`
	Port        string     `json:"port"`
	Ping        int        `json:"ping"`
	GUID        string     `json:"guid"`
	ConnectedAt time.Time  `json:"connectedAt"`
	LastSeen    time.Time  `json:"lastSeen"`
	Lobby       bool       `json:"lobby"`
	Opened      bool       `json:"opened"`
	KickedAt    *time.Time `json:"kickedAt"`
	Joining     bool       `json:"joining"`
	Warnings    int        `json:"warnings"`
	HistoryID   int64      `json:"historyID"`
	ClosedAt    *time.Time `json:"closedAt"`
	Chats       []lastChat `json:"chats"`
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func devTracked(t *trackedPlayer) *DevTracked {
	if t == nil {
		return nil
	}
	return &DevTracked{
		ID: t.id, Name: t.name, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid,
		ConnectedAt: t.connectedAt, LastSeen: t.lastSeen, Lobby: t.lobby, Opened: t.opened,
		KickedAt: timeOrNil(t.kickedAt), Joining: t.joining, Warnings: t.warnings,
		HistoryID: t.historyID, ClosedAt: timeOrNil(t.closedAt),
		Chats: append([]lastChat{}, t.chats...),
	}
}

// DevTracker is everything the player tracker holds about one GUID.
type DevTracker struct {
	Online       *DevTracked `json:"online"`       // tracker.byID: on the server now
	Away         *DevTracked `json:"away"`         // tracker.away: online when the server went down
	Recent       *DevTracked `json:"recent"`       // tracker.recent: left lately (restart grace)
	SessionEnded *time.Time  `json:"sessionEnded"` // tracker.ended: when their last session closed
	// The tracker itself: whether a poll has listed who is online since the
	// server last answered, and the restart grace.
	Known      bool       `json:"trackerKnown"`
	DownAt     *time.Time `json:"serverDownAt"`
	GraceUntil *time.Time `json:"graceUntil"`
}

// DevTrackerOf is what the tracker holds about guid.
func DevTrackerOf(guid string) DevTracker {
	g := strings.ToLower(guid)
	tracker.Lock()
	defer tracker.Unlock()
	d := DevTracker{Known: tracker.known, DownAt: timeOrNil(tracker.downAt), GraceUntil: timeOrNil(tracker.graceUntil)}
	for _, t := range tracker.byID {
		if t.guid == g {
			d.Online = devTracked(t)
		}
	}
	d.Away = devTracked(tracker.away[g])
	d.Recent = devTracked(tracker.recent[g])
	if at, ok := tracker.ended[g]; ok {
		d.SessionEnded = timeOrNil(at)
	}
	return d
}

// DevOnlineIDs is each tracked player's number, by lower-case GUID.
func DevOnlineIDs() map[string]int {
	tracker.Lock()
	defer tracker.Unlock()
	out := make(map[string]int, len(tracker.byID))
	for id, t := range tracker.byID {
		if t.guid != "" {
			out[t.guid] = id
		}
	}
	return out
}

// DevBanState is what package game holds about a GUID's bans besides the
// database: the next timed ban expiry it waits for, and an import still
// waiting to be placed.
func DevBanState(guid string) map[string]any {
	out := map[string]any{"nextExpiryCheck": nil, "importPending": false}
	if n := nextBanExpiry.Load(); n > 0 {
		out["nextExpiryCheck"] = time.Unix(n, 0)
	}
	// A ban, unban or sync holds the lock while it talks to the server; the
	// page doesn't wait for it.
	if BanListLock.TryLock() {
		out["importPending"] = importedPending[strings.ToLower(guid)]
		BanListLock.Unlock()
	} else {
		out["importPending"] = "unknown (the ban list is busy)"
	}
	return out
}
