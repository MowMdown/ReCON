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

	_ "time/tzdata" // lets the TZ env var work in minimal container images
)

// ============================================================================
// flows.go overview
// ----------------------------------------------------------------------------
// A "flow" is a named, saved script of RCON steps. What starts it is its
// Trigger:
//
//	schedule  a cron expression; each time it fires the flow runs, and every
//	          step has an offset from that cron time (before, on or after it)
//	join      runs once for every player who joins, offsets counted from the
//	          moment the join was seen; $player, $name ... are that player
//	manual    runs only when you press Run
//
// A schedule or join flow only runs by itself while it is Enabled.
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
//	-60m   60 minutes BEFORE the cron time        +90    90 seconds AFTER it
//	0m     exactly on the cron time               units: s, m, h (none = seconds)
//	cancel runs only if the flow is stopped before its cron time arrives
//
// A step command can also be "kickall [reason]" (or "#kickall"), which kicks every connected
// player (the same sweep as POST /kickall); the reason defaults to
// "Server restarting".
//
// If / else: a step can be a block. The offset goes on the if line; the
// lines inside are plain commands (no offsets) that all run at that moment:
//
//	0s | if $name == Survivor
//	kick $player Please change your name
//	else
//	say $player Welcome, $name!
//	end
//
// The condition is <value> <op> <value> with spaces around the operator.
// Operators: == != < > <= >= contains !contains (text ignores case, two
// numbers compare as numbers). It is checked when the step's time arrives.
// "fi" works as well as "end". Blocks can't be nested.
//
// Variables (in commands and conditions):
//
//	$minutes $seconds      how long before the cron time the step runs (0 on/after it)
//	$players               how many players are online right now (asks the server)
//	$player $name $guid $ip $ping   the joining player (join flows only)
//
// Schedule: a standard 5-field cron expression (minute hour day-of-month month
// day-of-week) in the server's local time, so set TZ in the container.
// Supports * , - / lists, month/day names and @hourly @daily @weekly @monthly
// @yearly.
//
// Join flows are started by the background poller (db.go) every 5 seconds. A
// player counts as joined once they finish loading in (their name no longer
// ends in "(Lobby)").
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

type flowCond struct {
	LHS, Op, RHS string
}

// flowStep is one timed step: either a plain command, or (Cond != nil) an
// if-block whose Then or Else commands run depending on the condition.
type flowStep struct {
	OffsetSecs int // negative = before the cron time, positive = after
	Command    string
	Cond       *flowCond
	Then, Else []string
}

// label is the step as shown in the run log.
func (s flowStep) label() string {
	if s.Cond != nil {
		return "if " + s.Cond.String()
	}
	return s.Command
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
	return 0, fmt.Errorf("time %q needs a sign: -%s runs before the cron time, +%s after it", when, m[2]+m[3], m[2]+m[3])
}

// Every variable a script may use. Anything else starting with $ in a
// condition is reported as a typo.
var knownVars = map[string]bool{
	"$minutes": true, "$seconds": true, "$players": true,
	"$player": true, "$name": true, "$guid": true, "$ip": true, "$ping": true,
}

var (
	condRe    = regexp.MustCompile(`^(\S+)\s+(==|!=|<=|>=|<|>|contains|!contains)\s+(.+)$`)
	condVarRe = regexp.MustCompile(`\$[A-Za-z]+`)
)

// parseCond reads "<value> <op> <value>". The operator needs spaces around
// it; the right-hand side is the rest of the line (optional "quotes").
func parseCond(s string) (*flowCond, error) {
	s = strings.TrimSpace(s)
	m := condRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("bad condition %q (write: <value> <op> <value>, with spaces around the operator; operators: == != < > <= >= contains !contains)", s)
	}
	rhs := strings.TrimSpace(m[3])
	if len(rhs) >= 2 && rhs[0] == '"' && rhs[len(rhs)-1] == '"' {
		rhs = rhs[1 : len(rhs)-1]
	}
	c := &flowCond{LHS: m[1], Op: strings.ToLower(m[2]), RHS: rhs}
	for _, v := range condVarRe.FindAllString(c.LHS+" "+c.RHS, -1) {
		if !knownVars[v] {
			return nil, fmt.Errorf("unknown variable %s in condition (known: $name $player $guid $ip $ping $players $minutes $seconds)", v)
		}
	}
	return c, nil
}

func (c *flowCond) String() string { return c.LHS + " " + c.Op + " " + c.RHS }

// eval checks the condition. expand fills in the variables of one side. The
// returned text is the condition with the variables filled in, for the log.
// Two whole numbers compare as numbers; otherwise text compares ignoring case.
func (c *flowCond) eval(expand func(string) (string, error)) (bool, string, error) {
	l, err := expand(c.LHS)
	if err != nil {
		return false, "", err
	}
	r, err := expand(c.RHS)
	if err != nil {
		return false, "", err
	}
	l, r = strings.TrimSpace(l), strings.TrimSpace(r)
	shown := fmt.Sprintf("%q %s %q", l, c.Op, r)
	ln, lerr := strconv.Atoi(l)
	rn, rerr := strconv.Atoi(r)
	numeric := lerr == nil && rerr == nil
	switch c.Op {
	case "==":
		if numeric {
			return ln == rn, shown, nil
		}
		return strings.EqualFold(l, r), shown, nil
	case "!=":
		if numeric {
			return ln != rn, shown, nil
		}
		return !strings.EqualFold(l, r), shown, nil
	case "contains":
		return strings.Contains(strings.ToLower(l), strings.ToLower(r)), shown, nil
	case "!contains":
		return !strings.Contains(strings.ToLower(l), strings.ToLower(r)), shown, nil
	}
	if !numeric {
		return false, shown, fmt.Errorf("%s needs two numbers", c.Op)
	}
	switch c.Op {
	case "<":
		return ln < rn, shown, nil
	case ">":
		return ln > rn, shown, nil
	case "<=":
		return ln <= rn, shown, nil
	case ">=":
		return ln >= rn, shown, nil
	}
	return false, shown, fmt.Errorf("unknown operator %s", c.Op)
}

func parseFlowScript(script string) (*parsedFlow, error) {
	pf := &parsedFlow{}
	var blk *flowStep // the open if-block, if any
	inElse := false
	blkLine := 0
	for i, raw := range strings.Split(script, "\n") {
		n := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		lower := strings.ToLower(line)

		// Inside an if-block: plain commands up to else / end.
		if blk != nil {
			switch lower {
			case "else":
				if inElse {
					return nil, fmt.Errorf("line %d: an if can only have one else", n)
				}
				inElse = true
				continue
			case "end", "fi":
				if len(blk.Then)+len(blk.Else) == 0 {
					return nil, fmt.Errorf("line %d: the if on line %d has no commands", n, blkLine)
				}
				pf.Steps = append(pf.Steps, *blk)
				blk = nil
				continue
			}
			if lower == "if" || strings.HasPrefix(lower, "if ") {
				return nil, fmt.Errorf("line %d: if blocks can't be nested", n)
			}
			if when, _, ok := strings.Cut(line, "|"); ok {
				w := strings.ToLower(strings.ReplaceAll(when, " ", ""))
				if w == "cancel" || offsetRe.MatchString(w) {
					return nil, fmt.Errorf("line %d: lines inside an if have no offset (the if line's offset is used)", n)
				}
			}
			cmd := cleanReason(line)
			if inElse {
				blk.Else = append(blk.Else, cmd)
			} else {
				blk.Then = append(blk.Then, cmd)
			}
			continue
		}

		if lower == "else" || lower == "end" || lower == "fi" {
			return nil, fmt.Errorf("line %d: %q without an if", n, lower)
		}
		if lower == "if" || strings.HasPrefix(lower, "if ") {
			return nil, fmt.Errorf("line %d: put the offset before the if, like \"0s | if $name == Bob\"", n)
		}
		when, cmd, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"<offset> | <command>\"", n)
		}
		when = strings.ToLower(strings.TrimSpace(when))
		trimmed := strings.TrimSpace(cmd)
		isIf := strings.EqualFold(trimmed, "if") || strings.HasPrefix(strings.ToLower(trimmed), "if ")
		if isIf {
			if when == "cancel" {
				return nil, fmt.Errorf("line %d: cancel lines can't use if", n)
			}
			off, err := parseOffset(when)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", n, err)
			}
			cond, err := parseCond(strings.TrimSpace(trimmed[2:]))
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", n, err)
			}
			blk = &flowStep{OffsetSecs: off, Cond: cond}
			inElse, blkLine = false, n
			continue
		}
		cmd = cleanReason(cmd)
		if cmd == "" {
			return nil, fmt.Errorf("line %d: command is empty", n)
		}
		if when == "cancel" {
			pf.OnCancel = append(pf.OnCancel, cmd)
			continue
		}
		off, err := parseOffset(when)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		pf.Steps = append(pf.Steps, flowStep{OffsetSecs: off, Command: cmd})
	}
	if blk != nil {
		return nil, fmt.Errorf("line %d: this if is missing its end", blkLine)
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

// The three ways a flow can be started.
const (
	trigSchedule = "schedule" // on a cron schedule
	trigJoin     = "join"     // each time a player joins
	trigManual   = "manual"   // only with the Run button
)

func validTrigger(t string) bool {
	return t == trigSchedule || t == trigJoin || t == trigManual
}

// legacyTrigger guesses the trigger for a flow from a file exported before
// triggers existed: no real schedule meant "when a player joins".
func legacyTrigger(schedule string) string {
	s := strings.Join(strings.Fields(schedule), " ")
	if s == "" || s == "* * * * *" {
		return trigJoin
	}
	return trigSchedule
}

type Flow struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Script          string `json:"script"`
	Trigger         string `json:"trigger"`  // schedule | join | manual
	Schedule        string `json:"schedule"` // cron expression, only for the schedule trigger
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	NextRun         string `json:"nextRun,omitempty"` // computed for the UI, not stored
}

const defaultRestartScript = `// Server restart. Offsets are relative to the cron time:
// -60m = 60 minutes before, 0m = on the cron time, +90 = 90 seconds after.
-60m | say -1 Server is going to restart in $minutes minutes!
-30m | say -1 Server is going to restart in $minutes minutes!
-15m | say -1 Server is going to restart in $minutes minutes!
-10m | say -1 Server is going to restart in $minutes minutes!
-5m  | say -1 Server is going to restart in $minutes minutes!
-3m  | say -1 Server is going to restart in $minutes minutes!
-3m  | #lock
0m   | #shutdown
+90  | #unlock
cancel | say -1 Server restart cancelled.
`

// initFlows creates the table and seeds an example restart flow on first run.
func initFlows() error {
	return globalDatabase.EnsureFlowsTable("Server restart", defaultRestartScript, "0 6 * * *")
}

// playerVarRe finds the per-player variables only join flows can fill in.
// ($players, the online count, works anywhere and doesn't match.)
var playerVarRe = regexp.MustCompile(`\$(player|name|guid|ip|ping)\b`)

// usesPlayerVars reports whether a script uses a per-player variable.
func usesPlayerVars(script string) bool { return playerVarRe.MatchString(script) }

const playerVarsOnlyJoin = "$player, $name, $guid, $ip and $ping only work in On Join flows"

// checkJoinSteps rejects steps a join flow can never run: the flow starts
// when the player is seen joining, so nothing can happen before that.
func checkJoinSteps(pf *parsedFlow) error {
	if pf.Steps[0].OffsetSecs < 0 {
		return errors.New("On Join flows start when the player joins, so steps can't have a - offset (use 0s or +)")
	}
	return nil
}

// flowsWithNextRun lists every flow and fills NextRun for enabled ones.
func flowsWithNextRun() ([]Flow, error) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range flows {
		if !flows[i].ScheduleEnabled {
			continue
		}
		switch flows[i].Trigger {
		case trigJoin:
			flows[i].NextRun = "On Join"
		case trigSchedule:
			if c, err := parseCron(flows[i].Schedule); err == nil {
				if next, ok := c.next(now); ok {
					flows[i].NextRun = fmtWhen(next)
				}
			}
		}
	}
	return flows, nil
}

// validateAndSaveFlow checks a flow from the edit dialog and stores it.
// Every error it returns is a user-facing message (the API sends them as 400).
//
// Changing a flow's trigger saves it disabled, and a new player-join flow
// always starts disabled, so an edit can never make a flow start firing in a
// way you didn't see. Manual flows are never enabled (there is nothing to
// enable); join and manual flows don't keep a schedule.
func validateAndSaveFlow(f Flow) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if !validTrigger(f.Trigger) {
		return 0, errors.New("choose a flow type: Schedule, On Join or Manual")
	}
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return 0, err
	}
	switch f.Trigger {
	case trigSchedule:
		if _, err := parseCron(f.Schedule); err != nil {
			return 0, errors.New("schedule: " + err.Error())
		}
		if usesPlayerVars(f.Script) {
			return 0, errors.New(playerVarsOnlyJoin)
		}
	case trigJoin:
		f.Schedule = ""
		if err := checkJoinSteps(pf); err != nil {
			return 0, err
		}
	case trigManual:
		f.Schedule = ""
		f.ScheduleEnabled = false
		if usesPlayerVars(f.Script) {
			return 0, errors.New(playerVarsOnlyJoin)
		}
	}
	if f.ID == 0 && f.Trigger == trigJoin {
		f.ScheduleEnabled = false
	} else if f.ID != 0 {
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
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return err
	}
	switch f.Trigger {
	case trigJoin:
		return checkJoinSteps(pf)
	case trigSchedule:
		if _, err := parseCron(f.Schedule); err != nil {
			return errors.New("schedule: " + err.Error())
		}
		if usesPlayerVars(f.Script) {
			return errors.New(playerVarsOnlyJoin)
		}
		return nil
	}
	return errors.New("this flow's type is Manual, so there is nothing to enable (use Run)")
}

// previewFlow builds the plain-English summary the edit dialog shows under
// the trigger: what it means, the next few run times, when the next
// countdown starts, and how the script's offsets fit around it.
// The result is sent to the browser as-is.
func previewFlow(trigger, schedule, script string) map[string]any {
	resp := map[string]any{"ok": true}
	pf, perr := parseFlowScript(script)
	if perr == nil {
		resp["span"] = pf.span()
	}
	switch trigger {
	case trigJoin:
		resp["description"] = "Runs each time a player joins, while Enabled ($player = their number, $name = their name). Press Run to start it by hand."
		if perr == nil {
			if err := checkJoinSteps(pf); err != nil {
				resp["span"] = "Note: " + err.Error() + "."
			} else {
				resp["span"] = strings.Replace(pf.span(), "the scheduled time", "the moment the player joins", -1)
			}
		}
		return resp
	case trigManual:
		resp["description"] = "Runs only when you press Run."
		if perr == nil {
			resp["span"] = strings.Replace(pf.span(), "the scheduled time", "the moment you press Run", -1)
		}
		return resp
	}
	c, err := parseCron(strings.TrimSpace(schedule))
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
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
// guarded by flowRuns' mutex; anchor never changes after creation.
type flowRun struct {
	flowID int64
	name   string
	status string // running | done | stopped
	active bool
	anchor time.Time // the cron time this run is built around
	log    []string
	cancel context.CancelFunc
	ctx    context.Context // player-join runs only: shared by all players' jobs
	jobs   int             // player-join runs only: players still being worked through
}

var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
}{byFlow: map[int64]*flowRun{}}

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

// startFlow starts a manual run (the Run button). With minutes <= 0 the cron
// time is placed so the earliest step fires immediately; otherwise the cron
// time is `minutes` from now and any step that would already be in the past
// is skipped (e.g. 10 -> the -60m/-30m/-15m steps are skipped).
func startFlow(f Flow, minutes int) error {
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return err
	}
	if usesPlayerVars(f.Script) {
		return errors.New("this flow uses $player, $name, $guid, $ip or $ping, so it only runs as an On Join flow")
	}
	lead := 0
	if minutes > 0 {
		lead = minutes * 60
	} else if first := pf.Steps[0].OffsetSecs; first < 0 {
		lead = -first
	}
	return launchFlow(f, pf, time.Now().Add(time.Duration(lead)*time.Second), "manual run")
}

// stopFlow cancels the active run of a flow (the Stop button). Its cancel
// steps run if the cron time hasn't been reached yet (see executeFlow).
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
	flowRuns.Unlock()
	return globalDatabase.DeleteFlow(id)
}

// launchFlow starts the run goroutine for an already-parsed flow around the
// given cron time.
func launchFlow(f Flow, pf *parsedFlow, anchor time.Time, source string) error {
	flowRuns.Lock()
	if old := flowRuns.byFlow[f.ID]; old != nil && old.active {
		flowRuns.Unlock()
		return errFlowRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true, anchor: anchor, cancel: cancel}
	flowRuns.byFlow[f.ID] = run
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

// onlineCount asks the server how many players are connected. A var so tests
// can replace it.
var onlineCount = func() (int, error) {
	out, acked, err := runShared("players", 3*time.Second, true)
	if err != nil {
		return 0, err
	}
	if !acked {
		return 0, errors.New("no answer from the server")
	}
	return len(parsePlayers(out)), nil
}

// expandVars fills the $variables of a command or condition side.
// offsetSecs is the step's offset ($minutes/$seconds); p is the joining
// player for player-join flows (fills $player $name $guid $ip $ping), nil
// otherwise. $players asks the server, and fails if it can't answer.
func expandVars(text string, offsetSecs int, p *Player) (string, error) {
	before := 0
	if offsetSecs < 0 {
		before = -offsetSecs
	}
	var vars []string
	// $players must come before $player: the replacer tries them in order.
	if strings.Contains(text, "$players") {
		n, err := onlineCount()
		if err != nil {
			return "", fmt.Errorf("could not read the player count: %v", err)
		}
		vars = append(vars, "$players", strconv.Itoa(n))
	}
	vars = append(vars, "$minutes", strconv.Itoa(before/60), "$seconds", strconv.Itoa(before))
	if p != nil {
		vars = append(vars, "$player", strconv.Itoa(p.ID), "$name", p.Name,
			"$guid", p.GUID, "$ip", p.IP, "$ping", strconv.Itoa(p.Ping))
	}
	return strings.NewReplacer(vars...).Replace(text), nil
}

// runFlowStep runs one step: a plain command, or the Then / Else commands of
// an if-block, chosen when the step's time arrives.
func runFlowStep(run *flowRun, st flowStep, p *Player) {
	if st.Cond == nil {
		runFlowCommand(run, st.Command, st.OffsetSecs, p)
		return
	}
	expand := func(s string) (string, error) { return expandVars(s, st.OffsetSecs, p) }
	ok, shown, err := st.Cond.eval(expand)
	if err != nil {
		run.logf("skipped if %s: %v", st.Cond, err)
		return
	}
	branch := st.Else
	if ok {
		branch = st.Then
	}
	run.logf("if %s -> %v", shown, ok)
	for _, c := range branch {
		runFlowCommand(run, c, st.OffsetSecs, p)
	}
}

// runFlowCommand sends one command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
//
// p is the joining player for player-join flows (fills $player and friends),
// nil for scheduled and manual runs.
func runFlowCommand(run *flowRun, command string, offsetSecs int, p *Player) {
	cmd, err := expandVars(command, offsetSecs, p)
	if err != nil {
		run.logf("skipped %q: %v", command, err)
		return
	}
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
			return
		}
		run.logf("kickall %q: kicked %v, no ack for %v, still connected %v", reason, rep.Order, rep.Unacked, rep.Remaining)
		return
	}
	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := runShared(cmd, 2*time.Second, retry)
	run.logf("sent %q (acked=%v err=%v)", cmd, acked, err)
}

func executeFlow(ctx context.Context, run *flowRun, pf *parsedFlow, source string) {
	defer run.cancel()
	started := time.Now()
	run.logf("started by %s, cron time is %s", source, run.anchor.Format("Mon 15:04:05"))
	for _, st := range pf.Steps {
		fireAt := run.anchor.Add(time.Duration(st.OffsetSecs) * time.Second)
		if fireAt.Before(started.Add(-scheduleGrace)) {
			run.logf("skipped %q (its time had already passed)", st.label())
			continue
		}
		if !sleepUntil(ctx, fireAt) {
			run.logf("stopped")
			// Cancel commands undo a countdown that never reached its cron time.
			if time.Now().Before(run.anchor) {
				for _, c := range pf.OnCancel {
					runFlowCommand(run, c, 0, nil)
				}
			}
			run.set("stopped", false)
			return
		}
		runFlowStep(run, st, nil)
	}
	run.logf("done")
	run.set("done", false)
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

func startScheduler() {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for now := range t.C {
			runScheduleTick(now)
		}
	}()
}

// runScheduleTick starts, for every enabled schedule flow, the first cron
// occurrence whose window (cron time + earliest offset ... cron time + latest
// offset) is open and not yet started. If the app was down when the window
// opened it joins the run in progress; steps already in the past are skipped.
func runScheduleTick(now time.Time) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != trigSchedule {
			continue // disabled, or started some other way (see startJoinFlows)
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
			if now.Before(t.Add(first)) {
				break // this window (and every later one) hasn't opened yet
			}
			key := fmt.Sprintf("%d@%d", f.ID, t.Unix())
			if _, done := scheduleFired[key]; !done {
				err := launchFlow(f, pf, t, "schedule")
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

// lobbySuffix is what the server appends to a player's name while they are
// still on the loading screen. It isn't part of their name.
const lobbySuffix = "(Lobby)"

// inLobby reports whether a player is still loading in.
func inLobby(p Player) bool {
	f := strings.Fields(p.Name)
	return len(f) > 0 && strings.EqualFold(f[len(f)-1], lobbySuffix)
}

// newArrivals updates joinWatch with this poll's players and returns the ones
// who weren't online at the previous poll. A player still in the lobby isn't
// counted as joined yet and isn't recorded, so they arrive on the first poll
// where the suffix is gone. A player already seen who shows (Lobby) again
// stays seen.
func newArrivals(players []Player) []Player {
	online := map[string]bool{}
	var joined []Player
	for _, p := range players {
		if p.GUID == "" {
			continue
		}
		if inLobby(p) && !joinWatch.seen[p.GUID] {
			continue
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
		if !f.ScheduleEnabled || f.Trigger != trigJoin {
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
	if run == nil || !run.active {
		ctx, cancel := context.WithCancel(context.Background())
		run = &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true, anchor: time.Now(), ctx: ctx, cancel: cancel}
		flowRuns.byFlow[f.ID] = run
	}
	run.jobs++
	flowRuns.Unlock()
	go executeJoinFlow(run, pf, p)
}

// executeJoinFlow runs the steps for one joining player, with offsets counted
// from the moment the join was seen. Cancel steps never apply here: there is
// no countdown to undo.
func executeJoinFlow(run *flowRun, pf *parsedFlow, p Player) {
	joinedAt := time.Now()
	run.logf("player %d (%s) joined", p.ID, p.Name)
	stopped := false
	for _, st := range pf.Steps {
		if !sleepUntil(run.ctx, joinedAt.Add(time.Duration(st.OffsetSecs)*time.Second)) {
			stopped = true
			break
		}
		runFlowStep(run, st, &p)
	}
	if stopped {
		run.logf("stopped for player %d (%s)", p.ID, p.Name)
	}
	flowRuns.Lock()
	run.jobs--
	if run.jobs == 0 && run.active {
		run.active = false
		run.status = "done"
		if stopped {
			run.status = "stopped"
		}
		run.cancel() // release the context
	}
	flowRuns.Unlock()
}
