package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "time/tzdata" // lets the TZ env var work in minimal container images
)

// ============================================================================
// flows.go overview
// ----------------------------------------------------------------------------
// A "flow" is a named, saved script of RCON steps timed around a "0 point".
// Every step has an offset from that point, so steps can run before it, on
// it, or after it.
//
// Script syntax, one step per line:  <offset> | <command>
//
//	-60m | say -1 Server is going to restart in $minutes minutes!
//	-3m  | #lock
//	0m   | #shutdown
//	+90  | #unlock
//	cancel | say -1 Server restart cancelled.
//	// comment
//
//	-60m   60 minutes BEFORE the 0 point          +90    90 seconds AFTER it
//	0m     exactly on the 0 point                 units: s, m, h (none = seconds)
//	cancel runs only if the flow is stopped before the 0 point arrives
//
// A step command can also be "kickall [reason]" (or "#kickall"), which kicks every connected
// player (the same sweep as POST /kickall); the reason defaults to
// "Server restarting".
//
// $minutes / $seconds in a command become how long before the 0 point that
// step runs (0 for steps on or after it).
//
// Triggers. Every flow has exactly one trigger, chosen in the edit dialog.
// It is stored in the flow's schedule column (no separate field):
//
//	schedule column     trigger
//	"manual"            manual     (blank or "* * * * *" also read as manual)
//	"on-join"           join
//	a cron expression   schedule
//
//	manual    Runs only when someone presses Run (or POSTs /api/flows/<id>/run).
//	          There is nothing to enable.
//	schedule  Runs on a standard 5-field cron expression (minute hour
//	          day-of-month month day-of-week) in the server's local time, so
//	          set TZ in the container. Supports * , - / lists, month/day names
//	          and @hourly @daily @weekly @monthly @yearly. The cron time is the
//	          0 point. Only runs while Enabled. A blank or "* * * * *" schedule
//	          is rejected: that means "no schedule", which is the manual trigger.
//	join      While Enabled, runs once for every player who finishes joining,
//	          with offsets counted from the moment the join was seen (so no
//	          negative offsets). $player becomes the player's number and $name
//	          their name, e.g.  0s | say $player Welcome to our server, $name!
//	          Joins are spotted by the background poller (db.go) every 5
//	          seconds; a player listed with "(Lobby)" after their name is still
//	          loading in and counts as joined once the tag is gone.
//
// Every flow can also be started by hand with Run, except one that uses
// $player or $name (those only make sense for a joining player).
//
// This file holds the flow logic only. Storage is in db.go, the HTTP API and
// the /flows page are in web.go, and commands go out through rcon.go.
//
// Runs live in memory: restarting this program abandons a run in progress
// (a scheduled run that was mid-countdown is picked up again, see scheduler).
// ============================================================================

// ---------------------------------------------------------------------------
// Script parsing
// ---------------------------------------------------------------------------

type flowStep struct {
	OffsetSecs int // negative = before the 0 point, positive = after
	Command    string
}

type parsedFlow struct {
	Steps    []flowStep // sorted by offset, earliest first (script order kept for ties)
	OnCancel []string
}

var offsetRe = regexp.MustCompile(`^([+-]?)(\d+)([smh]?)$`)

const maxOffsetSecs = 7 * 86400

func parseOffset(when string) (int, error) {
	when = strings.ToLower(strings.ReplaceAll(when, " ", ""))
	m := offsetRe.FindStringSubmatch(when)
	if m == nil {
		return 0, fmt.Errorf("bad time %q (examples: -60m, 0m, +90, +2h)", when)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n > maxOffsetSecs {
		return 0, fmt.Errorf("time %q is too large", when)
	}
	switch m[3] {
	case "m":
		n *= 60
	case "h":
		n *= 3600
	}
	if n > maxOffsetSecs {
		return 0, fmt.Errorf("time %q is too large (max 7 days)", when)
	}
	switch m[1] {
	case "-":
		return -n, nil
	case "+":
		return n, nil
	}
	if n == 0 {
		return 0, nil
	}
	return 0, fmt.Errorf("time %q needs a sign: -%s runs before the 0 point, +%s after it", when, m[2]+m[3], m[2]+m[3])
}

func parseFlowScript(script string) (*parsedFlow, error) {
	pf := &parsedFlow{}
	for i, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		when, cmd, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"<offset> | <command>\"", i+1)
		}
		cmd = cleanReason(cmd)
		if cmd == "" {
			return nil, fmt.Errorf("line %d: command is empty", i+1)
		}
		when = strings.ToLower(strings.TrimSpace(when))
		if when == "cancel" {
			pf.OnCancel = append(pf.OnCancel, cmd)
			continue
		}
		off, err := parseOffset(when)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		pf.Steps = append(pf.Steps, flowStep{OffsetSecs: off, Command: cmd})
	}
	if len(pf.Steps) == 0 {
		return nil, errors.New("flow needs at least one step like \"0m | say -1 hello\"")
	}
	sort.SliceStable(pf.Steps, func(a, b int) bool { return pf.Steps[a].OffsetSecs < pf.Steps[b].OffsetSecs })
	return pf, nil
}

func fmtDur(secs int) string {
	unit, n := "second", secs
	switch {
	case secs%3600 == 0:
		unit, n = "hour", secs/3600
	case secs%60 == 0:
		unit, n = "minute", secs/60
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

func relText(o int) string {
	switch {
	case o == 0:
		return "the scheduled time"
	case o < 0:
		return fmtDur(-o) + " before"
	}
	return fmtDur(o) + " after"
}

// span describes when the flow's steps run relative to the cron time.
func (pf *parsedFlow) span() string {
	first, last := pf.Steps[0].OffsetSecs, pf.Steps[len(pf.Steps)-1].OffsetSecs
	if first == last {
		if first == 0 {
			return "All steps run at the scheduled time."
		}
		return "All steps run " + relText(first) + " the scheduled time."
	}
	switch {
	case first == 0:
		return "Steps run from the scheduled time to " + relText(last) + " it."
	case last == 0:
		return "Steps run from " + relText(first) + " to the scheduled time."
	}
	return "Steps run from " + relText(first) + " to " + relText(last) + " the scheduled time."
}

// totalSpan describes how long a manual run takes from the moment Run is
// pressed (the earliest step fires right away).
func (pf *parsedFlow) totalSpan() string {
	first, last := pf.Steps[0].OffsetSecs, pf.Steps[len(pf.Steps)-1].OffsetSecs
	if first == last {
		return "All steps run as soon as you press Run."
	}
	return "The first step runs as soon as you press Run; the last one " + fmtDur(last-first) + " later."
}

// ---------------------------------------------------------------------------
// Cron
// ---------------------------------------------------------------------------

type cronExpr struct {
	min, hour, dom, mon, dow []bool // allowed values, indexed by value
	domStar, dowStar         bool   // field started with "*" (affects day matching)
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dowNames    = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	cronAliases = map[string]string{
		"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
		"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
	}
)

func cronNum(tok string, names map[string]int) (int, error) {
	if v, ok := names[tok]; ok {
		return v, nil
	}
	return strconv.Atoi(tok)
}

func parseCronField(field string, lo, hi int, names map[string]int, label string) ([]bool, bool, error) {
	set := make([]bool, hi+1)
	star := strings.HasPrefix(field, "*")
	for _, part := range strings.Split(field, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, false, fmt.Errorf("%s: bad step %q", label, stepStr)
			}
			step = n
		}
		var a, b int
		if rng == "*" {
			a, b = lo, hi
		} else {
			from, to, isRange := strings.Cut(rng, "-")
			x, err := cronNum(from, names)
			if err != nil {
				return nil, false, fmt.Errorf("%s: bad value %q", label, from)
			}
			a, b = x, x
			if isRange {
				y, err := cronNum(to, names)
				if err != nil {
					return nil, false, fmt.Errorf("%s: bad value %q", label, to)
				}
				b = y
			} else if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return nil, false, fmt.Errorf("%s: %q is out of range (%d-%d)", label, part, lo, hi)
		}
		for v := a; v <= b; v += step {
			set[v] = true
		}
	}
	return set, star, nil
}

func parseCron(expr string) (*cronExpr, error) {
	expr = strings.ToLower(strings.TrimSpace(expr))
	if a, ok := cronAliases[expr]; ok {
		expr = a
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, errors.New("expected 5 fields: minute hour day-of-month month day-of-week (e.g. 0 6 * * *)")
	}
	c := &cronExpr{}
	var err error
	if c.min, _, err = parseCronField(f[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if c.hour, _, err = parseCronField(f[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	if c.dom, c.domStar, err = parseCronField(f[2], 1, 31, nil, "day-of-month"); err != nil {
		return nil, err
	}
	if c.mon, _, err = parseCronField(f[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	if c.dow, c.dowStar, err = parseCronField(f[4], 0, 7, dowNames, "day-of-week"); err != nil {
		return nil, err
	}
	if c.dow[7] { // 7 is also Sunday
		c.dow[0] = true
	}
	return c, nil
}

// dayMatches follows standard cron: if both day-of-month and day-of-week are
// restricted a day matches when EITHER does; otherwise both must match.
func (c *cronExpr) dayMatches(t time.Time) bool {
	if !c.mon[int(t.Month())] {
		return false
	}
	domOK, dowOK := c.dom[t.Day()], c.dow[int(t.Weekday())]
	if c.domStar || c.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// next returns the first run time strictly after `after` (server local time).
func (c *cronExpr) next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	for d := 0; d <= 366*8; d++ {
		day := time.Date(after.Year(), after.Month(), after.Day()+d, 0, 0, 0, 0, loc)
		if !c.dayMatches(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if !c.hour[h] {
				continue
			}
			for m := 0; m < 60; m++ {
				if !c.min[m] {
					continue
				}
				cand := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
				if cand.After(after) {
					return cand, true
				}
			}
		}
	}
	return time.Time{}, false
}

func cronValues(set []bool, lo, hi int) []int {
	out := []int{}
	for v := lo; v <= hi && v < len(set); v++ {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func intStrings(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return out
}

func describeMinutes(mins []int) string {
	if len(mins) == 1 {
		if mins[0] == 0 {
			return "Every hour, on the hour"
		}
		return fmt.Sprintf("At %d minutes past every hour", mins[0])
	}
	if mins[0] == 0 && mins[1] > 0 && 60%mins[1] == 0 && len(mins) == 60/mins[1] {
		even := true
		for i, m := range mins {
			if m != i*mins[1] {
				even = false
			}
		}
		if even {
			return fmt.Sprintf("Every %d minutes", mins[1])
		}
	}
	return "At minutes " + joinAnd(intStrings(mins)) + " past every hour"
}

var (
	longDays   = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	longMonths = []string{"", "January", "February", "March", "April", "May", "June", "July",
		"August", "September", "October", "November", "December"}
)

// describe returns a plain-English sentence for the schedule.
func (c *cronExpr) describe() string {
	mins := cronValues(c.min, 0, 59)
	hrs := cronValues(c.hour, 0, 23)
	var when string
	switch {
	case len(mins) == 60 && len(hrs) == 24:
		when = "Every minute"
	case len(hrs) == 24:
		when = describeMinutes(mins)
	case len(mins) == 60:
		hs := make([]string, len(hrs))
		for i, h := range hrs {
			hs[i] = fmt.Sprintf("%02d:00", h)
		}
		when = "Every minute during the " + joinAnd(hs) + " hour"
		if len(hs) > 1 {
			when += "s"
		}
	case len(hrs)*len(mins) <= 8:
		ts := []string{}
		for _, h := range hrs {
			for _, m := range mins {
				ts = append(ts, fmt.Sprintf("%02d:%02d", h, m))
			}
		}
		when = "At " + joinAnd(ts)
	default:
		when = "At minute " + joinAnd(intStrings(mins)) + " past hour " + joinAnd(intStrings(hrs))
	}

	dom := cronValues(c.dom, 1, 31)
	dow := cronValues(c.dow, 0, 6)
	months := cronValues(c.mon, 1, 12)
	domAll, dowAll := len(dom) == 31, len(dow) == 7
	dowPhrase := func() string {
		key := strings.Join(intStrings(dow), ",")
		switch key {
		case "1,2,3,4,5":
			return "on weekdays"
		case "0,6":
			return "on weekends"
		}
		names := make([]string, len(dow))
		for i, d := range dow {
			names[i] = longDays[d]
		}
		return "on " + joinAnd(names)
	}
	domPhrase := "on day " + joinAnd(intStrings(dom)) + " of the month"
	var day string
	switch {
	case domAll && dowAll:
		day = ""
	case dowAll:
		day = domPhrase
	case domAll:
		day = dowPhrase()
	case c.domStar || c.dowStar:
		day = domPhrase + ", only when it falls " + dowPhrase()
	default:
		day = domPhrase + " and " + dowPhrase()
	}
	if len(months) != 12 {
		ms := make([]string, len(months))
		for i, m := range months {
			ms[i] = longMonths[m]
		}
		if day == "" {
			day = "every day"
		}
		day += ", in " + joinAnd(ms)
	}
	switch {
	case day != "":
		return when + ", " + day
	case strings.HasPrefix(when, "Every"):
		return when
	}
	return when + ", every day"
}

func fmtWhen(t time.Time) string { return t.Format("Mon 02 Jan 15:04 MST") }

// ---------------------------------------------------------------------------
// Flow definitions (stored in the flows table, see db.go)
// ---------------------------------------------------------------------------

// Flow triggers: how a flow starts besides the Run button.
const (
	triggerManual   = "manual"   // only Run starts it
	triggerSchedule = "schedule" // cron schedule, while Enabled
	triggerJoin     = "join"     // once per joining player, while Enabled
)

// What the schedule column holds for the two triggers that have no cron
// expression.
const (
	scheduleManual = "manual"
	scheduleJoin   = "on-join"
)

type Flow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Script string `json:"script"`
	// Schedule is what's stored: "manual", "on-join" or a cron expression.
	Schedule string `json:"schedule"`
	// Trigger (manual | schedule | join) is derived from Schedule when a flow
	// is read and is not stored. The edit dialog sends it on save; if it's
	// empty, Schedule decides.
	Trigger string `json:"trigger"`
	// ScheduleEnabled turns the trigger on (schedule and join flows). Manual
	// flows are always off.
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	NextRun         string `json:"nextRun,omitempty"` // computed for the UI, not stored
	LastRun         string `json:"lastRun,omitempty"` // when it was last started, for the UI, not stored
}

// maxFlowNameLen is the longest flow name allowed, in characters. The Flow
// column on the page (web.go) is sized to fit it.
const maxFlowNameLen = 48

// clipName shortens a name to at most n characters (not bytes), dropping
// any space left at the cut.
func clipName(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// triggerOf reads a flow's trigger from its stored schedule. Blank and
// "* * * * *" (what older flows used for "no schedule") count as manual.
func triggerOf(schedule string) string {
	s := strings.ToLower(strings.Join(strings.Fields(schedule), " "))
	switch s {
	case scheduleManual, "", "* * * * *":
		return triggerManual
	case scheduleJoin:
		return triggerJoin
	}
	return triggerSchedule
}

// fillTrigger sets f.Trigger from f.Schedule after a flow is read from the
// database. A manual flow is never reported as enabled.
func fillTrigger(f *Flow) {
	f.Trigger = triggerOf(f.Schedule)
	if f.Trigger == triggerManual {
		f.ScheduleEnabled = false
	}
}

// storedSchedule is what goes in the schedule column for a validated flow.
func storedSchedule(f Flow) string {
	switch f.Trigger {
	case triggerJoin:
		return scheduleJoin
	case triggerSchedule:
		return f.Schedule
	}
	return scheduleManual
}

const defaultRestartScript = `// Server restart. Offsets are relative to the cron time:
// -60m = 60 minutes before, 0s = on the cron time, +90s = 90 seconds after.
-60m | say -1 Server is going to restart in $minutes minutes!
-30m | say -1 Server is going to restart in $minutes minutes!
-15m | say -1 Server is going to restart in $minutes minutes!
-10m | say -1 Server is going to restart in $minutes minutes!
-5m  | say -1 Server is going to restart in $minutes minutes!
-3m  | say -1 Server is going to restart in $minutes minutes!
-2m  | #lock
-1m  | #kickall Server Restart
0s   | #shutdown
+90s | #unlock
cancel | say -1 Server restart cancelled.
`

// initFlows creates the table and seeds an example restart flow on first run.
func initFlows() error {
	return globalDatabase.EnsureFlowsTable("Server restart", defaultRestartScript, "0 6 * * *")
}

// normalizeTrigger turns what the dialog or API sent into one of the trigger
// constants. Empty means manual.
func normalizeTrigger(t string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "manual":
		return triggerManual, nil
	case "schedule", "cron":
		return triggerSchedule, nil
	case "join", "on-join", "playerjoin", "player-join", "on player join":
		return triggerJoin, nil
	}
	return "", fmt.Errorf("unknown trigger %q (use manual, schedule or join)", t)
}

// cronExamples follows every schedule error, so the dialog shows what a
// valid schedule looks like (the same examples as the help page).
const cronExamples = `Schedule: Enter a cron expression such as:
0 6 * * *  - every day at 06:00
0 */6 * * *  - every 6 hours (00:00, 06:00, 12:00, 18:00)
30 4 * * 1,4  - Mondays and Thursdays at 04:30
0 5 * * 1-5  - weekdays at 05:00`

// checkCronSchedule validates the schedule of a schedule-triggered flow.
// Anything triggerOf wouldn't read back as a cron schedule (blank,
// "* * * * *", "manual", "on-join") is rejected.
func checkCronSchedule(schedule string) error {
	if triggerOf(schedule) != triggerSchedule {
		return errors.New(cronExamples)
	}
	if _, err := parseCron(schedule); err != nil {
		return errors.New("Schedule: " + err.Error() + ".\n" + strings.TrimPrefix(cronExamples, "Schedule: "))
	}
	return nil
}

// playerVarRe finds the per-player variables only join flows can fill in.
var playerVarRe = regexp.MustCompile(`\$(player|name)\b`)

// usesPlayerVars reports whether a script uses $player or $name.
func usesPlayerVars(script string) bool { return playerVarRe.MatchString(script) }

const errPlayerVarsText = "$player and $name only work in flows triggered On player join"

// checkJoinSteps rejects steps a join flow can never run: the flow starts
// when the player is seen joining, so nothing can happen before that.
func checkJoinSteps(pf *parsedFlow) error {
	if pf.Steps[0].OffsetSecs < 0 {
		return errors.New("player-join flows start when the player joins, so steps can't have a - offset (use 0s or +)")
	}
	return nil
}

// checkTrigger checks that a flow's script and schedule fit its trigger.
// f.Trigger must already be normalized. Errors are user-facing messages.
func checkTrigger(f Flow, pf *parsedFlow) error {
	switch f.Trigger {
	case triggerSchedule:
		if err := checkCronSchedule(f.Schedule); err != nil {
			return err
		}
	case triggerJoin:
		return checkJoinSteps(pf)
	}
	if usesPlayerVars(f.Script) {
		return errors.New(errPlayerVarsText)
	}
	return nil
}

// flowsWithNextRun lists every flow and fills NextRun for the UI.
func flowsWithNextRun() ([]Flow, error) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	flowRuns.Lock()
	for i := range flows {
		if t, ok := flowRuns.last[flows[i].ID]; ok {
			flows[i].LastRun = fmtWhen(t)
		}
	}
	flowRuns.Unlock()
	for i := range flows {
		f := &flows[i]
		switch f.Trigger {
		case triggerManual:
			f.NextRun = "Manual only"
		case triggerJoin:
			if f.ScheduleEnabled {
				f.NextRun = "On player join"
			}
		case triggerSchedule:
			if !f.ScheduleEnabled {
				continue
			}
			if c, err := parseCron(f.Schedule); err == nil {
				if next, ok := c.next(now); ok {
					f.NextRun = fmtWhen(next)
				}
			}
		}
	}
	return flows, nil
}

// validateAndSaveFlow checks a flow from the edit dialog (or an import) and
// stores it. Every error it returns is a user-facing message (the API sends
// them as 400).
//
// New flows are always saved disabled, manual flows are never enabled, and
// an edit that changes a flow's trigger saves it disabled, so switching an
// enabled restart flow to "On player join" can never make it fire on every
// join without someone pressing Enable.
func validateAndSaveFlow(f Flow) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if utf8.RuneCountInString(f.Name) > maxFlowNameLen {
		return 0, fmt.Errorf("Name: %d characters max", maxFlowNameLen)
	}
	// The dialog sends a trigger; an import (or a bare API call) may only
	// have a schedule, in which case the schedule says what it is.
	if strings.TrimSpace(f.Trigger) == "" {
		f.Trigger = triggerOf(f.Schedule)
	}
	trig, err := normalizeTrigger(f.Trigger)
	if err != nil {
		return 0, err
	}
	f.Trigger = trig
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return 0, err
	}
	if err := checkTrigger(f, pf); err != nil {
		return 0, err
	}
	f.Schedule = storedSchedule(f)
	switch {
	case f.Trigger == triggerManual, f.ID == 0:
		f.ScheduleEnabled = false
	default:
		if old, err := globalDatabase.GetFlow(f.ID); err == nil && old.Trigger != f.Trigger {
			f.ScheduleEnabled = false
		}
	}
	id, err := globalDatabase.SaveFlow(f)
	if err != nil {
		return 0, errors.New("could not save (is the name already used?)")
	}
	return id, nil
}

// checkFlowCanEnable reports why a flow can't be enabled, if it can't.
// Errors are user-facing messages.
func checkFlowCanEnable(f Flow) error {
	if f.Trigger == triggerManual {
		return errors.New("manual flows have nothing to enable - press Run to start one, or edit the flow and pick the Schedule or On player join trigger")
	}
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return err
	}
	return checkTrigger(f, pf)
}

// previewFlow builds the plain-English summary the edit dialog shows under
// the trigger: what the trigger and schedule mean, the next few run times,
// when the next countdown starts, and how the script's offsets fit around it.
// The result is sent to the browser as-is.
func previewFlow(trigger, schedule, script string) map[string]any {
	trig, err := normalizeTrigger(trigger)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	resp := map[string]any{"ok": true}
	pf, perr := parseFlowScript(script)
	playerNote := "Note: " + errPlayerVarsText + "."

	switch trig {
	case triggerManual:
		resp["description"] = "Manual only - this flow runs when you press Run (or POST /api/flows/<id>/run). There is nothing to enable."
		if perr == nil {
			resp["span"] = pf.totalSpan()
			if usesPlayerVars(script) {
				resp["span"] = playerNote
			}
		}
		return resp

	case triggerJoin:
		resp["description"] = "While Enabled, this flow runs once for each player who finishes joining (once the (Lobby) tag is gone from their name). $player = their number, $name = their name."
		if perr == nil {
			if err := checkJoinSteps(pf); err != nil {
				resp["span"] = "Note: " + err.Error() + "."
			} else {
				resp["span"] = strings.Replace(pf.span(), "the scheduled time", "the moment the player joins", -1)
			}
		}
		return resp
	}

	// Schedule trigger.
	if perr == nil {
		resp["span"] = pf.span()
		if usesPlayerVars(script) {
			resp["span"] = playerNote
		}
	}
	if err := checkCronSchedule(schedule); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	c, _ := parseCron(schedule)
	resp["description"] = c.describe()
	next := []string{}
	t := time.Now()
	for i := 0; i < 3; i++ {
		n, ok := c.next(t)
		if !ok {
			break
		}
		if i == 0 && perr == nil && pf.Steps[0].OffsetSecs < 0 {
			resp["starts"] = fmtWhen(n.Add(time.Duration(pf.Steps[0].OffsetSecs) * time.Second))
		}
		next = append(next, fmtWhen(n))
		t = n
	}
	if len(next) == 0 {
		resp["description"] = c.describe() + " (this never happens)"
	}
	resp["next"] = next
	return resp
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// flowRun is the live (or last finished) run of one flow. Mutable fields are
// guarded by flowRuns' mutex; anchor only moves later, when a late step
// pushes the rest of the run back (see stepClock).
type flowRun struct {
	flowID int64
	name   string
	status string // running | done | stopped | error
	active bool
	failed bool      // a command failed (no reply or RCON error); the run ends as "error"
	anchor time.Time // the 0 point this run is built around
	log    []string
	cancel context.CancelFunc
	ctx    context.Context // player-join runs only: shared by all players' jobs
	jobs   int             // player-join runs only: players still being worked through
}

// flowRuns holds each flow's current or last run, and when each flow was
// last started (for a join flow, the last player join it ran for). Both live
// in memory only, so they reset when this program restarts.
var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
	last   map[int64]time.Time
}{byFlow: map[int64]*flowRun{}, last: map[int64]time.Time{}}

var (
	errFlowRunning    = errors.New("flow is already running")
	errFlowNotRunning = errors.New("flow is not running")
	errFlowBusy       = errors.New("stop the running flow first")
)

func (r *flowRun) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("flow[%s] %s", r.name, msg)
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	flowRuns.Unlock()
}

func (r *flowRun) set(status string, active bool) {
	flowRuns.Lock()
	r.status, r.active = status, active
	flowRuns.Unlock()
}

// markFailed records that a command in this run failed.
func (r *flowRun) markFailed() {
	flowRuns.Lock()
	r.failed = true
	flowRuns.Unlock()
}

// finish ends a run that wasn't stopped: "error" if any command failed,
// otherwise "done" (shown as Idle on the page).
func (r *flowRun) finish() {
	flowRuns.Lock()
	r.status, r.active = "done", false
	if r.failed {
		r.status = "error"
	}
	flowRuns.Unlock()
}

type runView struct {
	FlowID     int64    `json:"flowId"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Active     bool     `json:"active"`
	AnchorUnix int64    `json:"anchorUnix"`
	Log        []string `json:"log"`
}

func snapshotRuns() []runView {
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

// startFlow starts a manual run (the Run button), whatever the flow's
// trigger. With minutes <= 0 the 0 point is placed so the earliest step fires
// immediately; otherwise the 0 point is `minutes` from now and any step that
// would already be in the past is skipped (e.g. 10 -> the -60m/-30m/-15m
// steps are skipped).
func startFlow(f Flow, minutes int) error {
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return err
	}
	if usesPlayerVars(f.Script) {
		return errors.New("this flow uses $player or $name, so it only runs when a player joins")
	}
	lead := 0
	if minutes > 0 {
		lead = minutes * 60
	} else if first := pf.Steps[0].OffsetSecs; first < 0 {
		lead = -first
	}
	return launchFlow(f, pf, time.Now().Add(time.Duration(lead)*time.Second), sourceManual)
}

// The log line each kind of run starts with is "Started by <source>".
const (
	sourceManual   = "manual run"
	sourceSchedule = "schedule"
	sourceJoin     = "player join trigger"
)

// stopFlow cancels the active run of a flow (the Stop button). Its cancel
// steps run if the 0 point hasn't been reached yet (see executeFlow).
func stopFlow(id int64) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[id]
	active := run != nil && run.active
	flowRuns.Unlock()
	if !active {
		return errFlowNotRunning
	}
	run.cancel()
	return nil
}

// deleteFlow forgets a flow's last run and removes it from the database.
// A running flow can't be deleted (errFlowBusy).
func deleteFlow(id int64) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[id]
	if run != nil && run.active {
		flowRuns.Unlock()
		return errFlowBusy
	}
	delete(flowRuns.byFlow, id)
	delete(flowRuns.last, id)
	flowRuns.Unlock()
	return globalDatabase.DeleteFlow(id)
}

// launchFlow starts the run goroutine for an already-parsed flow around the
// given 0 point.
func launchFlow(f Flow, pf *parsedFlow, anchor time.Time, source string) error {
	flowRuns.Lock()
	if old := flowRuns.byFlow[f.ID]; old != nil && old.active {
		flowRuns.Unlock()
		return errFlowRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true, anchor: anchor, cancel: cancel}
	flowRuns.byFlow[f.ID] = run
	// Last run is when the first step is due: a scheduled run is started up
	// to one tick early (see launchDue) and sleeps until then.
	last := time.Now()
	if firstAt := anchor.Add(time.Duration(pf.Steps[0].OffsetSecs) * time.Second); firstAt.After(last) {
		last = firstAt
	}
	flowRuns.last[f.ID] = last
	flowRuns.Unlock()
	go executeFlow(ctx, run, pf, source)
	return nil
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

// runFlowCommand sends one step's command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
//
// A command that gets an RCON error, or no reply (except #shutdown), marks
// the run as failed; the remaining steps still run, and the run ends with
// status "error".
//
// p is the joining player for player-join flows (fills $player and $name),
// nil for scheduled and manual runs.
func runFlowCommand(run *flowRun, command string, offsetSecs int, p *Player) {
	before := 0
	if offsetSecs < 0 {
		before = -offsetSecs
	}
	vars := []string{"$minutes", strconv.Itoa(before / 60), "$seconds", strconv.Itoa(before)}
	if p != nil {
		vars = append(vars, "$player", strconv.Itoa(p.ID), "$name", p.Name)
	}
	cmd := strings.NewReplacer(vars...).Replace(command)
	cmd = cleanReason(cmd)
	lower := strings.ToLower(cmd)
	// kickall is this program's own command, not a BattlEye one: it lists the
	// players and kicks each (see kickAllUsing in rcon.go). Usage: kickall [reason]
	// Both "kickall" and "#kickall" are accepted.
	bare := strings.TrimPrefix(cmd, "#")
	lowerBare := strings.ToLower(bare)
	if lowerBare == "kickall" || strings.HasPrefix(lowerBare, "kickall ") {
		reason := strings.TrimSpace(bare[len("kickall"):])
		if reason == "" {
			reason = "Server restarting"
		}
		rep, err := kickAllUsing(sharedRunner{}, reason, false, false)
		if err != nil {
			run.logf("kickall failed: %v", err)
			run.markFailed()
			return
		}
		run.logf("kickall %q: kicked %v, no ack for %v, still connected %v", reason, rep.Order, rep.Unacked, rep.Remaining)
		return
	}
	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := runShared(cmd, 2*time.Second, retry)
	run.logf("sent %q (acked=%v err=%v)", cmd, acked, err)
	if err != nil || (!acked && lower != "#shutdown") {
		run.markFailed()
	}
}

// stepClock times a run's steps from its 0 point while keeping the spacing
// the script asks for: when a step goes out late (a late start, a slow RCON
// reply, a long kickall), the 0 point moves back by the same amount, so every
// later step still comes its full offset gap after the one before it.
type stepClock struct{ base time.Time }

// stepSlack is how late a step can be before later steps are moved back.
const stepSlack = 250 * time.Millisecond

func (c *stepClock) at(offsetSecs int) time.Time {
	return c.base.Add(time.Duration(offsetSecs) * time.Second)
}

// fired is called as a step goes out and returns how far the 0 point moved.
func (c *stepClock) fired(offsetSecs int) time.Duration {
	late := time.Since(c.at(offsetSecs))
	if late <= stepSlack {
		return 0
	}
	c.base = c.base.Add(late)
	return late
}

func executeFlow(ctx context.Context, run *flowRun, pf *parsedFlow, source string) {
	defer run.cancel()
	started := time.Now()
	run.logf("Started by %s", source)
	clock := &stepClock{base: run.anchor}
	for _, st := range pf.Steps {
		fireAt := clock.at(st.OffsetSecs)
		if fireAt.Before(started.Add(-scheduleGrace)) {
			run.logf("skipped %q (its time had already passed)", st.Command)
			continue
		}
		if !sleepUntil(ctx, fireAt) {
			run.logf("stopped")
			// Cancel commands undo a countdown that never reached its 0 point.
			if time.Now().Before(clock.base) {
				for _, c := range pf.OnCancel {
					runFlowCommand(run, c, 0, nil)
				}
			}
			run.set("stopped", false)
			return
		}
		if late := clock.fired(st.OffsetSecs); late > 0 {
			run.logf("%q is %.1fs late; later steps move back %.1fs to keep their spacing", st.Command, late.Seconds(), late.Seconds())
			flowRuns.Lock()
			run.anchor = clock.base // keeps the page's countdown right
			flowRuns.Unlock()
		}
		runFlowCommand(run, st.Command, st.OffsetSecs, nil)
	}
	run.logf("done")
	run.finish()
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// scheduleGrace lets a step whose time just passed (a late tick, a restart a
// few seconds into a window) still run instead of being skipped.
const scheduleGrace = 15 * time.Second

// scheduleFired remembers which occurrences were already started so a finished
// or stopped run isn't started again inside the same window. Only touched by
// the scheduler goroutine.
var scheduleFired = map[string]time.Time{}

// scheduleTick is how often the scheduler checks for runs to start.
const scheduleTick = 5 * time.Second

// launchDue reports whether a run whose first step is due at firstAt should
// be started at this tick. It looks one tick ahead: a run due before the next
// tick is started now and its goroutine sleeps until the exact second
// (sleepUntil). Without the look-ahead the first step ran up to one tick (5s)
// late, and any step due in that gap fired at the same moment as it.
func launchDue(now, firstAt time.Time) bool {
	return !now.Add(scheduleTick).Before(firstAt)
}

func startScheduler() {
	go func() {
		t := time.NewTicker(scheduleTick)
		defer t.Stop()
		for now := range t.C {
			runScheduleTick(now)
		}
	}()
}

// runScheduleTick starts, for every enabled schedule-triggered flow, the
// first cron occurrence whose window (cron time + earliest offset ... cron
// time + latest offset) is open, or opens before the next tick, and is not
// yet started. If the app was down when the window opened it joins the run in
// progress; steps already in the past are skipped.
func runScheduleTick(now time.Time) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != triggerSchedule {
			continue // disabled, manual, or a player-join flow (see startJoinFlows)
		}
		c, err := parseCron(f.Schedule)
		if err != nil {
			continue
		}
		pf, err := parseFlowScript(f.Script)
		if err != nil {
			continue
		}
		first := time.Duration(pf.Steps[0].OffsetSecs) * time.Second
		last := time.Duration(pf.Steps[len(pf.Steps)-1].OffsetSecs) * time.Second
		// First occurrence whose last step hasn't passed yet.
		t, ok := c.next(now.Add(-last).Add(-scheduleGrace))
		for i := 0; ok && i < 4; i++ {
			if !launchDue(now, t.Add(first)) {
				break // this window (and every later one) opens after the next tick
			}
			key := fmt.Sprintf("%d@%d", f.ID, t.Unix())
			if _, done := scheduleFired[key]; !done {
				err := launchFlow(f, pf, t, sourceSchedule)
				if errors.Is(err, errFlowRunning) {
					break // busy: retry next tick
				}
				if err != nil {
					log.Printf("scheduler: %s: %v", f.Name, err)
				}
				scheduleFired[key] = t
				break
			}
			t, ok = c.next(t)
		}
	}
	for k, e := range scheduleFired {
		if e.Before(now.Add(-48 * time.Hour)) {
			delete(scheduleFired, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Player-join trigger
// ---------------------------------------------------------------------------

// lobbySuffix is what BattlEye appends to a name while the player is still on
// the loading screen. It is not part of their name.
const lobbySuffix = "(Lobby)"

// inLobby reports whether BattlEye still lists the player as loading in.
func inLobby(p Player) bool {
	f := strings.Fields(p.Name)
	return len(f) > 0 && strings.EqualFold(f[len(f)-1], lobbySuffix)
}

// joinWatch remembers who was online at the last successful poll, by GUID
// (player numbers get reused). Only the background poller goroutine
// (db.go) touches it.
var joinWatch = struct {
	seen   map[string]bool
	primed bool // false until the first successful poll after startup
}{seen: map[string]bool{}}

// playersPolled is called by the poller with every successful `players`
// result and starts the player-join flows for anyone who wasn't online last
// time. The first call after startup only records who is online, so
// restarting this program doesn't greet everybody again.
func playersPolled(players []Player) {
	if joined := newArrivals(players); len(joined) > 0 {
		startJoinFlows(joined)
	}
}

// newArrivals updates joinWatch with this poll's players and returns the ones
// who have just finished joining. A player listed as "(Lobby)" is still on the
// loading screen, so they are left out and look new again on the next poll;
// they count as joined once the suffix is gone. A player who was already
// counted stays counted if they show "(Lobby)" again.
func newArrivals(players []Player) []Player {
	online := map[string]bool{}
	var joined []Player
	for _, p := range players {
		if p.GUID == "" {
			continue
		}
		if inLobby(p) && !joinWatch.seen[p.GUID] {
			continue // still loading in: not joined yet
		}
		online[p.GUID] = true
		if joinWatch.primed && !joinWatch.seen[p.GUID] {
			joined = append(joined, p)
		}
	}
	joinWatch.seen, joinWatch.primed = online, true
	return joined
}

// playersLost is called when the game server stops answering (the poller's
// third failed poll in a row). Everyone is forgotten, so players are greeted
// again when they reconnect after the server comes back.
func playersLost() {
	joinWatch.seen = map[string]bool{}
}

// startJoinFlows runs every enabled player-join flow once for each player.
func startJoinFlows(joined []Player) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("join flows: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != triggerJoin {
			continue
		}
		pf, err := parseFlowScript(f.Script)
		if err != nil || checkJoinSteps(pf) != nil {
			continue
		}
		for _, p := range joined {
			launchJoinRun(f, pf, p)
		}
	}
}

// launchJoinRun works through a join flow's steps for one player. Unlike a
// scheduled flow, several players can be in progress at once: they share one
// flowRun (one log on the page, one Stop button), which stays active until
// the last player's steps are done.
func launchJoinRun(f Flow, pf *parsedFlow, p Player) {
	flowRuns.Lock()
	run := flowRuns.byFlow[f.ID]
	fresh := run == nil || !run.active
	if fresh {
		ctx, cancel := context.WithCancel(context.Background())
		run = &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true, anchor: time.Now(), ctx: ctx, cancel: cancel}
		flowRuns.byFlow[f.ID] = run
	}
	run.jobs++
	flowRuns.last[f.ID] = time.Now()
	flowRuns.Unlock()
	if fresh {
		run.logf("Started by %s", sourceJoin)
	}
	go executeJoinFlow(run, pf, p)
}

// executeJoinFlow runs the steps for one joining player, with offsets counted
// from the moment the join was seen. Cancel steps never apply here: there is
// no countdown to undo.
func executeJoinFlow(run *flowRun, pf *parsedFlow, p Player) {
	joinedAt := time.Now()
	run.logf("player %d (%s) joined", p.ID, p.Name)
	stopped := false
	clock := &stepClock{base: joinedAt}
	for _, st := range pf.Steps {
		if !sleepUntil(run.ctx, clock.at(st.OffsetSecs)) {
			stopped = true
			break
		}
		if late := clock.fired(st.OffsetSecs); late > 0 {
			run.logf("%q for player %d is %.1fs late; later steps move back to keep their spacing", st.Command, p.ID, late.Seconds())
		}
		runFlowCommand(run, st.Command, st.OffsetSecs, &p)
	}
	if stopped {
		run.logf("stopped for player %d (%s)", p.ID, p.Name)
	}
	flowRuns.Lock()
	run.jobs--
	if run.jobs == 0 && run.active {
		run.active = false
		switch {
		case stopped:
			run.status = "stopped"
		case run.failed:
			run.status = "error"
		default:
			run.status = "done"
		}
		run.cancel() // release the context
	}
	flowRuns.Unlock()
}