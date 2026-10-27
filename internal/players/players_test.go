package players

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

// line feeds one server message to the tracker as if it arrived at `at`.
// line feeds one server message to the tracker as if it arrived at `at`.
func line(text string, at time.Time) { Track(battleye.ParseServerMessage(text, at)) }

// testDB opens a fresh database for this package and resets the tracker.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	SetDatabase(db)
	Reset()
	return db
}

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
	if p, known := Online(); !known || len(p) != 1 || p[0].Name != "New" {
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
