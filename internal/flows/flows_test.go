package flows

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/store/storetest"
)

// testDB opens a fresh database for this package and the ones flows use,
// and resets the player tracker.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := storetest.Open(t)
	SetDatabase(db)
	players.SetDatabase(db)
	bans.SetDatabase(db)
	players.ResetForTesting()
	return db
}

// setOnline puts players in the tracker as if the console log and a poll
// had told it about them (no database writes).
func setOnline(online ...battleye.Player) { players.SetOnlineForTesting(online...) }

func TestEvalCond(t *testing.T) {
	cases := []struct {
		subject, op, value, v string
		want                  bool
	}{
		{"player_role", "ge", "1", "1", true}, // Mod is at least Mod
		{"player_role", "ge", "1", "0", false},
		{"player_role", "le", "1", "2", false},
		{"player_role", "eq", "2", "2", true},
		{"player_role", "ne", "0", "1", true},
		{"player_id", "eq", "3", "03", true},
		{"player_id", "ne", "3", "4", true},
		{"player_guid", "eq", strings.ToUpper(storetest.GUID1), storetest.GUID1, true},
		{"player_guid", "ne", storetest.GUID1, storetest.GUID1, false},
		{"player_name", "eq", "survivor", "Survivor", true},
		{"player_name", "has", "ADMIN", "TheAdminGuy", true},
		{"player_name", "wild", "*admin*", "TheAdminGuy", true},
		{"player_ip", "ne", "1.2.3.4", "1.2.3.4", false},
		{"player_ping", "gt", "500", "501", true},
		{"players_online", "le", "0", "0", true},
		{"message", "has", "noob", "you noob!", true},
		{"message", "has", "noob", "noobish", false},
		{"message", "starts", "!discord", "!discord please", true},
		{"message", "exact", "!rules", "  !RULES ", true},
		{"message", "exact", "!rules", "!rules please", false},
	}
	for _, c := range cases {
		b := Branch{Subject: c.subject, Op: c.op, Value: c.value}
		if got := evaluateIfCondition(b, c.v); got != c.want {
			t.Errorf("{%s} %s %q against %q = %v, want %v", c.subject, c.op, c.value, c.v, got, c.want)
		}
	}
}

func TestRoleConditionUsesNumber(t *testing.T) {
	x := &job{p: &flowPlayer{role: roleMod}}
	v, _ := x.ifSubjectValue("player_role")
	if !evaluateIfCondition(Branch{Subject: "player_role", Op: "ge", Value: "1"}, v) {
		t.Fatalf("a Mod should be at least Mod (value %q)", v)
	}
	if name, _ := x.placeholderValue("player_role"); name != "Mod" {
		t.Fatalf("{player_role} placeholder = %q, want Mod", name)
	}
	if got := describeIfCondition(Branch{Subject: "player_role", Op: "ge", Value: "1"}); got != "{player_role} is at least Mod" {
		t.Fatalf("describeIfCondition = %q", got)
	}
}

func TestCheckIfOperators(t *testing.T) {
	ifStep := func(subject, op, value string) []Step {
		return []Step{{Type: "if", Branches: []Branch{{Subject: subject, Op: op, Value: value, Steps: []Step{{Type: "stop"}}}}}}
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_id", "gt", "3"), nil); err == nil {
		t.Error("{player_id} > 3 should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_guid", "eq", "nope"), nil); err == nil {
		t.Error("a malformed GUID value should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_role", "ge", "5"), nil); err == nil {
		t.Error("role 5 should be rejected")
	}
	if err := checkFlowSteps(triggerJoin, 0, ifStep("player_role", "le", "1"), nil); err != nil {
		t.Errorf("role at most Mod: %v", err)
	}
	if err := checkFlowSteps(triggerChat, 0, ifStep("message", "exact", "!rules"), nil); err != nil {
		t.Errorf("message is exactly: %v", err)
	}
}

// testFlow is a minimal valid manual flow.
func testFlow(name string) Flow {
	return Flow{Name: name, Trigger: triggerManual, Steps: []Step{{Type: "message", To: "all", Text: "hi"}}}
}

func flowIDs(t *testing.T) []int64 {
	t.Helper()
	flows, err := List()
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, f := range flows {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestFlowNumbersReuseFreedOnes(t *testing.T) {
	db := testDB(t)
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	add := func(name string) int64 {
		t.Helper()
		id, err := Save(testFlow(name), true)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, name := range []string{"a", "b", "c", "d"} {
		if id := add(name); id != int64(i) {
			t.Fatalf("flow %s got #%d, want #%d", name, id, i)
		}
	}
	for _, id := range flowIDs(t) {
		if err := Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	// Four deleted, four new: 0-3 again, not 4-7.
	for i, name := range []string{"e", "f", "g", "h"} {
		if id := add(name); id != int64(i) {
			t.Fatalf("after deleting all, flow %s got #%d, want #%d", name, id, i)
		}
	}
	// Deleting one in the middle leaves the others as they are; its number
	// goes to the next new flow, then numbering carries on after the last.
	if err := Delete(1); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(flowIDs(t)); got != "[0 2 3]" {
		t.Fatalf("after deleting #1: %s", got)
	}
	if id := add("i"); id != 1 {
		t.Fatalf("next flow got #%d, want #1", id)
	}
	if id := add("j"); id != 4 {
		t.Fatalf("next flow got #%d, want #4", id)
	}
	// Flow 0 is an ordinary flow: it can be edited.
	f, err := Get(0)
	if err != nil {
		t.Fatal(err)
	}
	f.Name = "renamed"
	if id, err := Save(f, false); err != nil || id != 0 {
		t.Fatalf("editing #0: id %d, %v", id, err)
	}
	if f, _ := Get(0); f.Name != "renamed" {
		t.Fatalf("flow #0 = %q", f.Name)
	}
}

// pingKickFlow is the example: for each player, if ping is over 500, kick them.
func pingKickFlow() []Step {
	return []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if", Branches: []Branch{{
		Subject: "player_ping", Op: "gt", Value: "500",
		Steps: []Step{{Type: "kick", Who: "player", Reason: "Ping too high ({player_ping})"}},
	}}}}}}
}

func TestForEachValidation(t *testing.T) {
	kick := []Step{{Type: "kick", Who: "player", Reason: "x"}}
	if err := checkFlowSteps(triggerSchedule, 0, kick, nil); err == nil {
		t.Error("Kick outside a For each player was accepted in a Schedule flow")
	}
	if err := checkFlowSteps(triggerSchedule, 0, pingKickFlow(), nil); err != nil {
		t.Errorf("the ping-kick example: %v", err)
	}
	if err := checkFlowSteps(triggerManual, 0, []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "hi {player_name}"}}}}, nil); err != nil {
		t.Errorf("a message to each player: %v", err)
	}
	nested := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "forEach", Of: "message", Steps: kick}}}}
	if err := checkFlowSteps(triggerSchedule, 0, nested, nil); err == nil || !strings.Contains(err.Error(), "inside another") {
		t.Errorf("a loop inside a loop: %v", err)
	}
	// {message} is the looped player's line, in any trigger.
	for _, of := range []string{"player", "message"} {
		words := []Step{{Type: "forEach", Of: of, Steps: []Step{{Type: "if", Branches: []Branch{{
			Subject: "message", Op: "has", Value: "noob", Steps: []Step{{Type: "message", To: "player", Text: "You said: {message}"}}}}}}}}
		if err := checkFlowSteps(triggerSchedule, 0, words, nil); err != nil {
			t.Errorf("For each %s with {message}: %v", of, err)
		}
	}
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "forEach", Of: "everything", Steps: kick}}, nil); err == nil {
		t.Error("For each of an unknown kind was accepted")
	}
	if got := normalizeFlowSteps([]Step{{Type: "forEach"}}); got[0].Of != "player" {
		t.Errorf("a For each without a kind reads as %q, want player", got[0].Of)
	}
	afterLoop := append(pingKickFlow(), kick...)
	if err := checkFlowSteps(triggerSchedule, 0, afterLoop, nil); err == nil {
		t.Error("a Kick after the loop was accepted")
	}
	// When stopped takes every kind of step, with the same rules as Steps.
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "lock"}}, append(pingKickFlow(), Step{Type: "wait", Time: "5s"}, Step{Type: "shutdown"})); err != nil {
		t.Errorf("When stopped with For each, If, Kick, Wait and Shut down: %v", err)
	}
	if err := checkFlowSteps(triggerSchedule, 0, []Step{{Type: "lock"}}, kick); err == nil {
		t.Error("a Kick with no player was accepted in When stopped")
	}
	if n := countAllSteps(pingKickFlow()); n != 3 {
		t.Errorf("countAllSteps = %d, want 3", n)
	}
}

func TestForEachKicksOnlyMatchingPlayers(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	setOnline(
		battleye.Player{ID: 4, GUID: storetest.GUID1, Name: "Laggy", Ping: 820},
		battleye.Player{ID: 1, GUID: storetest.GUID2, Name: "Fine", Ping: 40},
		battleye.Player{ID: 7, GUID: "", Name: "Loading (Lobby)", Ping: 999}, // not verified yet: skipped
		battleye.Player{ID: 2, GUID: "0000000000000000000000000000beef", Name: "Edge", Ping: 500},
		battleye.Player{ID: 9, GUID: "0000000000000000000000000000cafe", Name: "Awful", Ping: 1500},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &flowRun{name: "ping check", ctx: ctx, cancel: cancel}
	x := &job{run: run, ctx: ctx}
	if !x.execFlowSteps(pingKickFlow()) {
		t.Fatal("the run ended early")
	}
	want := []string{"kick 4 Ping too high (820)", "kick 9 Ping too high (1500)"}
	if fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	logText := strings.Join(run.log, "\n")
	if !strings.Contains(logText, "For each player: 4 online") || !strings.Contains(logText, "Laggy (#4): If {player_ping} > 500") {
		t.Fatalf("run log:\n%s", logText)
	}
	if strings.Contains(logText, "Fine") {
		t.Fatalf("a player who didn't match was logged:\n%s", logText)
	}

	// Stop flow inside the loop ends the whole run.
	sent = nil
	stopFirst := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "hi"}, {Type: "stop"}}}}
	if x.execFlowSteps(stopFirst) {
		t.Fatal("Stop flow inside the loop didn't end the run")
	}
	if len(sent) != 1 || sent[0] != "say 1 hi" {
		t.Fatalf("sent %q; the loop should stop after the first player (#1)", sent)
	}
}

func TestForEachConsidersOnlyChatSinceLastRun(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	players.TrackLineForTesting("Player #3 Survivor (1.2.3.4:2304) connected", t0)
	players.TrackLineForTesting("Player #3 Survivor - BE GUID: "+storetest.GUID1, t0)
	players.TrackLineForTesting("Player #5 Calm (5.6.7.8:2304) connected", t0)
	players.TrackLineForTesting("Player #5 Calm - BE GUID: "+storetest.GUID2, t0)
	// chat sends a line through the event hub, as the server does, so it
	// gets a real Seq, then to the tracker.
	chat := func(text string) {
		ev := battleye.ParseServerMessage(text, t0)
		rcon.Events.Publish(ev)
		ev.Seq = rcon.Events.LastSeq()
		players.TrackEventForTesting(ev)
	}
	const flowID = 42
	t.Cleanup(func() { forEachMarks.Lock(); delete(forEachMarks.byFlow, flowID); forEachMarks.Unlock() })
	// runFlow is one run of the flow: a fresh flowRun, as each launch makes.
	runFlow := func(steps []Step) {
		t.Helper()
		sent = nil
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		x := &job{run: &flowRun{flowID: flowID, name: "chat", ctx: ctx, cancel: cancel}, ctx: ctx}
		if !x.execFlowSteps(steps) {
			t.Fatal("the run ended early")
		}
	}
	warn := []Step{{Type: "forEach", Of: "message", Steps: []Step{{Type: "if", Branches: []Branch{{
		Subject: "message", Op: "has", Value: "noob",
		Steps: []Step{{Type: "message", To: "player", Text: "No insults: {message}"}}}}}}}}
	lastSaid := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "all", Text: "{player_name} said: {message}"}}}}

	chat("(Global) Survivor: hello all")
	chat("(Side) Calm: anyone noob here?")
	chat("(Global) Survivor: you noob")
	runFlow(warn)
	if want := []string{"say 5 No insults: anyone noob here?", "say 3 No insults: you noob"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("first run sent %q, want %q", sent, want)
	}
	runFlow(warn)
	if len(sent) != 0 {
		t.Fatalf("a run with no new lines sent %q", sent)
	}
	chat("(Global) Calm: noob")
	runFlow(warn)
	if want := []string{"say 5 No insults: noob"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("third run sent %q, want %q", sent, want)
	}

	// For each player: {message} is their latest line since the last run,
	// empty for a player who hasn't chatted since.
	chat("(Global) Survivor: brb")
	runFlow(lastSaid)
	if want := []string{"say -1 Survivor said: brb", "say -1 Calm said:"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("For each player sent %q, want %q", sent, want)
	}

	// Two For each in one run look at the same lines.
	chat("(Global) Calm: noob again")
	runFlow(append(append([]Step{}, lastSaid...), warn...))
	if want := []string{"say -1 Survivor said:", "say -1 Calm said: noob again", "say 5 No insults: noob again"}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("two loops in one run sent %q, want %q", sent, want)
	}

	// A player's lines go when they leave.
	players.TrackLineForTesting("Player #3 Survivor disconnected", t0.Add(time.Hour))
	if _, ok := players.LatestChatOf(storetest.GUID1); ok {
		t.Fatal("a player who left still has session lines")
	}
}

// TestSchedulerWakesForNextRun checks the clock-based scheduler: nothing
// starts before a run is due, it returns the run's first step (cron time
// plus delay) as its next wake-up, and starts the run once that has come.
func TestSchedulerWakesForNextRun(t *testing.T) {
	if err := testDB(t).EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	oldSend := sendCommand
	sendCommand = func(*flowRun, string) {}
	t.Cleanup(func() { sendCommand = oldSend; lastFired = map[int64]time.Time{} })

	f := Flow{Name: "hourly", Trigger: triggerSchedule, Schedule: "0 * * * *", Delay: "30s",
		Steps: []Step{{Type: "message", To: "all", Text: "hi"}}}
	id, err := Save(f, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetFlowScheduleEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Hour).Add(10 * time.Minute)
	due := now.Truncate(time.Hour).Add(time.Hour + 30*time.Second)
	if wake := runDueSchedules(now); !wake.Equal(due) {
		t.Fatalf("wake at %v, want %v (next hour plus the delay)", wake, due)
	}
	if wake := runDueSchedules(due); !wake.Equal(due.Add(time.Hour)) {
		t.Fatalf("after starting, wake at %v, want an hour later", wake)
	}
	// The run started, waiting for its first step at the (future) due time.
	flowRuns.Lock()
	run := flowRuns.byFlow[id]
	started := run != nil && run.active && run.anchor.Equal(due)
	flowRuns.Unlock()
	if !started {
		t.Fatal("the due run wasn't started")
	}
	Stop(id)
	if _, again := lastFired[id]; !again {
		t.Fatal("the cron time wasn't recorded")
	}
}

// TestWarningCommand checks !warning: a code or a player number finds the
// player, each warning is sent to them and the staff, the warning that
// reaches the threshold is followed by the kick or ban the settings name,
// and Players can't warn. It also checks !kick by player number.
func TestWarningCommand(t *testing.T) {
	db := testDB(t)
	const modGUID = "0000000000000000000000000000a11a"
	now := time.Now()
	for _, p := range []battleye.Player{{ID: 1, GUID: modGUID, Name: "Mod"}, {ID: 4, GUID: storetest.GUID1, Name: "Rowdy"}, {ID: 6, GUID: storetest.GUID2, Name: "Other"}} {
		if err := db.OpenPlayerSession(p.GUID, p.Name, "1.2.3.4", p.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SetPlayerRole(modGUID, roleMod); err != nil {
		t.Fatal(err)
	}
	mod := battleye.Player{ID: 1, GUID: modGUID, Name: "Mod"}
	setOnline(mod, battleye.Player{ID: 4, GUID: storetest.GUID1, Name: "Rowdy"}, battleye.Player{ID: 6, GUID: storetest.GUID2, Name: "Other"})

	var mu sync.Mutex
	var said, kicked, banned []string
	oldSay, oldKick, oldBan, oldDelay := chatSay, chatKick, banStep, warningActionDelay
	t.Cleanup(func() {
		chatSay, chatKick, banStep, warningActionDelay = oldSay, oldKick, oldBan, oldDelay
		chatState.Lock()
		chatState.loaded = false
		chatState.Unlock()
	})
	chatSay = func(to battleye.Player, everyone bool, text string) bool {
		mu.Lock()
		defer mu.Unlock()
		if everyone {
			said = append(said, "all: "+text)
		} else {
			said = append(said, fmt.Sprintf("%d: %s", to.ID, text))
		}
		return true
	}
	chatKick = func(p battleye.Player, reason string) bool {
		mu.Lock()
		kicked = append(kicked, fmt.Sprintf("%d %s", p.ID, reason))
		mu.Unlock()
		return true
	}
	banStep = func(p battleye.Player, online bool, minutes int, reason, logAs string) bans.Result {
		mu.Lock()
		banned = append(banned, fmt.Sprintf("%s %d %s", p.GUID, minutes, reason))
		mu.Unlock()
		return bans.Result{OK: true}
	}
	warningActionDelay = 10 * time.Millisecond
	settings := DefaultChatSettings()
	w := settings["warning"]
	w.Announce = false
	settings["warning"] = w
	chatState.Lock()
	chatState.settings, chatState.loaded = settings, true
	chatState.Unlock()
	take := func() (s, k, b []string) {
		mu.Lock()
		defer mu.Unlock()
		s, k, b, said, kicked, banned = said, kicked, banned, nil, nil, nil
		return
	}
	wait := func() { time.Sleep(100 * time.Millisecond) }

	handleChatCommand(mod, roleMod, "!warning 4 spamming chat")
	wait()
	s, k, b := take()
	if fmt.Sprint(s) != fmt.Sprint([]string{"4: Warning 1/2 from Mod: spamming chat", "1: Rowdy was warned by Mod (1/2): spamming chat"}) || k != nil || b != nil {
		t.Fatalf("first warning: said %q, kicked %q, banned %q", s, k, b)
	}
	handleChatCommand(mod, roleMod, "!warning #4 still spamming")
	wait()
	s, k, b = take()
	if len(s) != 3 || s[0] != "4: Warning 2/2 from Mod: still spamming" || s[2] != "1: Rowdy reached 2 warnings and has been banned: still spamming" {
		t.Fatalf("second warning said %q", s)
	}
	if fmt.Sprint(b) != fmt.Sprint([]string{storetest.GUID1 + " 0 still spamming"}) || k != nil {
		t.Fatalf("at the threshold: banned %q, kicked %q; want a permanent ban with the last reason", b, k)
	}
	// The count starts again after the threshold.
	handleChatCommand(mod, roleMod, "!warning 4 again")
	wait()
	if s, _, b := take(); len(s) == 0 || s[0] != "4: Warning 1/2 from Mod: again" || b != nil {
		t.Fatalf("after the ban the count didn't start again: %q", s)
	}

	// Kick at a threshold of 1, by code; Players can't warn.
	w = settings["warning"]
	w.Threshold, w.Action = 1, "kick"
	settings["warning"] = w
	handleChatCommand(battleye.Player{ID: 6, GUID: storetest.GUID2, Name: "Other"}, RolePlayer, "!warning 1 abuse")
	handleChatCommand(mod, roleMod, "!warning "+codeOf(storetest.GUID2)+" teamkilling")
	wait()
	if _, k, b := take(); fmt.Sprint(k) != fmt.Sprint([]string{"6 teamkilling"}) || b != nil {
		t.Fatalf("threshold 1, kick: kicked %q, banned %q", k, b)
	}

	// !kick by player number.
	handleChatCommand(mod, roleMod, "!kick 4 bye")
	if _, k, _ := take(); fmt.Sprint(k) != fmt.Sprint([]string{"4 bye"}) {
		t.Fatalf("!kick 4: kicked %q", k)
	}
}

func TestWarningSettingsChecked(t *testing.T) {
	for _, c := range []struct {
		threshold int
		action    string
		ok        bool
	}{{2, "ban", true}, {1, "kick", true}, {0, "ban", false}, {-1, "ban", false}, {101, "ban", false}, {2, "mute", false}} {
		s := DefaultChatSettings()
		w := s["warning"]
		w.Threshold, w.Action = c.threshold, c.action
		s["warning"] = w
		if err := checkChatSettings(s); (err == nil) != c.ok {
			t.Errorf("threshold %d, action %q: err = %v", c.threshold, c.action, err)
		}
	}
}

// pingCounterFlow is the ping-warning example: ++ on a high ping, kick at
// the threshold, otherwise a warning; reset when the ping is fine.
func pingCounterFlow() []Step {
	inner := Step{Type: "if", Branches: []Branch{{Subject: "counter", Op: "ge", Value: "{threshold}",
		Steps: []Step{{Type: "kick", Who: "player", Reason: "Ping {player_ping} over the limit"}}}},
		Else: &[]Step{{Type: "message", To: "player", Text: "Warning {counter}/{threshold}"}}}
	return []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if",
		Branches:    []Branch{{Subject: "player_ping", Op: "ge", Value: "500", Counter: "inc", Steps: []Step{inner}}},
		Else:        &[]Step{},
		ElseCounter: "reset"}}}}
}

// TestFlowCounter checks the per-player counter of a run: ++ on each high
// ping, reset when it's fine, {counter} and {threshold} in messages
// and Ifs, and a fresh run starting at 0.
func TestFlowCounter(t *testing.T) {
	testDB(t)
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { sent = append(sent, cmd) }
	t.Cleanup(func() { sendCommand = oldSend })
	if err := checkFlowSteps(triggerSchedule, 0, pingCounterFlow(), nil); err == nil || !strings.Contains(err.Error(), "Counter threshold") {
		t.Fatalf("{threshold} without a Counter threshold: err = %v", err)
	}
	if err := checkFlowSteps(triggerSchedule, 3, pingCounterFlow(), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &flowRun{name: "ping", ctx: ctx, cancel: cancel, counterThreshold: 3}
	x := &job{run: run, ctx: ctx}
	pass := func(lagPing, flakyPing int) []string {
		sent = nil
		setOnline(battleye.Player{ID: 4, GUID: storetest.GUID1, Name: "Laggy", Ping: lagPing}, battleye.Player{ID: 6, GUID: storetest.GUID2, Name: "Flaky", Ping: flakyPing})
		if !x.execFlowSteps(pingCounterFlow()) {
			t.Fatal("the run ended early")
		}
		return sent
	}
	steps := [][2]int{{600, 700}, {650, 40}, {700, 800}, {700, 900}}
	want := [][]string{
		{"say 4 Warning 1/3", "say 6 Warning 1/3"},
		{"say 4 Warning 2/3"},                                   // Flaky's ping is fine: reset
		{"kick 4 Ping 700 over the limit", "say 6 Warning 1/3"}, // Laggy's 3rd in a row
		{"kick 4 Ping 700 over the limit", "say 6 Warning 2/3"}, // still over: the counter stays past the threshold
	}
	for i, p := range steps {
		if got := pass(p[0], p[1]); fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Fatalf("pass %d sent %q, want %q", i+1, got, want[i])
		}
	}
	if !strings.Contains(strings.Join(run.log, "\n"), "Laggy (#4): counter ++, now 3/3") {
		t.Fatalf("run log:\n%s", strings.Join(run.log, "\n"))
	}

	// A new run starts every counter at 0.
	run2 := &flowRun{name: "ping", ctx: ctx, cancel: cancel, counterThreshold: 3}
	x = &job{run: run2, ctx: ctx}
	if got := pass(600, 40); fmt.Sprint(got) != fmt.Sprint([]string{"say 4 Warning 1/3"}) {
		t.Fatalf("new run sent %q", got)
	}
}

func TestFlowCounterChecked(t *testing.T) {
	// A counter needs a player: not outside a For each in a Schedule flow.
	top := []Step{{Type: "if", Branches: []Branch{{Subject: "players_online", Op: "gt", Value: "0", Counter: "inc", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerSchedule, 0, top, nil); err == nil || !strings.Contains(err.Error(), "needs a player") {
		t.Fatalf("counter without a player: err = %v", err)
	}
	if err := checkFlowSteps(triggerJoin, 0, top, nil); err != nil {
		t.Fatalf("counter in a join flow: %v", err)
	}
	bad := []Step{{Type: "if", Branches: []Branch{{Subject: "player_ping", Op: "gt", Value: "0", Counter: "double", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerJoin, 0, bad, nil); err == nil {
		t.Fatal("an unknown counter change was accepted")
	}
	word := []Step{{Type: "if", Branches: []Branch{{Subject: "player_ping", Op: "gt", Value: "{player_ping}", Steps: []Step{}}}}}
	if err := checkFlowSteps(triggerJoin, 0, word, nil); err == nil {
		t.Fatal("a number If compared with a placeholder other than {threshold}")
	}
	for _, n := range []int{-2, 1001} {
		f := Flow{Name: "x", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}, CounterThreshold: n}
		if err := checkFlowDefinition(f); err == nil {
			t.Errorf("counter threshold %d accepted", n)
		}
	}
	// No threshold (0) is fine, and so is a counter compared with a number.
	noThreshold := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "if",
		Branches: []Branch{{Subject: "counter", Op: "ge", Value: "3", Counter: "inc", Steps: []Step{}}}}}}}
	if err := checkFlowDefinition(Flow{Name: "x", Trigger: triggerManual, Steps: noThreshold}); err != nil {
		t.Errorf("no threshold: %v", err)
	}
	msg := []Step{{Type: "forEach", Of: "player", Steps: []Step{{Type: "message", To: "player", Text: "{counter}/{threshold}"}}}}
	if err := checkFlowSteps(triggerManual, 0, msg, nil); err == nil {
		t.Error("{threshold} in a message accepted without a Counter threshold")
	}
	if err := checkFlowSteps(triggerManual, 2, msg, nil); err != nil {
		t.Errorf("{threshold} with a threshold: %v", err)
	}
}

// TestFlowCounterThresholdStored checks the threshold is saved and read
// back, is none (0) when not sent, and that a flows table from before
// counters gets the column.
func TestFlowCounterThresholdStored(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`CREATE TABLE flows (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, script TEXT NOT NULL,
		cancel TEXT NOT NULL DEFAULT '', timer TEXT NOT NULL DEFAULT '', schedule TEXT NOT NULL DEFAULT '',
		schedule_enabled INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO flows (id, name, script, schedule) VALUES (0, 'old', '[{"type":"lock"}]', 'manual')`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	if f, err := Get(0); err != nil || f.CounterThreshold != 0 {
		t.Fatalf("old flow: threshold %d, err %v; want none", f.CounterThreshold, err)
	}
	id, err := Save(Flow{Name: "four", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}, CounterThreshold: 4}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := Get(id); f.CounterThreshold != 4 {
		t.Fatalf("saved threshold %d, want 4", f.CounterThreshold)
	}
	id, err = Save(Flow{Name: "unsent", Trigger: triggerManual, Steps: []Step{{Type: "lock"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := Get(id); f.CounterThreshold != 0 {
		t.Fatalf("threshold not sent: %d, want none", f.CounterThreshold)
	}
}

// TestStopEndsWhenStopped checks that the When stopped steps run after Stop,
// and that pressing Stop again ends them, even during a Wait.
func TestStopEndsWhenStopped(t *testing.T) {
	testDB(t)
	var mu sync.Mutex
	var sent []string
	oldSend := sendCommand
	sendCommand = func(_ *flowRun, cmd string) { mu.Lock(); sent = append(sent, cmd); mu.Unlock() }
	t.Cleanup(func() { sendCommand = oldSend })
	f := Flow{ID: 77, Name: "restart", Trigger: triggerManual,
		Steps:  []Step{{Type: "wait", Time: "1h"}},
		OnStop: []Step{{Type: "unlock"}, {Type: "wait", Time: "1h"}, {Type: "lock"}}}
	if err := launchFlowRun(f, time.Now(), "test", nil); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatal(what)
	}
	if err := Stop(f.ID); err != nil {
		t.Fatal(err)
	}
	waitFor("When stopped didn't start", func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) == 1 && sent[0] == "#unlock" })
	if err := Stop(f.ID); err != nil { // still running: the When stopped Wait
		t.Fatalf("second Stop: %v", err)
	}
	waitFor("the second Stop didn't end the run", func() bool {
		flowRuns.Lock()
		defer flowRuns.Unlock()
		return !flowRuns.byFlow[f.ID].active
	})
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(sent) != fmt.Sprint([]string{"#unlock"}) {
		t.Fatalf("sent %q; the Lock after the Wait should not go out", sent)
	}
}
