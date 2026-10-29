package game

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

func TestEventHubKeepsNewest(t *testing.T) {
	h := &eventHub{subs: map[chan battleye.ServerEvent]struct{}{}}
	total := 3*consoleLogLimit + 7
	for i := 0; i < total; i++ {
		h.Publish(battleye.ServerEvent{Kind: battleye.EventOther})
	}
	ev := h.recentEvents()
	if len(ev) != consoleLogLimit {
		t.Fatalf("kept %d events, want %d", len(ev), consoleLogLimit)
	}
	if last := ev[len(ev)-1].Seq; last != int64(total) {
		t.Fatalf("newest Seq = %d", last)
	}
	if first := ev[0].Seq; first != int64(total-consoleLogLimit+1) {
		t.Fatalf("oldest Seq = %d", first)
	}
	// after() finds its place by bisection; a limit keeps the newest.
	if got := h.after(int64(total - 3)); len(got) != 3 || got[0].Seq != int64(total-2) {
		t.Fatalf("after(total-3) = %d events starting at %d", len(got), got[0].Seq)
	}
	got, last := h.AfterWithLast(0, 10)
	if len(got) != 10 || got[9].Seq != int64(total) || last != int64(total) {
		t.Fatalf("afterWithLast(0, 10) = %d events, last %d", len(got), last)
	}
}

// resetWeb starts a test with no web session and no wait before the
// next login, and closes it and restores the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := Target()
	CloseSession()
	t.Cleanup(func() {
		SetTarget(oldAddr, oldPass)
		CloseSession()
	})
}

func TestReconnectWebOnlySwitchesOnSuccess(t *testing.T) {
	resetWeb(t)
	a, b := test.NewServer(t), test.NewServer(t)
	a.SetPassword("apw")
	b.SetPassword("bpw")
	addrA, addrB := a.Addr(), b.Addr()
	SetTarget(addrA, "apw")
	if err := Login(); err != nil {
		t.Fatal(err)
	}
	old, err := liveWebSession()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong password for B: nothing changes.
	if err := Reconnect(addrB, "nope"); err == nil {
		t.Fatal("a rejected login was accepted")
	}
	if addr, _ := Target(); addr != addrA {
		t.Fatalf("target moved to %s after a failed connect", addr)
	}
	if c, _ := liveWebSession(); c != old || old.IsClosed() {
		t.Fatal("the working session was replaced or closed after a failed connect")
	}

	// Right password: B becomes the session and the target; A's session closes.
	if err := Reconnect(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if addr, pass := Target(); addr != addrB || pass != "bpw" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if c, _ := liveWebSession(); c == old {
		t.Fatal("still on the old session")
	}
	time.Sleep(50 * time.Millisecond)
	if !old.IsClosed() {
		t.Fatal("the old session was left open")
	}
	if _, unlocks := b.Counts(); unlocks != 1 {
		t.Fatalf("B got %d #unlock(s), want 1", unlocks)
	}

	// Connecting again to the same place does a fresh handshake.
	before, _ := b.Counts()
	if err := Reconnect(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.Counts(); after != before+1 {
		t.Fatalf("reconnecting sent %d login(s), want 1", after-before)
	}
}

func TestNoPasswordDoesNotDial(t *testing.T) {
	if _, err := battleye.Dial("127.0.0.1:1", "", connectOptions(webSource, false)); err != battleye.ErrNoPassword {
		t.Fatalf("err = %v, want errNoPassword", err)
	}
}

// TestConnectTriesOnce checks that RCON Settings' Connect sends one login per
// press, even to a server that doesn't answer, and changes nothing.
func TestConnectTriesOnce(t *testing.T) {
	resetWeb(t)
	oldWait := battleye.ReloginReplyWait
	t.Cleanup(func() { battleye.ReloginReplyWait = oldWait })
	battleye.ReloginReplyWait = 50 * time.Millisecond
	f := test.NewServer(t)
	f.SetSilent(true)
	SetTarget("127.0.0.1:1", "pw")
	for press := 1; press <= 3; press++ {
		if err := Reconnect(f.Addr(), "pw"); err == nil {
			t.Fatal("connected to a server that doesn't answer")
		}
		time.Sleep(20 * time.Millisecond)
		if logins, _ := f.Counts(); logins != press {
			t.Fatalf("after %d press(es): %d login packets, want %d", press, logins, press)
		}
	}
	if addr, _ := Target(); addr != "127.0.0.1:1" {
		t.Fatalf("target changed to %s", addr)
	}
}

// TestSentCommandsGoInConsoleLog checks the master console log: a command
// sent for someone goes in once with who sent it, followed by an error when
// the server doesn't answer; polls and heartbeats don't go in at all; a new
// login logs its #unlock and asks for a players and bans poll.
func TestSentCommandsGoInConsoleLog(t *testing.T) {
	resetWeb(t)
	f := test.NewServer(t)
	SetTarget(f.Addr(), "pw")
	loginPollDue.Store(false)
	t.Cleanup(func() { loginPollDue.Store(false) })

	start := Events.LastSeq()
	if err := Login(); err != nil {
		t.Fatal(err)
	}
	if !loginPollDue.Load() {
		t.Fatal("a new login didn't ask for a poll")
	}
	Run("players", time.Second, false, "")
	Run("say -1 hello", time.Second, false, FlowSource("Greeter"))
	f.SetSilent(true)
	Run("kick 3 bye", 300*time.Millisecond, false, "web")

	var got []string
	for _, ev := range Events.after(start) {
		got = append(got, ev.Kind+"|"+ev.Text+"|"+ev.Source)
	}
	want := []string{
		"sent|> #unlock|login",
		`sent|> say -1 hello|flow "Greeter"`,
		"sent|> kick 3 bye|web",
		"error|(no acknowledgement from server)|web",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("console log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// testDB opens a fresh database and resets the player tracker.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	SetDatabase(db)
	ResetTracker()
	return db
}

// TestOneLoginWhileStarting checks the login against a server that is
// starting up: it takes packets but answers none until BattlEye is up, then
// answers the logins it got. ReCON sends ONE login and waits for it (every
// queued login becomes another "RCon admin logged in"), commands never log
// in, and the late answer makes it connected straight away, with one #unlock.
func TestOneLoginWhileStarting(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := test.NewServer(t)
	SetTarget(f.Addr(), "pw")
	oldBeat, oldPoll, oldReply, oldResend := heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, LoginResendAfter
	t.Cleanup(func() {
		heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, LoginResendAfter = oldBeat, oldPoll, oldReply, oldResend
		Fails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait = 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	Fails.Store(0)
	select {
	case <-loginAnswered: // from an earlier test
	default:
	}

	// Nothing is logged in yet: commands fail without logging in.
	f.Hold()
	for i := 0; i < 5; i++ {
		if _, _, err := Run("players", 10*time.Millisecond, false, ""); !errors.Is(err, ErrNotLoggedIn) {
			t.Fatalf("err = %v, want ErrNotLoggedIn", err)
		}
	}
	if l, _ := f.Counts(); l != 0 {
		t.Fatalf("commands sent %d login(s)", l)
	}

	// Two minutes of rounds: one login, then only empty commands that check
	// the port is still open.
	var w serverWatch
	t0 := time.Now()
	for i := 0; i <= 24; i++ {
		w.check(t0.Add(time.Duration(i) * pollEvery))
	}
	if l, _ := f.Counts(); l != 1 {
		t.Fatalf("%d login packets in two minutes of rounds; want 1", l)
	}
	if Status() != "unavailable" || !WaitingForLogin() {
		t.Fatalf("status %s, waiting for the login %v", Status(), WaitingForLogin())
	}
	if _, _, err := Run("players", 10*time.Millisecond, false, ""); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v while the login waits, want ErrNotLoggedIn", err)
	}

	// BattlEye is up and answers the login: connected at once, one #unlock.
	f.Release()
	select {
	case <-loginAnswered:
	case <-time.After(time.Second):
		t.Fatal("the late answer to the login didn't wake the server watch")
	}
	w.loginLanded(time.Now())
	if Status() != "connected" || WaitingForLogin() {
		t.Fatalf("status %s, waiting for the login %v, after the login was answered", Status(), WaitingForLogin())
	}
	if l, u := f.Counts(); l != 1 || u != 1 {
		t.Fatalf("%d login(s), %d #unlock(s); want 1 and 1", l, u)
	}
	w.check(t0.Add(25 * pollEvery))
	if l, u := f.Counts(); l != 1 || u != 1 || Status() != "connected" {
		t.Fatalf("a round later: %d login(s), %d #unlock(s), status %s", l, u, Status())
	}
}

// TestLoginLostOnlyWhenRefused checks that a login is sent again when the
// first can't be answered: the server refused it (its port was closed), or
// no answer came within LoginResendAfter (the packet was lost), in which
// case it goes on the same socket.
func TestLoginLostOnlyWhenRefused(t *testing.T) {
	testDB(t)
	resetWeb(t)
	oldBeat, oldPoll, oldReply, oldResend := heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, LoginResendAfter
	t.Cleanup(func() {
		heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, LoginResendAfter = oldBeat, oldPoll, oldReply, oldResend
		Fails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait = 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond

	// Nothing listens on the port: each heartbeat's login is refused.
	f := test.NewServer(t)
	closed := test.NewServer(t)
	addr := closed.Addr()
	closed.Close()
	SetTarget(addr, "pw")
	if err := Login(); err == nil || errors.Is(err, battleye.ErrLoginPending) || WaitingForLogin() {
		t.Fatalf("login to a closed port: %v, waiting %v", err, WaitingForLogin())
	}

	// A login the server holds: no other until LoginResendAfter.
	SetTarget(f.Addr(), "pw")
	f.Hold()
	for i := 0; i < 3; i++ {
		if err := Login(); !errors.Is(err, battleye.ErrLoginPending) {
			t.Fatalf("err = %v, want ErrLoginPending", err)
		}
	}
	if l, _ := f.Counts(); l != 1 {
		t.Fatalf("%d login(s) while one waits, want 1", l)
	}
	before, _ := liveOrPending()
	LoginResendAfter = 0
	Login()
	if l, _ := f.Counts(); l != 2 {
		t.Fatalf("%d login(s) after LoginResendAfter, want 2", l)
	}
	if after, _ := liveOrPending(); after != before {
		t.Fatal("the login was sent again on a new socket")
	}
}

// liveOrPending is the web session, logged in or not.
func liveOrPending() (*battleye.Session, bool) {
	webConnection.Lock()
	defer webConnection.Unlock()
	return webConnection.connection, webConnection.connection != nil
}

// TestOneLoginWhileSettling checks a server that accepts logins but doesn't
// answer commands yet: one login and one #unlock, no new login while it
// settles (ReloginSettle), then one on the same socket.
func TestOneLoginWhileSettling(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := test.NewServer(t)
	SetTarget(f.Addr(), "pw")
	oldBeat, oldPoll, oldReply, oldSettle := heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, ReloginSettle
	t.Cleanup(func() {
		heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait, ReloginSettle = oldBeat, oldPoll, oldReply, oldSettle
		Fails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait = 20*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	Fails.Store(0)

	f.SetMute(true)
	var w serverWatch
	t0 := time.Now()
	for i := 0; i <= 6; i++ {
		w.check(t0.Add(time.Duration(i) * heartbeatEvery / 2))
	}
	if l, u := f.Counts(); l != 1 || u != 1 {
		t.Fatalf("while settling: %d login(s), %d #unlock(s); want 1 and 1", l, u)
	}
	// Once it has settled, a heartbeat logs in again on the same socket.
	ReloginSettle = 0
	w.check(t0.Add(4 * heartbeatEvery))
	if l, u := f.Counts(); l != 2 || u != 2 {
		t.Fatalf("after settling: %d login(s), %d #unlock(s); want 2 and 2", l, u)
	}
	// It answers: connected, and no more logins.
	f.SetMute(false)
	w.check(t0.Add(5 * heartbeatEvery))
	if Status() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", Status())
	}
	if l, _ := f.Counts(); l != 2 {
		t.Fatalf("%d login(s) once answering, want 2", l)
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
	SetTarget(f.Addr(), "pw")
	oldBeat, oldPoll, oldReply := heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait
	t.Cleanup(func() {
		heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait = oldBeat, oldPoll, oldReply
		Fails.Store(0)
		loginPollDue.Store(false)
	})
	heartbeatWait, PlayersPollWait, battleye.ReloginReplyWait = 100*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	Fails.Store(0)

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
	for i := 1; i <= DownAfterMisses; i++ {
		w.check(t0.Add(heartbeatEvery + time.Duration(i)*pollEvery))
	}
	if Fails.Load() != DownAfterMisses || Status() != "unavailable" {
		t.Fatalf("fails = %d, status %s", Fails.Load(), Status())
	}
	if l, _ := f.Counts(); l != logins {
		t.Fatalf("%d login(s) between heartbeats, want 0", l-logins)
	}
	want("three missed polls", 2, 6, 1)
	w.check(t0.Add(heartbeatEvery + 4*pollEvery))
	want("a round while down", 2, 6, 1) // no poll, no heartbeat yet

	f.SetSilent(false)
	w.check(t0.Add(2 * heartbeatEvery)) // the heartbeat finds it back
	if Status() != "connected" {
		t.Fatalf("status %s after an answered heartbeat", Status())
	}
	want("back", 3, 6, 1)
	w.check(t0.Add(2*heartbeatEvery + pollEvery))
	want("polling again", 3, 7, 1)
}

// TestNextCheckFollowsRounds checks the retry countdown the status bar shows:
// the next round while the server answers or has missed one or two checks,
// and the next heartbeat once it is down.
func TestNextCheckFollowsRounds(t *testing.T) {
	oldNext, oldFails := NextPoll.Load(), Fails.Load()
	t.Cleanup(func() {
		NextPoll.Store(oldNext)
		Fails.Store(oldFails)
	})
	now := time.Now()
	Fails.Store(1)
	w := serverWatch{lastHeartbeat: now.Add(-10 * time.Second)}
	w.storeNextCheck(now)
	if d := NextCheck().Sub(now); d < 4900*time.Millisecond || d > 5100*time.Millisecond {
		t.Fatalf("next check in %v, want the next round (5s)", d)
	}
	// Down: only the heartbeat tries, 30s after the last one (20s from now).
	Fails.Store(DownAfterMisses)
	w.storeNextCheck(now)
	if d := NextCheck().Sub(now); d < 19900*time.Millisecond || d > 20100*time.Millisecond {
		t.Fatalf("next check in %v, want the next heartbeat (20s)", d)
	}
}

// line feeds one server message to the tracker as if it arrived at `at`.
// line feeds one server message to the tracker as if it arrived at `at`.
func line(text string, at time.Time) { Track(battleye.ParseServerMessage(text, at)) }

func TestTrackerSessionFromServerMessages(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	line("Player #3 Survivor (1.2.3.4:2304) connected", t0)
	if test.CountRows(t, db, "active_sessions") != 0 {
		t.Fatal("session opened before the GUID was known")
	}
	line("Player #3 Survivor - BE GUID: "+test.GUID1, t0.Add(time.Second))
	if test.CountRows(t, db, "active_sessions") != 1 {
		t.Fatal("session not opened on the GUID line")
	}
	line("Verified GUID ("+test.GUID1+") of player #3 Survivor", t0.Add(2*time.Second)) // no second session

	lobby := []battleye.Player{{ID: 3, IP: "1.2.3.4", GUID: test.GUID1, Name: "Survivor (Lobby)"}}
	inGame := []battleye.Player{{ID: 3, IP: "1.2.3.4", GUID: test.GUID1, Name: "Survivor"}}
	ApplyPoll(lobby, t0.Add(5*time.Second))
	ApplyPoll(inGame, t0.Add(30*time.Second))
	var loaded sql.NullTime
	if err := db.QueryRow("SELECT loaded_at FROM active_sessions WHERE guid = ?", test.GUID1).Scan(&loaded); err != nil || !loaded.Valid {
		t.Fatalf("loaded_at not set: %v %v", loaded, err)
	}
	seen := test.LastSeen(t, db, test.GUID1)
	ApplyPoll(inGame, t0.Add(35*time.Second)) // nothing changed: nothing written
	if !test.LastSeen(t, db, test.GUID1).Equal(seen) {
		t.Fatal("a poll with nothing new wrote to the database")
	}

	left := t0.Add(10 * time.Minute)
	line("Player #3 Survivor disconnected", left)
	h := test.History(t, db, test.GUID1)
	if len(h) != 1 || h[0].Ended != store.EndDisconnected || h[0].Duration != 600 || !h[0].Loaded || !h[0].Connected.Equal(t0) {
		t.Fatalf("history = %+v", h)
	}
	if !test.LastSeen(t, db, test.GUID1).Equal(left) {
		t.Fatalf("last_seen = %v, want the disconnect line's time %v", test.LastSeen(t, db, test.GUID1), left)
	}
	// A poll sent before the disconnect line arrived must not reopen them.
	ApplyPoll(inGame, left.Add(-2*time.Second))
	if test.CountRows(t, db, "active_sessions") != 0 {
		t.Fatal("a stale poll reopened the session")
	}
}

func TestTrackerKickAndBan(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #1 A (1.1.1.1:2304) connected", t0)
	line("Player #1 A - BE GUID: "+test.GUID1, t0)
	line("Player #1 A ("+test.GUID1+") has been kicked by BattlEye: Admin Kick (bye)", t0.Add(time.Minute))
	line("Player #1 A disconnected", t0.Add(time.Minute))
	if h := test.History(t, db, test.GUID1); len(h) != 1 || h[0].Ended != store.EndKicked {
		t.Fatalf("kick history = %+v", h)
	}

	line("Player #2 B (2.2.2.2:2304) connected", t0)
	line("Player #2 B - BE GUID: "+test.GUID2, t0)
	if err := db.RecordPlayerBan(test.GUID2, "B", "2.2.2.2", "cheating", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	line("Player #2 B ("+test.GUID2+") has been kicked by BattlEye: Admin Ban (cheating)", t0.Add(time.Minute))
	line("Player #2 B disconnected", t0.Add(time.Minute))
	if h := test.History(t, db, test.GUID2); len(h) != 1 || h[0].Ended != store.EndBanned {
		t.Fatalf("ban history = %+v", h)
	}
}

func TestTrackerMissedLines(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	on := []battleye.Player{{ID: 4, IP: "4.4.4.4", GUID: test.GUID1, Name: "Quiet"}}
	// Already on when ReCON started: the first poll opens the session.
	ApplyPoll(on, t0)
	if test.CountRows(t, db, "active_sessions") != 1 {
		t.Fatal("poll didn't open a session for an untracked player")
	}
	// Gone without a disconnect line: the next poll closes them, at its time.
	ApplyPoll(nil, t0.Add(10*time.Second))
	h := test.History(t, db, test.GUID1)
	if len(h) != 1 || !h[0].At.Equal(t0.Add(10*time.Second)) || h[0].Ended != store.EndDisconnected {
		t.Fatalf("history = %+v", h)
	}

	// Server stops answering: every open session closes when it last answered.
	ApplyPoll(on, t0.Add(time.Minute))
	ServerWentDown(t0.Add(time.Minute + 5*time.Second))
	h = test.History(t, db, test.GUID1)
	if test.CountRows(t, db, "active_sessions") != 0 || len(h) != 2 || !h[1].At.Equal(t0.Add(time.Minute+5*time.Second)) {
		t.Fatalf("serverWentDown didn't close the session: %+v", h)
	}
}

func TestLastChatFromServerMessages(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Survivor (1.2.3.4:2304) connected", t0)
	line("Player #0 Survivor - BE GUID: "+test.GUID1, t0)
	line("Player #1 Quiet (5.6.7.8:2304) connected", t0)
	line("Player #1 Quiet - BE GUID: "+test.GUID2, t0)

	line("(Global) Survivor: first", t0.Add(time.Minute))
	line("(Side) Survivor: anyone at NWAF?", t0.Add(2*time.Minute))
	// ReCON's replies are commands it sends, not lines from the server, and
	// are only written to the chat history.
	if err := db.AddChatMessage(t0.Add(3*time.Minute), test.GUID1, store.ChatOut, "> say 0 hello from the admins"); err != nil {
		t.Fatal(err)
	}
	last := LastChatPerPlayer()
	if c := last[0]; c.GUID != test.GUID1 || c.Chat.Text != "anyone at NWAF?" || c.Chat.Channel != "Side" || !c.Chat.At.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("Survivor's last chat = %+v", c)
	}
	if _, ok := last[1]; ok {
		t.Fatal("a player who hasn't chatted has a last message")
	}

	// Two players with the same name: the line can't be placed, so neither gets it.
	line("Player #2 Quiet (9.9.9.9:2304) connected", t0)
	line("(Global) Quiet: who said that?", t0.Add(4*time.Minute))
	if _, ok := LastChatPerPlayer()[1]; ok {
		t.Fatal("an ambiguous chat line was given to a player")
	}

	// It goes when they leave: a new session starts empty.
	line("Player #0 Survivor disconnected", t0.Add(5*time.Minute))
	line("Player #0 Survivor (1.2.3.4:2304) connected", t0.Add(6*time.Minute))
	line("Player #0 Survivor - BE GUID: "+test.GUID1, t0.Add(6*time.Minute))
	if _, ok := LastChatPerPlayer()[0]; ok {
		t.Fatal("the last session's message is still shown")
	}
}

// TestRestartKeepsReturningPlayersSession checks the restart grace: a player
// back within restartGrace of the next login keeps one session with the
// downtime not counted; one who isn't is left closed when the server went down.
func TestRestartKeepsReturningPlayersSession(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Back (1.1.1.1:2304) connected", t0)
	line("Player #0 Back - BE GUID: "+test.GUID1, t0)
	line("Player #1 Gone (2.2.2.2:2304) connected", t0)
	line("Player #1 Gone - BE GUID: "+test.GUID2, t0)

	down := t0.Add(10 * time.Minute)
	ServerWentDown(down)
	if test.CountRows(t, db, "active_sessions") != 0 {
		t.Fatal("sessions still open while the server is down")
	}
	login := down.Add(5 * time.Minute)
	serverLoggedIn(login)
	back := login.Add(30 * time.Second)
	line("Player #4 Back (1.1.1.1:2304) connected", back)
	line("Player #4 Back - BE GUID: "+test.GUID1, back)
	if h := test.History(t, db, test.GUID1); len(h) != 0 || test.CountRows(t, db, "active_sessions") != 1 {
		t.Fatalf("not resumed: history %+v", h)
	}
	ForgetAway(login.Add(restartGrace + time.Second))
	line("Player #5 Gone (2.2.2.2:2304) connected", login.Add(2*time.Minute))
	line("Player #5 Gone - BE GUID: "+test.GUID2, login.Add(2*time.Minute))
	if h := test.History(t, db, test.GUID2); len(h) != 1 || !h[0].At.Equal(down) || h[0].Duration != 600 {
		t.Fatalf("late player's old session = %+v, want closed at the outage", h)
	}

	line("Player #4 Back disconnected", back.Add(20*time.Minute))
	h := test.History(t, db, test.GUID1)
	// 10 minutes before the outage and 20 after: the 5.5 minutes down aren't counted.
	if len(h) != 1 || h[0].Duration != 30*60 || !h[0].Connected.Equal(t0) {
		t.Fatalf("resumed session = %+v, want one 30 minute session from t0", h)
	}
	var playtime int
	db.QueryRow("SELECT total_playtime_seconds FROM players WHERE guid = ?", test.GUID1).Scan(&playtime)
	if playtime != 30*60 {
		t.Fatalf("playtime = %d, want 1800", playtime)
	}
}

// TestRestartNewIPIsNewSession checks that a player back within
// restartGrace but from another IP gets a new session on that IP, and the
// old one stays closed at the outage.
func TestRestartNewIPIsNewSession(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	line("Player #0 Mover (1.1.1.1:2304) connected", t0)
	line("Player #0 Mover - BE GUID: "+test.GUID1, t0)
	down := t0.Add(10 * time.Minute)
	ServerWentDown(down)
	login := down.Add(time.Minute)
	serverLoggedIn(login)
	back := login.Add(time.Minute)
	line("Player #2 Mover (9.9.9.9:2304) connected", back)
	line("Player #2 Mover - BE GUID: "+test.GUID1, back)
	if h := test.History(t, db, test.GUID1); len(h) != 1 || !h[0].At.Equal(down) || h[0].Duration != 600 {
		t.Fatalf("old session = %+v, want closed at the outage", h)
	}
	var ip string
	if err := db.QueryRow("SELECT ip FROM active_sessions WHERE guid = ?", test.GUID1).Scan(&ip); err != nil || ip != "9.9.9.9" {
		t.Fatalf("new session ip = %q (%v), want 9.9.9.9", ip, err)
	}
}

func TestLobbyWatching(t *testing.T) {
	testDB(t)
	now := time.Now()
	if anyoneInLobby() {
		t.Fatal("someone loading with nobody online")
	}
	line("Player #2 New (3.3.3.3:2304) connected", now)
	if !anyoneInLobby() {
		t.Fatal("a player who just connected isn't loading")
	}
	ApplyPoll([]battleye.Player{{ID: 2, IP: "3.3.3.3", Name: "New"}}, now.Add(time.Second))
	if anyoneInLobby() {
		t.Fatal("still loading after (Lobby) left the name")
	}
	if p, known := OnlinePlayers(); !known || len(p) != 1 || p[0].Name != "New" {
		t.Fatalf("listOnlinePlayers = %+v %v", p, known)
	}
}

// TestJoinFlowsWaitForLoading checks that a player seen connecting is handed
// to the join flows once "(Lobby)" leaves their name, and only once; a
// player who was already on when ReCON started is not.
func TestJoinFlowsWaitForLoading(t *testing.T) {
	testDB(t)
	t0 := time.Now()
	ApplyPoll([]battleye.Player{{ID: 1, IP: "1.1.1.1", GUID: test.GUID2, Name: "Already"}}, t0)
	line("Player #3 New (1.2.3.4:2304) connected", t0.Add(time.Second))
	line("Player #3 New - BE GUID: "+test.GUID1, t0.Add(time.Second))
	both := func(lobby bool) []battleye.Player {
		name := "New"
		if lobby {
			name += " (Lobby)"
		}
		return []battleye.Player{{ID: 1, IP: "1.1.1.1", GUID: test.GUID2, Name: "Already"}, {ID: 3, IP: "1.2.3.4", GUID: test.GUID1, Name: name}}
	}
	if j := ApplyPoll(both(true), t0.Add(5*time.Second)); len(j) != 0 {
		t.Fatalf("joined while loading: %+v", j)
	}
	j := ApplyPoll(both(false), t0.Add(10*time.Second))
	if len(j) != 1 || j[0].ID != 3 || j[0].GUID != test.GUID1 {
		t.Fatalf("joined = %+v, want player 3", j)
	}
	if j := ApplyPoll(both(false), t0.Add(15*time.Second)); len(j) != 0 {
		t.Fatalf("joined twice: %+v", j)
	}
}

// TestShutdownKeepsSession replays a #shutdown: BattlEye disconnects the
// player just before the server stops answering, they rejoin after the
// restart, and they keep one session with the minute away not counted. A
// player banned just before, and one who left long before, start over.
func TestShutdownKeepsSession(t *testing.T) {
	db := testDB(t)
	const bannedGUID = "0000000000000000000000000000bad1"
	t0 := time.Date(2026, 10, 26, 21, 45, 31, 0, time.UTC)
	line("Player #0 MowMdown (10.0.1.51:56583) connected", t0)
	line("Player #0 MowMdown - BE GUID: "+test.GUID1, t0)
	line("Player #1 Early (2.2.2.2:2304) connected", t0)
	line("Player #1 Early - BE GUID: "+test.GUID2, t0)
	line("Player #2 Cheater (3.3.3.3:2304) connected", t0)
	line("Player #2 Cheater - BE GUID: "+bannedGUID, t0)

	line("Player #1 Early disconnected", t0.Add(10*time.Minute)) // long before the restart
	shutdown := time.Date(2026, 10, 26, 22, 3, 3, 0, time.UTC)
	if err := db.RecordPlayerBan(bannedGUID, "Cheater", "3.3.3.3", "cheating", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	line("Player #2 Cheater (-) has been kicked by BattlEye: Admin Ban (cheating)", shutdown.Add(-30*time.Second))
	line("Player #2 Cheater disconnected", shutdown.Add(-30*time.Second))
	line("Player #0 MowMdown disconnected", shutdown.Add(2*time.Second))
	ServerWentDown(shutdown) // the last answer, before BattlEye's disconnect line
	login := time.Date(2026, 10, 26, 22, 4, 29, 0, time.UTC)
	serverLoggedIn(login)
	back := time.Date(2026, 10, 26, 22, 4, 33, 0, time.UTC)
	line("Player #0 MowMdown (10.0.1.51:44046) connected", back)
	line("Player #0 MowMdown - BE GUID: "+test.GUID1, back)
	line("Player #4 Early (2.2.2.2:2304) connected", back)
	line("Player #4 Early - BE GUID: "+test.GUID2, back)
	line("Player #5 Cheater (3.3.3.3:2304) connected", back)
	line("Player #5 Cheater - BE GUID: "+bannedGUID, back)

	if h := test.History(t, db, test.GUID1); len(h) != 0 {
		t.Fatalf("MowMdown's session wasn't carried over: history %+v", h)
	}
	if h := test.History(t, db, test.GUID2); len(h) != 1 {
		t.Fatalf("a player who left long before the restart was carried over: %+v", h)
	}
	if h := test.History(t, db, bannedGUID); len(h) != 1 {
		t.Fatalf("a banned player was carried over: %+v", h)
	}
	line("Player #0 MowMdown disconnected", time.Date(2026, 10, 26, 22, 5, 51, 0, time.UTC))
	h := test.History(t, db, test.GUID1)
	// 21:45:31 to 22:05:51, less the 88s away (22:03:05 to 22:04:33).
	if len(h) != 1 || !h[0].Connected.Equal(t0) || h[0].Duration != 20*60+20-88 {
		t.Fatalf("session = %+v, want one from 21:45:31 lasting %ds", h, 20*60+20-88)
	}
}

// TestKickAllRestartKeepsSessions: a restart flow's Kick everyone kicks
// each player (BattlEye's kick line, then the disconnect) shortly before
// #shutdown; players back after the restart keep their sessions.
func TestKickAllRestartKeepsSessions(t *testing.T) {
	db := testDB(t)
	t0 := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	list := []struct{ id, name, guid, ip string }{{"0", "Alpha", test.GUID1, "1.1.1.1"}, {"1", "Bravo", test.GUID2, "2.2.2.2"}}
	for _, p := range list {
		line("Player #"+p.id+" "+p.name+" ("+p.ip+":2304) connected", t0)
		line("Player #"+p.id+" "+p.name+" - BE GUID: "+p.guid, t0)
	}
	kickall := t0.Add(time.Hour)
	for _, p := range list {
		line("Player #"+p.id+" "+p.name+" ("+p.guid+") has been kicked by BattlEye: Admin Kick (Server restarting)", kickall)
		line("Player #"+p.id+" "+p.name+" disconnected", kickall)
	}
	down := kickall.Add(30 * time.Second) // Wait 30s, then #shutdown
	ServerWentDown(down)
	serverLoggedIn(down.Add(90 * time.Second))
	back := down.Add(2 * time.Minute)
	for i, p := range list {
		id := strconv.Itoa(i + 5)
		line("Player #"+id+" "+p.name+" ("+p.ip+":2304) connected", back)
		line("Player #"+id+" "+p.name+" - BE GUID: "+p.guid, back)
	}
	for _, p := range list {
		if h := test.History(t, db, p.guid); len(h) != 0 || test.CountRows(t, db, "active_sessions") != 2 {
			t.Fatalf("%s wasn't carried over after Kick everyone: history %+v", p.name, h)
		}
	}
}

const timedGUID = "cccccccccccccccccccccccccccccccc"

// banTestDB opens a fresh database for this package with one permanent ban
// (test.GUID1) and one timed ban (timedGUID), and no RCON server to talk to.
func banTestDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	SetDatabase(db)
	oldAddr, oldPass := Target()
	CloseSession()
	t.Cleanup(func() { SetTarget(oldAddr, oldPass); CloseSession() })
	SetTarget("127.0.0.1:1", "")
	if err := db.RecordPlayerBan(test.GUID1, "Cheater", "1.1.1.1", "aimbot", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPlayerBan(timedGUID, "Spammer", "2.2.2.2", "spam", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestImportPermanentBans checks Import's rules: a GUID already banned
// permanently and a malformed one are skipped with a reason, a timed ban
// becomes permanent, missing names and reasons get defaults, and with the
// server not answering the bans wait for the next login.
func TestImportPermanentBans(t *testing.T) {
	db := banTestDB(t)
	res := ImportBans(BanFile{Bans: []store.PermanentBan{
		{GUID: test.GUID1, Name: "Cheater", Reason: "aimbot"},
		{GUID: strings.ToUpper(test.GUID2), Name: "", Reason: ""},
		{GUID: "not-a-guid", Name: "Broken"},
		{GUID: timedGUID, Name: "Spammer", Reason: "spam again", SteamID: "76561198000000001"},
	}})
	if res.Added != 2 || res.OnServer || res.Kicked != 0 {
		t.Fatalf("result = %+v, want 2 added, not on the server yet", res)
	}
	want := []string{"Cheater (01234567): already banned permanently", "Broken (not-a-gu): not a BattlEye GUID"}
	if strings.Join(res.Skipped, "|") != strings.Join(want, "|") {
		t.Fatalf("skipped = %q, want %q", res.Skipped, want)
	}
	if st, _ := db.GetBanState(test.GUID2); !st.Banned || !st.Perm || st.Reason != importReason {
		t.Fatalf("imported ban with no reason = %+v", st)
	}
	if st, _ := db.GetBanState(timedGUID); !st.Banned || !st.Perm || st.Reason != "spam again" {
		t.Fatalf("timed ban after import = %+v, want permanent", st)
	}
	f, err := ExportBans(nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, b := range f.Bans {
		names[b.GUID] = b.Name + "/" + b.SteamID
	}
	if len(f.Bans) != 3 || names[test.GUID2] != "Unknown/" || names[timedGUID] != "Spammer/76561198000000001" {
		t.Fatalf("export after import = %+v", f.Bans)
	}
	if !importedPending[test.GUID2] || !importedPending[timedGUID] {
		t.Fatalf("imported bans not waiting for the server: %v", importedPending)
	}
}

// TestExportOnlyPermanent checks that Export leaves timed bans out and,
// given GUIDs, keeps only those.
func TestExportOnlyPermanent(t *testing.T) {
	banTestDB(t)
	f, err := ExportBans(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Bans) != 1 || f.Bans[0].GUID != test.GUID1 || f.Bans[0].Reason != "aimbot" || f.Bans[0].IP != "1.1.1.1" {
		t.Fatalf("export = %+v, want only the permanent ban", f.Bans)
	}
	if f, _ := ExportBans(map[string]bool{test.GUID2: true}); len(f.Bans) != 0 {
		t.Fatalf("export of a GUID with no permanent ban = %+v", f.Bans)
	}
}
