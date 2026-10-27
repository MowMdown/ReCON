package watch

import (
	"errors"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

// testDB opens a fresh database for the packages the server watch uses,
// and resets the player tracker.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	players.SetDatabase(db)
	bans.SetDatabase(db)
	players.Reset()
	return db
}

// resetWeb starts a test with no web session, and closes it and restores
// the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := rcon.Target()
	rcon.CloseSession()
	t.Cleanup(func() {
		rcon.SetTarget(oldAddr, oldPass)
		rcon.CloseSession()
	})
}

// TestOneLoginPerHeartbeat checks that a server which isn't answering gets
// at most one login packet per heartbeat (each queued packet becomes another
// "RCon admin logged in" when the server catches up), that commands never
// log in, and that an accepted login is followed by exactly one #unlock and
// no new login while it settles.
func TestOneLoginPerHeartbeat(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := test.NewServer(t)
	rcon.SetTarget(f.Addr(), "pw")
	oldBeat, oldPoll, oldReply, oldSettle := heartbeatWait, players.PollWait, battleye.ReloginReplyWait, rcon.ReloginSettle
	t.Cleanup(func() {
		heartbeatWait, players.PollWait, battleye.ReloginReplyWait, rcon.ReloginSettle = oldBeat, oldPoll, oldReply, oldSettle
		rcon.Fails.Store(0)
		rcon.LoginPollDue.Store(false)
	})
	heartbeatWait, players.PollWait, battleye.ReloginReplyWait = 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	rcon.Fails.Store(0)

	// Nothing is logged in yet: commands fail without logging in.
	f.SetSilent(true)
	for i := 0; i < 5; i++ {
		if _, _, err := rcon.Run("players", 10*time.Millisecond, false, ""); !errors.Is(err, rcon.ErrNotLoggedIn) {
			t.Fatalf("err = %v, want rcon.ErrNotLoggedIn", err)
		}
	}
	if l, _ := f.Counts(); l != 0 {
		t.Fatalf("commands sent %d login(s)", l)
	}

	var w serverWatch
	t0 := time.Now()
	logins := func() int { l, _ := f.Counts(); return l }
	// A minute of rounds against a silent server: one login per heartbeat
	// (new sockets while none is accepted).
	for i := 0; i <= 12; i++ {
		w.check(t0.Add(time.Duration(i) * pollEvery))
	}
	if l := logins(); l != 3 {
		t.Fatalf("%d login packets in a minute of rounds; want 3 (one per heartbeat)", l)
	}
	if rcon.Status() != "unavailable" {
		t.Fatalf("status %s", rcon.Status())
	}

	// The server takes logins but doesn't answer commands yet (it's starting
	// up): one login, one #unlock, and no new login while it settles.
	f.SetSilent(false)
	f.SetMute(true)
	for i := 6; i <= 12; i++ {
		w.check(t0.Add(time.Duration(i) * heartbeatEvery / 2))
	}
	if l, u := f.Counts(); l != 4 || u != 1 {
		t.Fatalf("while settling: %d login(s), %d #unlock(s); want 4 and 1", l, u)
	}
	// Once it has settled, a heartbeat logs in again on the same socket.
	rcon.ReloginSettle = 0
	w.check(t0.Add(7 * heartbeatEvery))
	if l, u := f.Counts(); l != 5 || u != 2 {
		t.Fatalf("after settling: %d login(s), %d #unlock(s); want 5 and 2", l, u)
	}
	// It answers: connected, and no more logins.
	f.SetMute(false)
	w.check(t0.Add(8 * heartbeatEvery))
	if rcon.Status() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", rcon.Status())
	}
	if l := logins(); l != 5 {
		t.Fatalf("%d login(s) once answering, want 5", l)
	}
}

// TestServerWatch checks the server watch: `players` every round and the
// heartbeat every heartbeatEvery while the server answers, the ban list
// after a login; three unanswered rounds mark the server down without
// logging in; while it's down only the heartbeat checks it.
func TestServerWatch(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := test.NewServer(t)
	rcon.SetTarget(f.Addr(), "pw")
	oldBeat, oldPoll, oldReply := heartbeatWait, players.PollWait, battleye.ReloginReplyWait
	t.Cleanup(func() {
		heartbeatWait, players.PollWait, battleye.ReloginReplyWait = oldBeat, oldPoll, oldReply
		rcon.Fails.Store(0)
		rcon.LoginPollDue.Store(false)
	})
	heartbeatWait, players.PollWait, battleye.ReloginReplyWait = 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	rcon.Fails.Store(0)

	sent := func() (beats, players, bans int) {
		for _, c := range f.Commands() {
			switch c {
			case "players":
				players++
			case "bans":
				bans++
			}
		}
		return f.Beats(), players, bans
	}
	want := func(when string, beats, players, bans int) {
		t.Helper()
		if b, p, n := sent(); b != beats || p != players || n != bans {
			t.Fatalf("%s: %d heartbeats, %d players, %d bans; want %d, %d, %d", when, b, p, n, beats, players, bans)
		}
	}
	var w serverWatch
	t0 := time.Now()
	w.check(t0) // logs in: heartbeat, players, and the ban list after the login
	want("first round", 1, 1, 1)
	w.check(t0.Add(pollEvery))
	want("second round", 1, 2, 1)
	w.check(t0.Add(heartbeatEvery))
	want("a heartbeat later", 2, 3, 1)

	f.SetSilent(true)
	logins, _ := f.Counts()
	for i := 1; i <= rcon.DownAfterMisses; i++ {
		w.check(t0.Add(heartbeatEvery + time.Duration(i)*pollEvery))
	}
	if rcon.Fails.Load() != rcon.DownAfterMisses || rcon.Status() != "unavailable" {
		t.Fatalf("fails = %d, status %s", rcon.Fails.Load(), rcon.Status())
	}
	if l, _ := f.Counts(); l != logins {
		t.Fatalf("%d login(s) between heartbeats, want 0", l-logins)
	}
	want("three missed polls", 2, 6, 1)
	w.check(t0.Add(heartbeatEvery + 4*pollEvery))
	want("a round while down", 2, 6, 1) // no poll, no heartbeat yet

	f.SetSilent(false)
	w.check(t0.Add(2 * heartbeatEvery)) // the heartbeat finds it back
	if rcon.Status() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", rcon.Status())
	}
	want("back", 3, 6, 1)
	w.check(t0.Add(2*heartbeatEvery + pollEvery))
	want("polling again", 3, 7, 1)
}

// TestNextCheckFollowsRounds checks the retry countdown the status bar shows:
// the next round while the server answers or has missed one or two checks,
// and the next heartbeat once it is down.
func TestNextCheckFollowsRounds(t *testing.T) {
	oldNext, oldFails := rcon.NextPoll.Load(), rcon.Fails.Load()
	t.Cleanup(func() {
		rcon.NextPoll.Store(oldNext)
		rcon.Fails.Store(oldFails)
	})
	now := time.Now()
	rcon.Fails.Store(1)
	w := serverWatch{lastHeartbeat: now.Add(-10 * time.Second)}
	w.storeNextCheck(now)
	if d := rcon.NextCheck().Sub(now); d < 4900*time.Millisecond || d > 5100*time.Millisecond {
		t.Fatalf("next check in %v, want the next round (5s)", d)
	}
	// Down: only the heartbeat tries, 30s after the last one (20s from now).
	rcon.Fails.Store(rcon.DownAfterMisses)
	w.storeNextCheck(now)
	if d := rcon.NextCheck().Sub(now); d < 19900*time.Millisecond || d > 20100*time.Millisecond {
		t.Fatalf("next check in %v, want the next heartbeat (20s)", d)
	}
}
