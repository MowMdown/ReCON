package watch

import (
	"log"
	"time"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
)

// pollEvery is how often `players` is polled while the server is answering.
// The poll keeps the player tracker (package players) up to date: pings, and anyone the
// console log missed leaving is closed within one poll.
var pollEvery = 5 * time.Second

// heartbeatEvery is how often an empty command is sent, whether or not the
// server is answering. BattlEye drops a client it hasn't heard from in about
// 45 seconds. The heartbeat is also the only thing that logs in, so a server
// that isn't answering gets at most one login packet per heartbeatEvery;
// while it's down, the heartbeat is the only check (polls pause).
var heartbeatEvery = 30 * time.Second

var heartbeatWait = 2 * time.Second

// Start checks the web session every pollEvery, starting now:
//
//   - Delete timed bans that have run out, if one is due (bans.ExpireDue), and let go
//     of players who didn't come back after a restart in time
//     (players.ForgetAway).
//   - Every heartbeatEvery, the heartbeat: log in on a new socket if there is
//     no live session (rcon.Login), then send an empty command
//     (heartbeatWait). While the server is down and the empty command goes
//     unanswered, log in again on the same socket (rcon.Relogin).
//   - While the server is answering, poll `players` (players.PollNow).
//     Neither is logged.
//   - A round with no answer counts as a miss. At exactly rcon.DownAfterMisses in
//     a row the server is taken to be down: open sessions are closed once
//     (players.ServerWentDown) so playtime stops while it's offline. From
//     then on only the heartbeat checks it. This is the only place that
//     decides the server isn't answering.
//   - An answered round resets the count, and reads the ban list after a
//     login and every bans.SyncEvery.
func Start() {
	go func() {
		var w serverWatch
		w.check(time.Now())
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for range t.C {
			w.check(time.Now())
		}
	}()
}

// serverWatch is what the watch goroutine remembers between rounds.
type serverWatch struct {
	lastAnswered  time.Time // when the server last answered
	lastHeartbeat time.Time // when the last heartbeat was sent
	lastBanSync   time.Time // when the ban list was last read
}

// isDown reports whether the server has missed rcon.DownAfterMisses checks in a row.
func (w *serverWatch) isDown() bool { return rcon.Fails.Load() >= rcon.DownAfterMisses }

// check is one round (see Start).
func (w *serverWatch) check(now time.Time) {
	bans.ExpireDue(now)
	players.ForgetAway(now)
	defer w.storeNextCheck(now)
	heartbeatDue := now.Sub(w.lastHeartbeat) >= heartbeatEvery
	if w.isDown() && !heartbeatDue {
		return // only the heartbeat checks a server that's down
	}
	answered := false
	if heartbeatDue {
		w.lastHeartbeat = now
		answered = w.heartbeat()
	}
	if !w.isDown() {
		answered = players.PollNow() || answered
	}
	rcon.Polled.Store(true)
	if !answered {
		if rcon.Fails.Add(1) == rcon.DownAfterMisses {
			down := w.lastAnswered
			if down.IsZero() {
				down = now
			}
			players.ServerWentDown(down)
		}
		return
	}
	w.answered(now)
}

// heartbeat logs in if there is no live session, sends an empty command,
// and, while the server is down and that goes unanswered, logs in again on
// the same socket. It reports whether the server answered. At most one login
// packet goes out per heartbeat.
func (w *serverWatch) heartbeat() bool {
	if err := rcon.Login(); err != nil {
		return false
	}
	_, acked, err := rcon.Run("", heartbeatWait, false, "")
	if err == nil && acked {
		return true
	}
	return w.isDown() && rcon.Relogin() && players.PollNow()
}

// answered records a round the server answered: the count starts again,
// and the ban list is read after a login and every bans.SyncEvery.
func (w *serverWatch) answered(now time.Time) {
	w.lastAnswered = now
	rcon.Fails.Store(0)
	rcon.Answered()
	if rcon.LoginPollDue.Swap(false) || time.Since(w.lastBanSync) >= bans.SyncEvery {
		w.lastBanSync = time.Now()
		if err := bans.SyncWithServer(); err != nil {
			log.Printf("rcon: bans: %v", err)
			w.lastBanSync = time.Now().Add(time.Minute - bans.SyncEvery) // try again in a minute
		}
	}
}

// storeNextCheck sets rcon.NextPoll for the status bar: the next round, or,
// while the server is down, the next heartbeat.
func (w *serverWatch) storeNextCheck(now time.Time) {
	next := now.Add(pollEvery)
	if w.isDown() {
		next = w.lastHeartbeat.Add(heartbeatEvery)
	}
	rcon.NextPoll.Store(next.UnixMilli())
}
