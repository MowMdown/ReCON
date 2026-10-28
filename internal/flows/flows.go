// Package flows runs flows: lists of steps built on the Flows page (send a
// message, wait, lock the server, kick the player, an If with Else ifs and
// an Else, a For each, and so on). Steps run top to bottom; only Wait takes
// time.
//
// Every flow has one trigger (triggers.go):
//
//	schedule  a cron schedule, while Enabled
//	join      once for each player who finishes loading in
//	chat      once for each chat message a player sends
//	manual    only when Run is pressed
//
// and an optional delay before step 1. "The player" is whoever triggered a
// join or chat run; {player_name}, {player_id}, {player_ping}, {player_ip},
// {player_guid}, {player_role} and {message} are filled in from them.
// Schedule and Manual flows have no player, so they message everyone only.
//
// The package also has the built-in chat commands (!help, !shout, !whisper,
// !players, !vote, !code, !kick, !ban, !warning), in commands.go.
package flows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/store"
)

// Flow triggers.
const (
	triggerManual   = "manual"   // only Run starts it
	triggerSchedule = "schedule" // cron schedule, while Enabled
	triggerJoin     = "join"     // once per joining player, while Enabled
	triggerChat     = "chat"     // once per chat message, while Enabled
)

// What the schedule column holds for the triggers without a cron expression.
const (
	scheduleManual = "manual"
	scheduleJoin   = "on-join"
	scheduleChat   = "on-chat"
)

// Flow is one flow as the Flows page and the API see it. In the flows table
// the steps are JSON in the script column, When stopped is JSON in the
// cancel column, and Delay is in the timer column.
type Flow struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Trigger is manual, schedule, join or chat. It is stored in the
	// schedule column ("manual", "on-join", "on-chat" or the cron
	// expression) and filled in when read.
	Trigger string `json:"trigger"`
	// Schedule is the cron expression (schedule flows only).
	Schedule string `json:"schedule"`
	// Delay is the time before step 1, as typed ("", "5s", "1m20s").
	Delay  string `json:"delay"`
	Steps  []Step `json:"steps"`
	OnStop []Step `json:"onStop"`
	// CounterThreshold is {threshold}: what the flow's per-player
	// counters are compared to (1 to maxCounterThreshold), or 0 for none.
	CounterThreshold int `json:"counterThreshold"`
	// ScheduleEnabled turns the trigger on (schedule, join and chat flows).
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	NextRun         string `json:"nextRun,omitempty"` // for the page, not stored
	LastRun         string `json:"lastRun,omitempty"` // for the page, not stored
}

// MaxNameLen is the longest flow name allowed, in characters. The Flow
// column on the page is sized to fit it.
const MaxNameLen = 48

// ClipName shortens a name to at most n characters (not bytes).
func ClipName(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// triggerFromSchedule reads a flow's trigger from its stored schedule. Blank counts
// as manual.
func triggerFromSchedule(schedule string) string {
	switch strings.ToLower(strings.Join(strings.Fields(schedule), " ")) {
	case scheduleManual, "":
		return triggerManual
	case scheduleJoin:
		return triggerJoin
	case scheduleChat:
		return triggerChat
	}
	return triggerSchedule
}

// storedScheduleText is what goes in the schedule column.
func storedScheduleText(f Flow) string {
	switch f.Trigger {
	case triggerJoin:
		return scheduleJoin
	case triggerChat:
		return scheduleChat
	case triggerSchedule:
		return f.Schedule
	}
	return scheduleManual
}

// flowFromRow fills a flow read from the database: its trigger from the
// schedule column and its steps from JSON. Steps that can't be read are left
// empty, so the flow fails its checks and doesn't run.
func flowFromRow(f *Flow, script, cancel string) {
	f.Trigger = triggerFromSchedule(f.Schedule)
	if f.Trigger != triggerSchedule {
		f.Schedule = ""
	}
	if f.Trigger == triggerManual {
		f.ScheduleEnabled = false
	}
	f.Steps, f.OnStop = []Step{}, []Step{}
	if err := json.Unmarshal([]byte(script), &f.Steps); err != nil {
		f.Steps = []Step{}
		log.Printf("flow[%s] steps can't be read: %v", f.Name, err)
	}
	if strings.TrimSpace(cancel) != "" {
		if err := json.Unmarshal([]byte(cancel), &f.OnStop); err != nil {
			f.OnStop = []Step{}
		}
	}
	f.Steps, f.OnStop = normalizeFlowSteps(f.Steps), normalizeFlowSteps(f.OnStop)
}

// flowFromRecord turns a flows row into a Flow (see flowFromRow).
func flowFromRecord(r store.FlowRecord) Flow {
	f := Flow{ID: r.ID, Name: r.Name, Delay: r.Delay, Schedule: r.Schedule,
		ScheduleEnabled: r.Enabled, CounterThreshold: r.CounterThreshold}
	flowFromRow(&f, r.Script, r.Cancel)
	return f
}

// List returns every stored flow in display order.
func List() ([]Flow, error) {
	rows, err := database.ListStoredFlows()
	if err != nil {
		return nil, err
	}
	flows := make([]Flow, len(rows))
	for i, r := range rows {
		flows[i] = flowFromRecord(r)
	}
	return flows, nil
}

// Get returns one stored flow, or sql.ErrNoRows.
func Get(id int64) (Flow, error) {
	r, err := database.GetStoredFlow(id)
	if err != nil {
		return Flow{}, err
	}
	return flowFromRecord(r), nil
}

// stepsJSON is how steps are stored.
func stepsJSON(list []Step) string {
	b, _ := json.Marshal(normalizeFlowSteps(list))
	return string(b)
}

// triggerLabel is a trigger's name as the page shows it.
func triggerLabel(t string) string {
	switch t {
	case triggerSchedule:
		return "Schedule"
	case triggerJoin:
		return "On join"
	case triggerChat:
		return "Player message"
	}
	return "Manual"
}

// normalizeTriggerName checks the trigger the page sent. Empty means manual.
func normalizeTriggerName(t string) (string, error) {
	switch t = strings.ToLower(strings.TrimSpace(t)); t {
	case "":
		return triggerManual, nil
	case triggerManual, triggerSchedule, triggerJoin, triggerChat:
		return t, nil
	}
	return "", fmt.Errorf("unknown trigger %q (use manual, schedule, join or chat)", t)
}

// cronExamples follows every schedule error, so the page shows what a valid
// schedule looks like.
const cronExamples = `Enter a cron expression such as:
0 6 * * *  - every day at 06:00
0 */6 * * *  - every 6 hours (00:00, 06:00, 12:00, 18:00)
30 4 * * 1,4  - Mondays and Thursdays at 04:30
0 5 * * 1-5  - weekdays at 05:00`

// checkCronSchedule validates the schedule of a schedule-triggered flow.
func checkCronSchedule(schedule string) error {
	if triggerFromSchedule(schedule) != triggerSchedule {
		return errors.New("Schedule: " + cronExamples)
	}
	if _, err := parseCronExpression(schedule); err != nil {
		return errors.New("Schedule: " + err.Error() + ".\n" + cronExamples)
	}
	return nil
}

// flowDelaySeconds reads a flow's delay in seconds. Errors are user-facing.
func flowDelaySeconds(f Flow) (int, error) {
	if strings.TrimSpace(f.Delay) == "" {
		return 0, nil
	}
	secs, ok := parseDur(f.Delay)
	switch {
	case !ok:
		return 0, errors.New("Delay: enter a time such as 30 (seconds), 90s or 1m20s")
	case secs > maxTimeSecs:
		return 0, errors.New("Delay: 24h at most")
	}
	return secs, nil
}

// checkFlowDefinition runs every check a flow must pass to be saved or run.
func checkFlowDefinition(f Flow) error {
	if f.Trigger == triggerSchedule {
		if err := checkCronSchedule(f.Schedule); err != nil {
			return err
		}
	}
	if _, err := flowDelaySeconds(f); err != nil {
		return err
	}
	if f.CounterThreshold < 0 || f.CounterThreshold > maxCounterThreshold {
		return fmt.Errorf("Counter threshold: leave it empty, or enter a whole number from 1 to %d", maxCounterThreshold)
	}
	return checkFlowSteps(f.Trigger, f.CounterThreshold, f.Steps, f.OnStop)
}

// Init creates the flows table (empty on a new database) and logs any
// saved flow that can't run.
func Init() error {
	if err := database.EnsureFlowsTable(); err != nil {
		return err
	}
	flows, err := List()
	if err != nil {
		return err
	}
	for _, f := range flows {
		if err := checkFlowDefinition(f); err != nil {
			log.Printf("flow[%s] won't run until it is fixed: %v", f.Name, err)
		}
	}
	return nil
}

// ListWithNextRun lists every flow and fills NextRun and LastRun for the page.
func ListWithNextRun() ([]Flow, error) {
	flows, err := List()
	if err != nil {
		return nil, err
	}
	flowRuns.Lock()
	for i := range flows {
		if r := flowRuns.byFlow[flows[i].ID]; r != nil {
			flows[i].LastRun = fmtWhen(r.lastRun)
		}
	}
	flowRuns.Unlock()
	now := time.Now()
	for i := range flows {
		f := &flows[i]
		switch {
		case f.Trigger == triggerManual:
			f.NextRun = "Manual only"
		case !f.ScheduleEnabled:
		case f.Trigger == triggerJoin:
			f.NextRun = "On player join"
		case f.Trigger == triggerChat:
			f.NextRun = "On player message"
		default:
			if c, err := parseCronExpression(f.Schedule); err == nil {
				if next, ok := c.next(now); ok {
					delay, _ := flowDelaySeconds(*f)
					f.NextRun = fmtWhen(next.Add(time.Duration(delay) * time.Second))
				}
			}
		}
	}
	return flows, nil
}

// Save checks a flow from the page (or an import) and stores
// it: a new flow when isNew, else an update of flow f.ID. Every error it
// returns is a user-facing message.
//
// New flows are saved disabled and manual flows are never enabled. A saved
// flow's trigger can't be changed (a restart flow must never turn into one
// that fires on every join), and saving an edit keeps it enabled or disabled
// as it was.
func Save(f Flow, isNew bool) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	f.Delay = strings.ToLower(strings.TrimSpace(f.Delay))
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if utf8.RuneCountInString(f.Name) > MaxNameLen {
		return 0, fmt.Errorf("Name: %d characters max", MaxNameLen)
	}
	trig, err := normalizeTriggerName(f.Trigger)
	if err != nil {
		return 0, err
	}
	f.Trigger = trig
	var old Flow
	if !isNew {
		if old, err = Get(f.ID); err != nil {
			return 0, errors.New("this flow no longer exists; reload the page")
		}
		if old.Trigger != f.Trigger {
			return 0, fmt.Errorf("a saved flow's trigger can't be changed (this one is %s); make a new flow instead", triggerLabel(old.Trigger))
		}
	}
	f.Steps, f.OnStop = normalizeFlowSteps(f.Steps), normalizeFlowSteps(f.OnStop)
	if f.Trigger != triggerSchedule && f.Trigger != triggerManual {
		f.OnStop = []Step{}
	}
	if err := checkFlowDefinition(f); err != nil {
		return 0, err
	}
	f.Schedule = storedScheduleText(f)
	f.ScheduleEnabled = !isNew && f.Trigger != triggerManual && old.ScheduleEnabled
	id, err := database.SaveStoredFlow(store.FlowRecord{ID: f.ID, Name: f.Name, Script: stepsJSON(f.Steps), Cancel: stepsJSON(f.OnStop),
		Delay: f.Delay, Schedule: f.Schedule, Enabled: f.ScheduleEnabled, CounterThreshold: f.CounterThreshold}, isNew)
	if err != nil {
		return 0, errors.New("could not save (is the name already used?)")
	}
	WakeScheduler()
	return id, nil
}

// CheckCanEnable reports why a flow can't be enabled, if it can't.
func CheckCanEnable(f Flow) error {
	if f.Trigger == triggerManual {
		return errors.New("manual flows have nothing to enable - press Run to start one, or edit the flow and pick another trigger")
	}
	return checkFlowDefinition(f)
}

// Preview is the edit dialog's line under the trigger: what the
// schedule means and when the next runs start.
func Preview(f Flow) map[string]any {
	trig, err := normalizeTriggerName(f.Trigger)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	f.Trigger = trig
	delay, err := flowDelaySeconds(f)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if trig != triggerSchedule {
		return map[string]any{"ok": true}
	}
	if err := checkCronSchedule(f.Schedule); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	c, _ := parseCronExpression(f.Schedule)
	resp := map[string]any{"ok": true, "description": c.describe()}
	next := []string{}
	t := time.Now()
	for i := 0; i < 3; i++ {
		n, ok := c.next(t)
		if !ok {
			break
		}
		next = append(next, fmtWhen(n.Add(time.Duration(delay)*time.Second)))
		t = n
	}
	if len(next) == 0 {
		resp["description"] = c.describe() + " (this never happens)"
	}
	resp["next"] = next
	return resp
}

// maxSteps and maxDepth keep a flow to a size the page can show.
const (
	maxSteps = 500
	maxDepth = 10
)

var stepTypes = map[string]bool{"message": true, "playerList": true, "wait": true, "lock": true,
	"unlock": true, "shutdown": true, "kick": true, "ban": true, "kickAll": true, "command": true,
	"stop": true, "if": true, "forEach": true}

// triggerHas is what a trigger provides: a player, and a chat message.
func triggerHas(trigger string) map[string]bool {
	return map[string]bool{
		needNothing: true,
		needPlayer:  trigger == triggerJoin || trigger == triggerChat,
		needChat:    trigger == triggerChat,
	}
}

type stepChecker struct {
	has    map[string]bool
	n      int    // step number, counted as the page numbers them
	inLoop bool   // inside a For each player
	where  string // "Step" or "When stopped, step"
}

func (c *stepChecker) fail(msg string) error { return fmt.Errorf("%s %d: %s", c.where, c.n, msg) }

// tokensOK reports the first placeholder in text the trigger can't fill.
func (c *stepChecker) tokensOK(text string) error {
	for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
		need, ok := flowTokens[m[1]]
		if need == needThreshold && !c.has[need] {
			return c.fail(m[0] + " " + needText[need])
		}
		if ok && need != needNothing && need != needThreshold && !c.has[need] {
			return c.fail(m[0] + " " + needText[need])
		}
	}
	return nil
}

func (c *stepChecker) checkStepList(list []Step, depth int) error {
	if depth > maxDepth {
		return c.fail(fmt.Sprintf("Ifs can go %d deep at most", maxDepth))
	}
	for i := range list {
		s := &list[i]
		c.n++
		if !stepTypes[s.Type] {
			return c.fail(fmt.Sprintf("unknown step %q", s.Type))
		}
		player := c.has[needPlayer]
		switch s.Type {
		case "message", "playerList":
			switch s.To {
			case "all":
			case "player":
				if !player {
					return c.fail(`"Player" ` + needText[needPlayer])
				}
			default:
				return c.fail("pick who it goes to")
			}
			if s.Type == "message" && s.Text == "" {
				return c.fail("Add the message text")
			}
		case "wait":
			secs, ok := parseDur(s.Time)
			if !ok || secs <= 0 {
				return c.fail("Enter a time: 30 (seconds), 90s, 1m20s or 1h")
			}
			if secs > maxTimeSecs {
				return c.fail("A wait can be 24h at most")
			}
		case "kick", "ban":
			if !player {
				return c.fail("Kick and Ban " + needText[needPlayer])
			}
			s.Who = "player"
			if s.Type == "ban" {
				if n, err := strconv.Atoi(s.Len); err != nil || n < 0 {
					return c.fail("pick how long the ban lasts")
				}
			}
		case "command":
			if s.Cmd == "" {
				return c.fail("Type the command to send")
			}
		case "if":
			if err := c.checkIfStep(s, depth); err != nil {
				return err
			}
			continue // the branches checked their own steps
		case "forEach":
			if err := c.checkForEach(s, depth); err != nil {
				return err
			}
			continue
		}
		for _, t := range []string{s.Text, s.Reason, s.Cmd} {
			if err := c.tokensOK(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkForEach checks a For each's steps with a player (each one in turn)
// and a {message} (that player's line) available.
func (c *stepChecker) checkForEach(s *Step, depth int) error {
	if c.inLoop {
		return c.fail("a For each can't go inside another one")
	}
	if s.Of != forEachPlayer && s.Of != forEachMessage {
		return c.fail("pick what For each goes through: player or message")
	}
	outer := c.has
	c.has = map[string]bool{needNothing: true, needPlayer: true, needChat: true, needThreshold: outer[needThreshold]}
	c.inLoop = true
	err := c.checkStepList(s.Steps, depth+1)
	c.has, c.inLoop = outer, false
	return err
}

func (c *stepChecker) checkIfStep(s *Step, depth int) error {
	if len(s.Branches) == 0 {
		return c.fail("this If has no condition")
	}
	first := s.Branches[0].Subject
	sub, ok := condSubjects[first]
	if !ok {
		return c.fail(fmt.Sprintf("unknown If subject %q", first))
	}
	if !c.has[sub.need] {
		return c.fail("{" + first + "} " + needText[sub.need])
	}
	num := c.n // the If's own number, for its conditions' errors
	for _, b := range s.Branches {
		if b.Subject != first {
			return fmt.Errorf("%s %d: every Else if must compare {%s}, like its If", c.where, num, first)
		}
		okOp := false
		for _, op := range condOps[sub.kind] {
			okOp = okOp || op == b.Op
		}
		if !okOp {
			return fmt.Errorf("%s %d: pick an operator", c.where, num)
		}
		switch {
		case sub.kind == "num" && strings.TrimSpace(b.Value) == counterThresholdToken:
			if !c.has[needThreshold] {
				return fmt.Errorf("%s %d: %s %s", c.where, num, counterThresholdToken, needText[needThreshold])
			}
		case sub.kind == "num" || first == "player_id":
			if _, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil {
				return fmt.Errorf("%s %d: Enter a whole number", c.where, num)
			}
		case sub.kind == "role":
			if n, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil || n < RolePlayer || n > RoleAdmin {
				return fmt.Errorf("%s %d: pick Player, Mod or Admin", c.where, num)
			}
		case first == "player_guid":
			if !battleye.GUIDPattern.MatchString(strings.TrimSpace(b.Value)) {
				return fmt.Errorf("%s %d: Enter a GUID (32 letters and digits, 0-9 and a-f)", c.where, num)
			}
		case sub.kind == "word":
			if len(terms(b.Value)) == 0 {
				return fmt.Errorf(`%s %d: Enter a word, several words, or a "quoted phrase"`, c.where, num)
			}
		}
	}
	counters := []string{s.ElseCounter}
	for _, b := range s.Branches {
		counters = append(counters, b.Counter)
	}
	for _, op := range counters {
		if _, ok := counterOps[op]; !ok {
			return fmt.Errorf("%s %d: pick what the counter does: 0, ++, -- or reset", c.where, num)
		}
		if op != "" && !c.has[needPlayer] {
			return fmt.Errorf("%s %d: the counter is kept for each player, so it needs a player: use it inside a For each, or with the On join or Player message trigger", c.where, num)
		}
	}
	for _, l := range branchLists(s) {
		if err := c.checkStepList(l, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// checkFlowSteps validates a flow's steps and its When stopped steps for its
// trigger and Counter threshold (0: none). When stopped takes every kind of
// step, with the same rules. Errors name the step as the page numbers them.
func checkFlowSteps(trigger string, threshold int, steps, onStop []Step) error {
	if len(steps) == 0 {
		return errors.New("add at least one step")
	}
	if countAllSteps(steps)+countAllSteps(onStop) > maxSteps {
		return fmt.Errorf("a flow can have %d steps at most", maxSteps)
	}
	c := &stepChecker{has: triggerHas(trigger), where: "Step"}
	c.has[needThreshold] = threshold > 0
	if err := c.checkStepList(steps, 0); err != nil {
		return err
	}
	if len(onStop) > 0 && trigger != triggerSchedule && trigger != triggerManual {
		return errors.New("only Schedule and Manual flows have When stopped steps")
	}
	s := &stepChecker{has: triggerHas(trigger), where: "When stopped, step"}
	s.has[needThreshold] = threshold > 0
	return s.checkStepList(onStop, 0)
}

// flowPlayer is the player a join or chat run is for, as they were when it
// started. Their number is looked up again (by GUID) whenever it's used,
// because numbers are reused when players leave.
type flowPlayer struct {
	battleye.Player
	role    int
	message string
}

type job struct {
	run *flowRun
	ctx context.Context
	p   *flowPlayer // nil for schedule and manual runs; each player in turn inside a For each player
	// inLoop: running a For each player's steps. Conditions are then only
	// logged when they match (with the player's name), so a check of 60
	// players doesn't fill the run log with "no".
	inLoop bool
}

// sendCommand sends one flow command. Tests replace it.
var sendCommand = runFlowCommand

// currentID is the player's number now, or false if they have left.
func (x *job) currentID() (int, bool) {
	if x.p == nil {
		return 0, false
	}
	if p, ok := game.PlayerByGUID(x.p.GUID); ok {
		return p.ID, true
	}
	return 0, false
}

// placeholderValue is what a placeholder or If subject stands for in this run.
func (x *job) placeholderValue(name string) (string, bool) {
	switch name {
	case "players_online":
		return strconv.Itoa(len(onlineNow())), true
	case "threshold":
		return strconv.Itoa(x.run.counterThreshold), true
	}
	if x.p == nil {
		return "", false
	}
	switch name {
	case "player_name":
		return battleye.BareName(x.p.Name), true
	case "player_id":
		if id, ok := x.currentID(); ok {
			return strconv.Itoa(id), true
		}
		return strconv.Itoa(x.p.ID), true
	case "player_ping":
		return strconv.Itoa(x.p.Ping), true
	case "player_ip":
		return x.p.IP, true
	case "player_guid":
		return x.p.GUID, true
	case "player_role":
		return RoleName(x.p.role), true
	case "message":
		return x.p.message, true
	case "counter":
		return strconv.Itoa(x.run.counterOf(x.p.GUID)), true
	}
	return "", false
}

// ifSubjectValue is what an If compares: the same as value, except that a role
// is its number (0 Player, 1 Mod, 2 Admin) so "is at least Mod" can work.
func (x *job) ifSubjectValue(name string) (string, bool) {
	if name == "player_role" && x.p != nil {
		return strconv.Itoa(x.p.role), true
	}
	return x.placeholderValue(name)
}

// fillPlaceholders replaces the placeholders in a step's text. Unknown {words} are left
// as typed, and what is filled in is not expanded again.
func (x *job) fillPlaceholders(text string) string {
	return battleye.CleanReason(tokenRe.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := x.placeholderValue(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	}))
}

// sendChatMessage sends a message to everyone, or to the player. A private message is
// added to the player's chat history, as the Message dialog does.
func (x *job) sendChatMessage(to, text string) {
	if text == "" {
		return
	}
	if to != "player" {
		sendCommand(x.run, "say -1 "+text)
		return
	}
	id, ok := x.currentID()
	if !ok {
		x.run.logf("%s has left; message not sent", battleye.BareName(x.p.Name))
		return
	}
	cmd := fmt.Sprintf("say %d %s", id, text)
	sendCommand(x.run, cmd)
	recordSentMessage(x.p.GUID, cmd)
}

// recordSentMessage adds a private message ReCON sent to a player's chat history.
func recordSentMessage(guid, cmd string) {
	if database == nil || guid == "" {
		return
	}
	if err := database.AddChatMessage(time.Now(), guid, store.ChatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// listOnlineNames is the latest player list as "Online (N): a, b, c".
func listOnlineNames() string {
	online := onlineNow()
	names := make([]string, 0, len(online))
	for _, p := range online {
		names = append(names, battleye.BareName(p.Name))
	}
	if len(names) == 0 {
		return "Online (0): nobody"
	}
	return fmt.Sprintf("Online (%d): %s", len(names), strings.Join(names, ", "))
}

// banStep bans the player. Tests replace it.
var banStep = func(p battleye.Player, online bool, minutes int, reason, logAs string) game.BanResult {
	return game.BanGUID(p, online, minutes, reason, logAs)
}

// execFlowSteps runs a list of steps. It returns false when the run must end: Stop
// was pressed during a wait, or a Stop flow step ran.
func (x *job) execFlowSteps(list []Step) bool {
	for i := range list {
		s := &list[i]
		if x.ctx.Err() != nil {
			return false
		}
		switch s.Type {
		case "message":
			x.sendChatMessage(s.To, x.fillPlaceholders(s.Text))
		case "playerList":
			x.sendChatMessage(s.To, listOnlineNames())
		case "wait":
			secs, _ := parseDur(s.Time)
			if !sleepUntil(x.ctx, time.Now().Add(time.Duration(secs)*time.Second)) {
				return false
			}
		case "lock", "unlock", "shutdown":
			sendCommand(x.run, "#"+s.Type)
		case "kickAll":
			sendCommand(x.run, strings.TrimSpace("kickall "+x.fillPlaceholders(s.Reason)))
		case "command":
			sendCommand(x.run, x.fillPlaceholders(s.Cmd))
		case "kick":
			id, ok := x.currentID()
			if !ok {
				x.run.logf("%s has left; kick skipped", battleye.BareName(x.p.Name))
				continue
			}
			reason := x.fillPlaceholders(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			sendCommand(x.run, fmt.Sprintf("kick %d %s", id, reason))
		case "ban":
			minutes, _ := strconv.Atoi(s.Len)
			reason := x.fillPlaceholders(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			p := x.p.Player
			id, online := x.currentID()
			p.ID = id
			res := banStep(p, online, minutes, reason, x.run.name)
			if !res.OK {
				x.run.logf("ban of %s failed: %s", battleye.BareName(p.Name), res.Error)
				x.run.markRunFailed()
			} else {
				x.run.logf("banned %s (%s)", battleye.BareName(p.Name), banLength(minutes))
			}
		case "stop":
			x.run.logf("Stop flow step: the run ends here")
			return false
		case "forEach":
			if !x.runForEachStep(s.Of, s.Steps) {
				return false
			}
		case "if":
			ran := false
			for k, b := range s.Branches {
				v, _ := x.ifSubjectValue(b.Subject)
				cmp := b
				if strings.TrimSpace(cmp.Value) == counterThresholdToken {
					cmp.Value = strconv.Itoa(x.run.counterThreshold)
				}
				ok := evaluateIfCondition(cmp, v)
				word := "If "
				if k > 0 {
					word = "Else if "
				}
				if !x.inLoop {
					x.run.logf("%s%s: %s", word, describeIfCondition(b), map[bool]string{true: "yes", false: "no"}[ok])
				} else if ok {
					x.run.logf("%s (#%d): %s%s", battleye.BareName(x.p.Name), x.p.ID, word, describeIfCondition(b))
				}
				if ok {
					x.changeCounter(b.Counter)
					if !x.execFlowSteps(b.Steps) {
						return false
					}
					ran = true
					break
				}
			}
			if !ran && s.Else != nil {
				if x.inLoop {
					x.run.logf("%s (#%d): Else", battleye.BareName(x.p.Name), x.p.ID)
				} else {
					x.run.logf("Else")
				}
				x.changeCounter(s.ElseCounter)
				if !x.execFlowSteps(*s.Else) {
					return false
				}
			}
		}
	}
	return true
}

// counterOf is a player's counter in this run (0 until a branch changes it).
func (r *flowRun) counterOf(guid string) int {
	r.counterMu.Lock()
	defer r.counterMu.Unlock()
	return r.counters[strings.ToLower(guid)]
}

// changeCounter applies an If branch's or Else's counter change (see
// Branch.Counter) to the player, and logs the new value when it changes.
func (x *job) changeCounter(op string) {
	if op == "" || x.p == nil {
		return
	}
	r, g := x.run, strings.ToLower(x.p.GUID)
	r.counterMu.Lock()
	if r.counters == nil {
		r.counters = map[string]int{}
	}
	old := r.counters[g]
	n := old
	switch op {
	case "inc":
		n++
	case "dec":
		n = max(n-1, 0)
	case "reset":
		n = 0
	}
	r.counters[g] = n
	r.counterMu.Unlock()
	if n != old {
		r.logf("%s (#%d): counter %s, now %d/%d", battleye.BareName(x.p.Name), x.p.ID, counterOps[op], n, r.counterThreshold)
	}
}

// forEachMarks is, per flow, the Seq of the last server event its previous
// run's For each steps looked at, so a run only considers chat sent since
// the flow's last run. In memory: a restart starts again from the lines of
// the sessions in progress.
var forEachMarks = struct {
	sync.Mutex
	byFlow map[int64]int64
}{byFlow: map[int64]int64{}}

// chatWindowOf returns the chat lines (by server event Seq, (from, to])
// that this run's For each steps consider: everything since the flow's last
// run. The first call in a run fixes the window and moves the flow's mark,
// so every For each in the run sees the same lines.
func (r *flowRun) chatWindowOf() (from, to int64) {
	forEachMarks.Lock()
	defer forEachMarks.Unlock()
	if !r.chatWindow.set {
		r.chatWindow.from = forEachMarks.byFlow[r.flowID]
		r.chatWindow.to = game.Events.LastSeq()
		r.chatWindow.set = true
		forEachMarks.byFlow[r.flowID] = r.chatWindow.to
	}
	return r.chatWindow.from, r.chatWindow.to
}

// runForEachStep runs a For each's steps, in order: once for every player online
// (of "player", by player number), or once for every chat line players
// online sent this session that this flow hasn't handled yet (of "message",
// oldest first). Each run has that player as "the player"; players BattlEye
// hasn't verified (no GUID) are left out. It returns false when the run must
// end (Stop pressed, or a Stop flow step ran).
func (x *job) runForEachStep(of string, steps []Step) bool {
	type item struct {
		p   battleye.Player
		msg string
	}
	var items []item
	from, to := x.run.chatWindowOf()
	x.run.polled.Do(func() { game.PollPlayers() })
	if of == forEachMessage {
		senders := map[string]bool{}
		for _, l := range game.ChatsAfter(from) {
			if l.Chat.Seq > to {
				continue // sent after this run started looking
			}
			p, ok := game.PlayerByGUID(l.GUID)
			if !ok {
				continue // left since the line was sent
			}
			items = append(items, item{p, l.Chat.Text})
			senders[l.GUID] = true
		}
		x.run.logf("For each message: %d new from %d player(s)", len(items), len(senders))
	} else {
		for _, p := range onlineNow() { // by player number
			if p.GUID == "" {
				continue
			}
			msg := "" // only what they wrote since the flow's last run
			if c, ok := game.LatestChatOf(p.GUID); ok && c.Seq > from && c.Seq <= to {
				msg = c.Text
			}
			items = append(items, item{p, msg})
		}
		x.run.logf("For each player: %d online", len(items))
	}
	for _, it := range items {
		if x.ctx.Err() != nil {
			return false
		}
		each := &job{run: x.run, ctx: x.ctx, inLoop: true,
			p: &flowPlayer{Player: it.p, role: lookupPlayerRole(it.p.GUID), message: it.msg}}
		if !each.execFlowSteps(steps) {
			return false
		}
	}
	return true
}

// banLength is "permanently" or "for 1h", for log lines.
func banLength(minutes int) string {
	if minutes <= 0 {
		return "permanent"
	}
	return "for " + fmtDur(minutes*60)
}

// runFlowCommand sends one flow command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
//
// An RCON error, or no reply (except to #shutdown), marks the run as failed;
// the run carries on, and ends with status "error".
func runFlowCommand(run *flowRun, cmd string) {
	if cmd == "" {
		return
	}
	lower := strings.ToLower(cmd)

	// kickall [reason] is this program's own command, not a BattlEye one: it
	// lists the players and kicks each (battleye.KickAll).
	if bare := strings.TrimPrefix(lower, "#"); bare == "kickall" || strings.HasPrefix(bare, "kickall ") {
		reason := strings.TrimSpace(strings.TrimPrefix(cmd, "#")[len("kickall"):])
		rep, err := game.KickAll(reason, game.FlowSource(run.name))
		if err != nil {
			run.logf("kickall failed: %v", err)
			run.markRunFailed()
			return
		}
		run.addToRunLog(rep.Summary(reason))
		return
	}

	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := game.Run(cmd, 2*time.Second, retry, game.FlowSource(run.name))
	run.addToRunLog(fmt.Sprintf("sent %q (acked=%v err=%v)", cmd, acked, err))
	if err != nil || (!acked && lower != "#shutdown") {
		run.markRunFailed()
	}
}

// sleepUntil waits for t. It returns false if ctx was cancelled first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}

// flowRun is the live (or last finished) run of one flow. A schedule or
// manual run has one job; a join or chat run has one job per player in
// progress, all sharing this run's log, context and Stop button. Mutable
// fields are guarded by flowRuns' mutex.
type flowRun struct {
	flowID  int64
	name    string
	status  string // running | done | stopped | error
	active  bool
	failed  bool      // a command failed; the run ends as "error"
	stopped bool      // the Stop button was pressed
	anchor  time.Time // when step 1 starts (after the delay)
	lastRun time.Time // when the flow was last started
	jobs    int       // jobs still working through the steps
	log     []string
	onStop  []Step // sent once if Stop is pressed
	// chatWindow is the chat lines this run's For each steps look at:
	// Seq in (from, to]. Set by the first For each of the run (chatWindowOf).
	chatWindow struct {
		set      bool
		from, to int64
	}
	// counters is each player's counter (by lower-case GUID) for this run,
	// all 0 when it starts; counterThreshold is the flow's threshold.
	counterMu        sync.Mutex
	counters         map[string]int
	counterThreshold int
	// polled makes the run's first For each poll `players` once, for fresh
	// pings and anyone the console log missed (game.PollNow).
	polled   sync.Once
	stopOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
	// stopCancel ends the When stopped steps while they run (Stop pressed
	// again). Set by sendOnStop, under flowRuns' lock.
	stopCancel context.CancelFunc
}

// flowRuns holds each flow's current or last run, in memory only.
var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
}{byFlow: map[int64]*flowRun{}}

var (
	ErrRunning        = errors.New("flow is already running")
	errFlowNotRunning = errors.New("flow is not running")
	ErrBusy           = errors.New("stop the running flow first")
)

// The log line each kind of run starts with is "Started by <source>".
const (
	sourceManual   = "manual run"
	sourceSchedule = "schedule"
	sourceJoin     = "player join trigger"
	sourceChat     = "player message trigger"
)

// maxRunLog is how many lines a run's log keeps (join and chat runs can go
// on for a long time).
const maxRunLog = 500

// logf writes a line to the container log as flow[name] and to the run log
// shown on the Flows page.
func (r *flowRun) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("flow[%s] %s", r.name, msg)
	r.addToRunLog(msg)
}

// addToRunLog adds a line to the Flows page's run log only. Used for sent
// commands, which game.Run already logs to the container.
func (r *flowRun) addToRunLog(msg string) {
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	if len(r.log) > maxRunLog {
		r.log = append([]string(nil), r.log[len(r.log)-maxRunLog:]...)
	}
	flowRuns.Unlock()
}

func (r *flowRun) markRunFailed() {
	flowRuns.Lock()
	r.failed = true
	flowRuns.Unlock()
}

// sendOnStop runs the When stopped steps, once per run. They can be any
// steps, Waits included: pressing Stop again ends them (Stop), and
// like a run they stop after 24 hours at most.
func (r *flowRun) sendOnStop() {
	r.stopOnce.Do(func() {
		if len(r.onStop) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), maxTimeSecs*time.Second)
		defer cancel()
		flowRuns.Lock()
		r.stopCancel = cancel
		flowRuns.Unlock()
		r.logf("running the When stopped steps")
		if !(&job{run: r, ctx: ctx}).execFlowSteps(r.onStop) && ctx.Err() != nil {
			r.logf("When stopped steps stopped")
		}
	})
}

// finishRunJob is called when one job finishes. The last one ends the run:
// "stopped" if Stop was pressed, "error" if a command failed, else "done".
func (r *flowRun) finishRunJob(stopped bool) {
	flowRuns.Lock()
	defer flowRuns.Unlock()
	r.stopped = r.stopped || stopped
	if r.jobs--; r.jobs > 0 {
		return
	}
	r.active = false
	switch {
	case r.stopped:
		r.status = "stopped"
	case r.failed:
		r.status = "error"
	default:
		r.status = "done"
	}
	r.cancel() // release the context
}

type runView struct {
	FlowID     int64    `json:"flowId"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Active     bool     `json:"active"`
	AnchorUnix int64    `json:"anchorUnix"`
	Log        []string `json:"log"`
}

func SnapshotRuns() []runView {
	flowRuns.Lock()
	defer flowRuns.Unlock()
	out := []runView{}
	for _, r := range flowRuns.byFlow {
		out = append(out, runView{
			FlowID: r.flowID, Name: r.name, Status: r.status, Active: r.active,
			AnchorUnix: r.anchor.Unix(), Log: append([]string(nil), r.log...),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].FlowID < out[b].FlowID })
	return out
}

// Delete forgets a flow's last run and removes it from the database.
func Delete(id int64) error {
	flowRuns.Lock()
	if run := flowRuns.byFlow[id]; run != nil && run.active {
		flowRuns.Unlock()
		return ErrBusy
	}
	delete(flowRuns.byFlow, id)
	flowRuns.Unlock()
	forEachMarks.Lock()
	delete(forEachMarks.byFlow, id) // a new flow reusing the number starts afresh
	forEachMarks.Unlock()
	err := database.DeleteStoredFlow(id)
	WakeScheduler()
	return err
}

// launchFlowRun starts a run of f whose first step goes at `start`. p is the player
// for join and chat flows (nil otherwise); a player run adds a job to the
// flow's active run if there is one, while any other start is refused with
// ErrRunning.
func launchFlowRun(f Flow, start time.Time, source string, p *flowPlayer) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[f.ID]
	active := run != nil && run.active
	if active && p == nil {
		flowRuns.Unlock()
		return ErrRunning
	}
	if !active {
		ctx, cancel := context.WithCancel(context.Background())
		run = &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true,
			onStop: f.OnStop, ctx: ctx, cancel: cancel, counterThreshold: f.CounterThreshold}
		if old := flowRuns.byFlow[f.ID]; old != nil && p != nil {
			run.log = old.log // join and chat flows keep one running log
		}
		flowRuns.byFlow[f.ID] = run
	}
	run.jobs++
	run.anchor = start
	run.lastRun = time.Now()
	flowRuns.Unlock()
	if !active || p != nil {
		run.logf("Started by %s", source)
	}
	go func() { run.finishRunJob(runFlowJob(run, f, start, p)) }()
	return nil
}

// runFlowJob works through one job and reports whether it was stopped.
func runFlowJob(run *flowRun, f Flow, start time.Time, p *flowPlayer) (stopped bool) {
	who := ""
	if p != nil {
		who = fmt.Sprintf(" for %s (#%d)", battleye.BareName(p.Name), p.ID)
		if p.message != "" {
			run.logf("%s: %s", battleye.BareName(p.Name), p.message)
		} else {
			run.logf("player %d (%s) joined", p.ID, battleye.BareName(p.Name))
		}
	}
	if d := time.Until(start); d >= time.Second {
		run.logf("waiting %s before step 1", fmtDur(int((d+time.Second/2)/time.Second)))
	}
	if !sleepUntil(run.ctx, start) {
		run.logf("stopped%s", who)
		run.sendOnStop()
		return true
	}
	x := &job{run: run, ctx: run.ctx, p: p}
	if !x.execFlowSteps(f.Steps) && run.ctx.Err() != nil {
		run.logf("stopped%s", who)
		run.sendOnStop()
		return true
	}
	run.logf("done%s", who)
	return false
}

// lookupPlayerRole reads a player's role (Player when unknown).
func lookupPlayerRole(guid string) int {
	if database == nil {
		return RolePlayer
	}
	return database.GetPlayerRole(guid)
}

// startPlayerFlows runs every enabled flow with the given trigger once for
// the player, after each flow's delay.
func startPlayerFlows(trigger, source string, p *flowPlayer) {
	flows, err := List()
	if err != nil {
		log.Printf("%s flows: %v", trigger, err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != trigger || checkFlowDefinition(f) != nil {
			continue
		}
		delay, _ := flowDelaySeconds(f)
		pp := *p
		launchFlowRun(f, time.Now().Add(time.Duration(delay)*time.Second), source, &pp)
	}
}

// maxTimeSecs is the longest time a flow can use (a wait or the delay). It
// matches the 24 hour safety limit on a run.
const maxTimeSecs = 86400

var durRe = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// parseDur reads a time as typed on the pages: a plain number is seconds
// ("90"), or h, m and s can be combined, largest first ("90s", "1m20s",
// "2h 15m"). Spaces and case are ignored. ok is false for anything else.
func parseDur(s string) (int, bool) {
	s = strings.ToLower(strings.Join(strings.Fields(s), ""))
	if s == "" {
		return 0, false
	}
	m := durRe.FindStringSubmatch(s)
	plain := m == nil && strings.Trim(s, "0123456789") == ""
	if m == nil && !plain {
		return 0, false
	}
	if plain {
		m = []string{s, "", "", s}
	}
	if m[1] == "" && m[2] == "" && m[3] == "" {
		return 0, false
	}
	total := 0
	for i, mul := range []int{3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n > 10*maxTimeSecs {
			return 10 * maxTimeSecs, true // too long; callers check the limit
		}
		total += n * mul
	}
	return total, true
}

// fmtDur writes seconds for log lines: 1h 5m 20s, 45s, 0s.
func fmtDur(secs int) string {
	if secs <= 0 {
		return "0s"
	}
	var parts []string
	if h := secs / 3600; h > 0 {
		parts = append(parts, strconv.Itoa(h)+"h")
	}
	if m := secs % 3600 / 60; m > 0 {
		parts = append(parts, strconv.Itoa(m)+"m")
	}
	if s := secs % 60; s > 0 {
		parts = append(parts, strconv.Itoa(s)+"s")
	}
	return strings.Join(parts, " ")
}

// Step is one row of a flow, stored as JSON in the flows table. Which fields
// are used depends on Type:
//
//	message     To (all | player), Text
//	playerList  To: sends "Online (N): name, name, ..."
//	wait        Time ("30", "1m20s")
//	lock, unlock, shutdown
//	kick        Who (player), Reason
//	ban         Who (player), Len (minutes, 0 = permanent), Reason
//	kickAll     Reason
//	command     Cmd: any RCON command
//	stop        ends the run
//	if          Branches (the If, then any Else ifs) and Else. Each branch,
//	            and the Else (ElseCounter), can change the player's counter
//	            when it runs (see Branch.Counter)
//	forEach     Of (player | message) and Steps. player: the steps run once
//	            for every player online, as "the player", with {message} their
//	            latest chat line this session. message: once for every chat
//	            line players online sent this session that this flow hasn't
//	            handled yet, as the player who sent it, with {message} the
//	            line. Ifs inside decide who or what qualifies.
type Step struct {
	Type     string   `json:"type"`
	To       string   `json:"to,omitempty"`
	Text     string   `json:"text,omitempty"`
	Time     string   `json:"time,omitempty"`
	Who      string   `json:"who,omitempty"`
	Len      string   `json:"len,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Cmd      string   `json:"cmd,omitempty"`
	Branches []Branch `json:"branches,omitempty"`
	Else     *[]Step  `json:"else,omitempty"`  // nil: no Else
	Steps    []Step   `json:"steps,omitempty"` // forEach: the steps run for each player or message
	Of       string   `json:"of,omitempty"`    // forEach: "player" or "message"
	// ElseCounter is what the Else does to the player's counter, like Branch.Counter.
	ElseCounter string `json:"elseCounter,omitempty"`
}

// Branch is the If or one Else if: a condition and the steps it runs. Every
// branch of one If compares the same Subject; only Op and Value differ.
type Branch struct {
	Subject string `json:"subject"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Steps   []Step `json:"steps"`
	// Counter is what this branch does to the player's counter for this
	// run when it is the one that runs, before its steps: "" nothing,
	// "inc" +1, "dec" -1 (not below 0) or "reset" back to 0.
	Counter string `json:"counter,omitempty"`
}

// Counter changes an If branch or Else can make (Branch.Counter).
var counterOps = map[string]string{"": "", "inc": "++", "dec": "--", "reset": "reset"}

// maxCounterThreshold is the highest flow counter threshold. 0 means the
// flow has none, and then {threshold} can't be used.
const maxCounterThreshold = 1000

// counterThresholdToken is the one placeholder a number If may compare to.
const counterThresholdToken = "{threshold}"

// What a For each goes through.
const (
	forEachPlayer  = "player"
	forEachMessage = "message"
)

// isBlock reports whether a step holds other steps (If, For each).
func isBlock(s *Step) bool { return s.Type == "if" || s.Type == "forEach" }

// branchLists is a block's step lists in order: an If's branches, then its
// Else; a For each player's one list.
func branchLists(s *Step) [][]Step {
	if s.Type == "forEach" {
		return [][]Step{s.Steps}
	}
	out := make([][]Step, 0, len(s.Branches)+1)
	for _, b := range s.Branches {
		out = append(out, b.Steps)
	}
	if s.Else != nil {
		out = append(out, *s.Else)
	}
	return out
}

// normalizeFlowSteps gives every branch and Else a non-nil list, so the page always
// gets [] rather than null, and trims the text fields.
func normalizeFlowSteps(list []Step) []Step {
	if list == nil {
		return []Step{}
	}
	for i := range list {
		s := &list[i]
		s.Type = strings.TrimSpace(s.Type)
		s.To, s.Who, s.Len = strings.TrimSpace(s.To), strings.TrimSpace(s.Who), strings.TrimSpace(s.Len)
		s.Text, s.Reason, s.Cmd = battleye.CleanReason(s.Text), battleye.CleanReason(s.Reason), battleye.CleanReason(s.Cmd)
		s.Time = strings.TrimSpace(s.Time)
		for j := range s.Branches {
			b := &s.Branches[j]
			b.Subject, b.Op, b.Value = strings.TrimSpace(b.Subject), strings.TrimSpace(b.Op), battleye.CleanReason(b.Value)
			b.Counter = strings.TrimSpace(b.Counter)
			b.Steps = normalizeFlowSteps(b.Steps)
		}
		if s.Else != nil {
			e := normalizeFlowSteps(*s.Else)
			s.Else = &e
		} else {
			s.ElseCounter = ""
		}
		s.ElseCounter = strings.TrimSpace(s.ElseCounter)
		if s.Type == "forEach" {
			s.Steps = normalizeFlowSteps(s.Steps)
			if s.Of = strings.TrimSpace(s.Of); s.Of == "" {
				s.Of = forEachPlayer
			}
		}
	}
	return list
}

// countAllSteps counts every step, inside Ifs and For each player too.
func countAllSteps(list []Step) int {
	n := 0
	for i := range list {
		n++
		if isBlock(&list[i]) {
			for _, l := range branchLists(&list[i]) {
				n += countAllSteps(l)
			}
		}
	}
	return n
}

// flowRunLength is how long the waits take, counting the longest branch of each
// If, and a For each player's waits once (they repeat for every player).
func flowRunLength(list []Step) int {
	total := 0
	for i := range list {
		s := &list[i]
		switch s.Type {
		case "wait":
			secs, _ := parseDur(s.Time)
			total += secs
		case "if":
			longest := 0
			for _, l := range branchLists(s) {
				if n := flowRunLength(l); n > longest {
					longest = n
				}
			}
			total += longest
		case "forEach":
			total += flowRunLength(s.Steps)
		}
	}
	return total
}

// database is the player database, set at startup (SetDatabase).
var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }
