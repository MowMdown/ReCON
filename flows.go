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
// A "flow" is a named script of RCON steps, each timed by an offset from the
// flow's "0 point":   <offset> | <command>
//
//	-60m | say -1 Restart in $minutes minutes!    (60 minutes before the 0 point)
//	-1m  | if $players > 0                        (lines below a step belong to it)
//	#kickall Server Restart
//	end
//	0m   | #shutdown                              (on it)
//	+90  | #unlock                                (90 seconds after; no unit = seconds)
//	cancel | say -1 Restart cancelled.            (only if stopped before the 0 point)
//
// Sections: the script language (parsing, if/else, $variables, sending
// commands), cron, flow definitions and validation, the runner, the
// scheduler, and the player-join trigger.
//
// Triggers (stored in the schedule column: "manual", "on-join" or a cron
// expression): manual = Run button only; schedule = cron time is the 0 point;
// join = runs once per joining player, 0 point = when the join was seen.
// The in-app Help dialog (templates/flows.html) is the full user guide.
//
// Runs live in memory; a scheduled run that was mid-countdown when the
// program restarted is picked up again by the scheduler.
// ============================================================================

// ---------------------------------------------------------------------------
// Script language: parsing, if/else/end, conditions and $variables
// ---------------------------------------------------------------------------

type nodeKind int

const (
	nodeCmd nodeKind = iota // an RCON command
	nodeSet                 // $var = value
	nodeIf                  // if / else / end block
)

type flowNode struct {
	kind   nodeKind
	line   int
	text   string    // the command, or the value of a $var = value line
	name   string    // the variable, for nodeSet
	cond   *flowCond // for nodeIf
	then   []flowNode
	els    []flowNode
	inElse bool // parse-time: lines now go to els
}

type flowStep struct {
	OffsetSecs int    // negative = before the 0 point, positive = after
	When       string // the offset as written ("-60m", "cancel"), for log lines
	Line       int
	Body       []flowNode
}

// label names a step in log lines, e.g. `step -60m (line 3)`.
func (st flowStep) label() string { return fmt.Sprintf("step %s (line %d)", st.When, st.Line) }

type parsedFlow struct {
	Steps    []flowStep // sorted by offset, earliest first (script order kept for ties)
	OnCancel []flowStep
}

// Variables the program fills in; a script can't set these.
var builtinVars = map[string]bool{"player": true, "name": true, "minutes": true, "seconds": true, "players": true}

var (
	offsetRe   = regexp.MustCompile(`^([+-]?)(\d+)([smh]?)$`)
	stepLikeRe = regexp.MustCompile(`^(cancel|[+-]?\s*\d[\d\s]*[a-z]*)$`)
	assignRe   = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)
	varRe      = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	numberRe   = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
)

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
	switch {
	case m[1] == "-":
		return -n, nil
	case m[1] == "+", n == 0:
		return n, nil
	}
	return 0, fmt.Errorf("time %q needs a sign: -%s runs before the 0 point, +%s after it", when, m[2]+m[3], m[2]+m[3])
}

// normalizeScript is applied before a script is saved: blank lines are
// dropped (a "//" line is the way to keep a visual separator) and trailing
// spaces trimmed. Indentation is kept.
func normalizeScript(s string) string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// parseFlowScript turns a script into steps. Every error names the line.
func parseFlowScript(script string) (*parsedFlow, error) {
	pf := &parsedFlow{}
	var cur *flowStep
	var curCancel bool
	var stack []*flowNode // open if blocks, innermost last
	assigned := map[string]bool{}
	type varUse struct {
		name string
		line int
	}
	var condVars []varUse

	// add appends a node where lines are currently going and returns it.
	add := func(n flowNode) *flowNode {
		if len(stack) == 0 {
			cur.Body = append(cur.Body, n)
			return &cur.Body[len(cur.Body)-1]
		}
		top := stack[len(stack)-1]
		if top.inElse {
			top.els = append(top.els, n)
			return &top.els[len(top.els)-1]
		}
		top.then = append(top.then, n)
		return &top.then[len(top.then)-1]
	}

	finish := func() error {
		if cur == nil {
			return nil
		}
		if len(stack) > 0 {
			return fmt.Errorf("line %d: this \"if\" has no \"end\"", stack[len(stack)-1].line)
		}
		if len(cur.Body) == 0 {
			return fmt.Errorf("line %d: step has no command", cur.Line)
		}
		if curCancel {
			pf.OnCancel = append(pf.OnCancel, *cur)
		} else {
			pf.Steps = append(pf.Steps, *cur)
		}
		cur = nil
		return nil
	}

	body := func(text string, ln int) error {
		low := strings.ToLower(text)
		switch {
		case low == "if" || strings.HasPrefix(low, "if "):
			c, err := parseCond(strings.TrimSpace(text[2:]))
			if err != nil {
				return fmt.Errorf("line %d: %v", ln, err)
			}
			for _, and := range c.any {
				for _, cmp := range and {
					for _, side := range []string{cmp.left, cmp.right} {
						for _, m := range varRe.FindAllStringSubmatch(side, -1) {
							condVars = append(condVars, varUse{strings.ToLower(m[1]), ln})
						}
					}
				}
			}
			stack = append(stack, add(flowNode{kind: nodeIf, line: ln, cond: c}))
		case low == "else":
			if len(stack) == 0 {
				return fmt.Errorf("line %d: \"else\" without an \"if\"", ln)
			}
			top := stack[len(stack)-1]
			if top.inElse {
				return fmt.Errorf("line %d: second \"else\" for the \"if\" on line %d", ln, top.line)
			}
			top.inElse = true
		case low == "end":
			if len(stack) == 0 {
				return fmt.Errorf("line %d: \"end\" without an \"if\"", ln)
			}
			stack = stack[:len(stack)-1]
		case strings.HasPrefix(low, "else "):
			return fmt.Errorf("line %d: \"else\" goes on a line of its own (for else-if, put an if inside the else)", ln)
		case strings.HasPrefix(low, "end "):
			return fmt.Errorf("line %d: \"end\" goes on a line of its own", ln)
		default:
			if m := assignRe.FindStringSubmatch(text); m != nil {
				name := strings.ToLower(m[1])
				if strings.HasPrefix(m[2], "=") {
					return fmt.Errorf("line %d: == compares, so it goes in an if (use a single = to set a variable)", ln)
				}
				if builtinVars[name] {
					return fmt.Errorf("line %d: $%s is filled in automatically and can't be set", ln, name)
				}
				assigned[name] = true
				add(flowNode{kind: nodeSet, line: ln, name: name, text: strings.TrimSpace(m[2])})
				return nil
			}
			add(flowNode{kind: nodeCmd, line: ln, text: cleanReason(text)})
		}
		return nil
	}

	for i, raw := range strings.Split(script, "\n") {
		ln := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// A step line: "<offset> | ..." or "cancel | ...". A "||" inside a
		// condition never looks like this, because what's before it isn't an
		// offset.
		if when, rest, ok := strings.Cut(line, "|"); ok {
			w := strings.ToLower(strings.TrimSpace(when))
			if stepLikeRe.MatchString(w) {
				if err := finish(); err != nil {
					return nil, err
				}
				cur = &flowStep{Line: ln, When: strings.ReplaceAll(w, " ", "")}
				curCancel = w == "cancel"
				if !curCancel {
					off, err := parseOffset(w)
					if err != nil {
						return nil, fmt.Errorf("line %d: %v", ln, err)
					}
					cur.OffsetSecs = off
				}
				if rest = strings.TrimSpace(rest); rest != "" {
					if err := body(rest, ln); err != nil {
						return nil, err
					}
				}
				continue
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: expected \"<offset> | <command>\" (a script starts with a step such as 0s | say -1 hello)", ln)
		}
		if err := body(line, ln); err != nil {
			return nil, err
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(pf.Steps) == 0 {
		return nil, errors.New("flow needs at least one step like \"0m | say -1 hello\"")
	}
	for _, v := range condVars {
		if !builtinVars[v.name] && !assigned[v.name] {
			return nil, fmt.Errorf("line %d: unknown variable $%s (set it first with $%s = ...)", v.line, v.name, v.name)
		}
	}
	sort.SliceStable(pf.Steps, func(a, b int) bool { return pf.Steps[a].OffsetSecs < pf.Steps[b].OffsetSecs })
	return pf, nil
}

// ---------------------------------------------------------------------------
// Conditions
// ---------------------------------------------------------------------------

type flowCmp struct{ left, op, right string }

// flowCond is any of several all-of groups: (a && b) || (c) ...
type flowCond struct {
	text string
	any  [][]flowCmp
}

var cmpOps = []string{"==", "!=", "<=", ">=", "<", ">"}

func parseCond(s string) (*flowCond, error) {
	if s == "" {
		return nil, errors.New("\"if\" needs a condition, e.g. if $name == survivor")
	}
	c := &flowCond{text: s}
	for _, orPart := range strings.Split(s, "||") {
		var and []flowCmp
		for _, part := range strings.Split(orPart, "&&") {
			cmp, err := parseCmp(strings.TrimSpace(part))
			if err != nil {
				return nil, err
			}
			and = append(and, cmp)
		}
		c.any = append(c.any, and)
	}
	return c, nil
}

func parseCmp(s string) (flowCmp, error) {
	if s == "" {
		return flowCmp{}, errors.New("missing comparison next to && or ||")
	}
	for i := 0; i < len(s); i++ {
		for _, op := range cmpOps {
			if strings.HasPrefix(s[i:], op) {
				l, r := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(op):])
				if l == "" || r == "" {
					return flowCmp{}, fmt.Errorf("%q needs something on both sides of %s", s, op)
				}
				return flowCmp{l, op, r}, nil
			}
		}
		if s[i] == '=' {
			return flowCmp{}, fmt.Errorf("%q: use == to compare (a single = sets a variable)", s)
		}
	}
	return flowCmp{}, fmt.Errorf("%q needs a comparison: == != < <= > >=", s)
}

// unquote drops one pair of surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ---------------------------------------------------------------------------
// Running steps
// ---------------------------------------------------------------------------

// runCtx is one job's variables: one run of a scheduled or manual flow, or
// one player in a join flow.
type runCtx struct {
	run    *flowRun
	p      *Player           // the joining player, join flows only
	vars   map[string]string // set by $var = value
	offset int               // the current step's offset, for $minutes/$seconds
}

func newRunCtx(run *flowRun, p *Player) *runCtx {
	return &runCtx{run: run, p: p, vars: map[string]string{}}
}

// lookup returns a variable's value; ok is false if it has none, and then
// the "$name" text is left as typed.
func (x *runCtx) lookup(name string) (string, bool) {
	before := 0
	if x.offset < 0 {
		before = -x.offset
	}
	switch name {
	case "minutes":
		return strconv.Itoa(before / 60), true
	case "seconds":
		return strconv.Itoa(before), true
	case "player":
		if x.p != nil {
			return strconv.Itoa(x.p.ID), true
		}
		return "", false
	case "name":
		if x.p != nil {
			return x.p.Name, true
		}
		return "", false
	case "players":
		out, acked, err := runShared("players", 3*time.Second, true)
		if err != nil || !acked {
			x.run.logf("couldn't read the player count (err=%v)", err)
			x.run.markFailed()
			return "", false
		}
		return strconv.Itoa(len(parsePlayers(out))), true
	}
	v, ok := x.vars[name]
	return v, ok
}

// expand fills in every $variable that has a value. Values are not expanded
// again, so a player named "$players" stays literal text.
func (x *runCtx) expand(s string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := x.lookup(strings.ToLower(m[1:])); ok {
			return v
		}
		return m
	})
}

func (x *runCtx) evalCond(c *flowCond) bool {
	for _, and := range c.any {
		all := true
		for _, cmp := range and {
			if !x.evalCmp(cmp) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (x *runCtx) evalCmp(c flowCmp) bool {
	l := strings.ToLower(strings.TrimSpace(unquote(x.expand(c.left))))
	r := strings.ToLower(strings.TrimSpace(unquote(x.expand(c.right))))
	if numberRe.MatchString(l) && numberRe.MatchString(r) {
		a, _ := strconv.ParseFloat(l, 64)
		b, _ := strconv.ParseFloat(r, 64)
		switch c.op {
		case "==":
			return a == b
		case "!=":
			return a != b
		case "<":
			return a < b
		case "<=":
			return a <= b
		case ">":
			return a > b
		case ">=":
			return a >= b
		}
	}
	switch c.op {
	case "==":
		return l == r
	case "!=":
		return l != r
	}
	x.run.logf("can't use %s on %q and %q: both sides must be numbers (counted as false)", c.op, l, r)
	return false
}

// runStep runs one step's lines at its offset.
func (x *runCtx) runStep(st flowStep) {
	x.offset = st.OffsetSecs
	x.exec(st.Body)
}

func (x *runCtx) exec(nodes []flowNode) {
	for _, n := range nodes {
		switch n.kind {
		case nodeCmd:
			runFlowCommand(x.run, cleanReason(x.expand(n.text)))
		case nodeSet:
			v := strings.TrimSpace(x.expand(n.text))
			x.vars[n.name] = v
			x.run.logf("set $%s = %q", n.name, v)
		case nodeIf:
			ok := x.evalCond(n.cond)
			x.run.logf("if %s -> %v", n.cond.text, ok)
			if ok {
				x.exec(n.then)
			} else {
				x.exec(n.els)
			}
		}
	}
}

// runFlowCommand sends one command (variables already filled in). Only
// #lock/#unlock are retried (repeating them is harmless); `say` must not be
// repeated, and #shutdown is expected to go unanswered because the server
// dies.
//
// An RCON error, or no reply (except to #shutdown), marks the run as failed;
// the remaining steps still run, and the run ends with status "error".
func runFlowCommand(run *flowRun, cmd string) {
	if cmd == "" {
		return
	}
	lower := strings.ToLower(cmd)

	// kickall [reason] (or #kickall) is this program's own command, not a
	// BattlEye one: it lists the players and kicks each (kickAllUsing).
	if bare := strings.TrimPrefix(lower, "#"); bare == "kickall" || strings.HasPrefix(bare, "kickall ") {
		reason := strings.TrimSpace(strings.TrimPrefix(cmd, "#")[len("kickall"):])
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

// ---------------------------------------------------------------------------
// Describing a script's timing (for the edit dialog)
// ---------------------------------------------------------------------------

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
	switch {
	case first == last && first == 0:
		return "All steps run at the scheduled time."
	case first == last:
		return "All steps run " + relText(first) + " the scheduled time."
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

// What the schedule column holds for the two triggers without a cron expression.
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
	// Trigger (manual | schedule | join) is derived from Schedule when read,
	// not stored. The edit dialog sends it on save; if empty, Schedule decides.
	Trigger string `json:"trigger"`
	// ScheduleEnabled turns the trigger on (schedule and join flows only).
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	NextRun         string `json:"nextRun,omitempty"` // computed for the UI, not stored
	LastRun         string `json:"lastRun,omitempty"` // when it was last started, for the UI, not stored
}

// maxFlowNameLen is the longest flow name allowed, in characters. The Flow
// column on the page is sized to fit it.
const maxFlowNameLen = 48

// clipName shortens a name to at most n characters (not bytes).
func clipName(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// triggerOf reads a flow's trigger from its stored schedule. Blank and
// "* * * * *" (what older flows used for "no schedule") count as manual.
func triggerOf(schedule string) string {
	switch strings.ToLower(strings.Join(strings.Fields(schedule), " ")) {
	case scheduleManual, "", "* * * * *":
		return triggerManual
	case scheduleJoin:
		return triggerJoin
	}
	return triggerSchedule
}

// fillTrigger sets f.Trigger after a flow is read from the database. A manual
// flow is never reported as enabled.
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

// normalizeTrigger checks the trigger the dialog sent. Empty means manual.
func normalizeTrigger(t string) (string, error) {
	switch t = strings.ToLower(strings.TrimSpace(t)); t {
	case "":
		return triggerManual, nil
	case triggerManual, triggerSchedule, triggerJoin:
		return t, nil
	}
	return "", fmt.Errorf("unknown trigger %q (use manual, schedule or join)", t)
}

// cronExamples follows every schedule error, so the dialog shows what a
// valid schedule looks like.
const cronExamples = `Enter a cron expression such as:
0 6 * * *  - every day at 06:00
0 */6 * * *  - every 6 hours (00:00, 06:00, 12:00, 18:00)
30 4 * * 1,4  - Mondays and Thursdays at 04:30
0 5 * * 1-5  - weekdays at 05:00`

// checkCronSchedule validates the schedule of a schedule-triggered flow.
// Anything triggerOf wouldn't read back as a cron schedule is rejected.
func checkCronSchedule(schedule string) error {
	if triggerOf(schedule) != triggerSchedule {
		return errors.New("Schedule: " + cronExamples)
	}
	if _, err := parseCron(schedule); err != nil {
		return errors.New("Schedule: " + err.Error() + ".\n" + cronExamples)
	}
	return nil
}

// playerVarRe finds the per-player variables only join flows can fill in.
var playerVarRe = regexp.MustCompile(`\$(player|name)\b`)

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

// checkTrigger checks that a flow's script and schedule fit its (already
// normalized) trigger. Errors are user-facing messages.
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

// flowsWithNextRun lists every flow and fills NextRun and LastRun for the UI.
func flowsWithNextRun() ([]Flow, error) {
	flows, err := globalDatabase.ListFlows()
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
		default:
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
// stores it. Every error it returns is a user-facing message.
//
// New flows are saved disabled, manual flows are never enabled, and an edit
// that changes a flow's trigger saves it disabled, so switching an enabled
// restart flow to "On player join" can't make it fire without pressing Enable.
func validateAndSaveFlow(f Flow) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if utf8.RuneCountInString(f.Name) > maxFlowNameLen {
		return 0, fmt.Errorf("Name: %d characters max", maxFlowNameLen)
	}
	// An import (or a bare API call) may only have a schedule; it says what the trigger is.
	if strings.TrimSpace(f.Trigger) == "" {
		f.Trigger = triggerOf(f.Schedule)
	}
	trig, err := normalizeTrigger(f.Trigger)
	if err != nil {
		return 0, err
	}
	f.Trigger = trig
	f.Script = normalizeScript(f.Script)
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return 0, err
	}
	if err := checkTrigger(f, pf); err != nil {
		return 0, err
	}
	f.Schedule = storedSchedule(f)
	if f.Trigger == triggerManual || f.ID == 0 {
		f.ScheduleEnabled = false
	} else if old, err := globalDatabase.GetFlow(f.ID); err == nil && old.Trigger != f.Trigger {
		f.ScheduleEnabled = false
	}
	id, err := globalDatabase.SaveFlow(f)
	if err != nil {
		return 0, errors.New("could not save (is the name already used?)")
	}
	return id, nil
}

// checkFlowCanEnable reports why a flow can't be enabled, if it can't.
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
// the trigger: what it means, the next few run times, when the next countdown
// starts, and how the script's offsets fit around it.
func previewFlow(trigger, schedule, script string) map[string]any {
	trig, err := normalizeTrigger(trigger)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	// Script mistakes (an if with no end, a variable never set, ...) show in
	// red while typing, with the line number.
	pf, perr := parseFlowScript(script)
	if perr != nil && strings.TrimSpace(script) != "" {
		return map[string]any{"ok": false, "error": "Script: " + perr.Error()}
	}
	resp := map[string]any{"ok": true}
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
				resp["span"] = strings.ReplaceAll(pf.span(), "the scheduled time", "the moment the player joins")
			}
		}
		return resp
	}

	// Schedule trigger.
	if err := checkCronSchedule(schedule); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if perr == nil {
		resp["span"] = pf.span()
		if usesPlayerVars(script) {
			resp["span"] = playerNote
		}
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

// flowRun is the live (or last finished) run of one flow. A scheduled or
// manual run has one job; a join run has one job per player in progress, all
// sharing this run's log, context and Stop button. Mutable fields are guarded
// by flowRuns' mutex.
type flowRun struct {
	flowID  int64
	name    string
	status  string // running | done | stopped | error
	active  bool
	failed  bool      // a command failed; the run ends as "error"
	stopped bool      // the Stop button was pressed
	anchor  time.Time // the 0 point (moves later when a late step pushes the run back)
	lastRun time.Time // when the flow was last started (join flows: last player)
	jobs    int       // jobs still working through the steps
	log     []string
	ctx     context.Context
	cancel  context.CancelFunc
}

// flowRuns holds each flow's current or last run, in memory only.
var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
}{byFlow: map[int64]*flowRun{}}

var (
	errFlowRunning    = errors.New("flow is already running")
	errFlowNotRunning = errors.New("flow is not running")
	errFlowBusy       = errors.New("stop the running flow first")
)

// The log line each kind of run starts with is "Started by <source>".
const (
	sourceManual   = "manual run"
	sourceSchedule = "schedule"
	sourceJoin     = "player join trigger"
)

func (r *flowRun) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("flow[%s] %s", r.name, msg)
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	flowRuns.Unlock()
}

func (r *flowRun) markFailed() {
	flowRuns.Lock()
	r.failed = true
	flowRuns.Unlock()
}

// jobDone is called when one job finishes. The last one ends the run:
// "stopped" if Stop was pressed, "error" if a command failed, else "done".
func (r *flowRun) jobDone(stopped bool) {
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
// immediately; otherwise the 0 point is `minutes` from now and steps that
// would already be in the past are skipped.
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
	return launch(f, pf, time.Now().Add(time.Duration(lead)*time.Second), sourceManual, nil)
}

// stopFlow cancels the active run of a flow (the Stop button).
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
func deleteFlow(id int64) error {
	flowRuns.Lock()
	if run := flowRuns.byFlow[id]; run != nil && run.active {
		flowRuns.Unlock()
		return errFlowBusy
	}
	delete(flowRuns.byFlow, id)
	flowRuns.Unlock()
	return globalDatabase.DeleteFlow(id)
}

// launch starts a run of f around the given 0 point. p is the joining player
// for join flows (nil otherwise); a join adds a job to the flow's active run
// if there is one, while any other start is refused with errFlowRunning.
func launch(f Flow, pf *parsedFlow, anchor time.Time, source string, p *Player) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[f.ID]
	active := run != nil && run.active
	if active && p == nil {
		flowRuns.Unlock()
		return errFlowRunning
	}
	if !active {
		ctx, cancel := context.WithCancel(context.Background())
		run = &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true, anchor: anchor, ctx: ctx, cancel: cancel}
		flowRuns.byFlow[f.ID] = run
	}
	run.jobs++
	// Last run is when the first step is due: a scheduled run is started up
	// to one tick early (see launchDue) and sleeps until then.
	run.lastRun = time.Now()
	if firstAt := anchor.Add(time.Duration(pf.Steps[0].OffsetSecs) * time.Second); firstAt.After(run.lastRun) {
		run.lastRun = firstAt
	}
	flowRuns.Unlock()
	if !active {
		run.logf("Started by %s", source)
	}
	go func() { run.jobDone(runSteps(run, pf, anchor, p)) }()
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

// runSteps works through one job's steps and reports whether it was stopped.
// Steps whose time had already passed when the job started are skipped (a
// scheduled run joined mid-window). Cancel steps run only when a countdown is
// stopped before its 0 point, which can't happen for a join job.
//
// What each step does is in the script-language section above; the job's
// variables live in one runCtx for the whole job.
func runSteps(run *flowRun, pf *parsedFlow, anchor time.Time, p *Player) (stopped bool) {
	if p != nil {
		run.logf("player %d (%s) joined", p.ID, p.Name)
	}
	x := newRunCtx(run, p)
	started := time.Now()
	clock := &stepClock{base: anchor}
	for _, st := range pf.Steps {
		fireAt := clock.at(st.OffsetSecs)
		if fireAt.Before(started.Add(-scheduleGrace)) {
			run.logf("skipped %s (its time had already passed)", st.label())
			continue
		}
		if !sleepUntil(run.ctx, fireAt) {
			if p != nil {
				run.logf("stopped for player %d (%s)", p.ID, p.Name)
				return true
			}
			run.logf("stopped")
			if time.Now().Before(clock.base) {
				for _, c := range pf.OnCancel {
					x.runStep(c)
				}
			}
			return true
		}
		if late := clock.fired(st.OffsetSecs); late > 0 {
			run.logf("%s is %.1fs late; later steps move back %.1fs to keep their spacing", st.label(), late.Seconds(), late.Seconds())
			if p == nil {
				flowRuns.Lock()
				run.anchor = clock.base // keeps the page's countdown right
				flowRuns.Unlock()
			}
		}
		x.runStep(st)
	}
	if p == nil {
		run.logf("done")
	}
	return false
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// scheduleGrace lets a step whose time just passed (a late tick, a restart a
// few seconds into a window) still run instead of being skipped.
const scheduleGrace = 15 * time.Second

// scheduleTick is how often the scheduler checks for runs to start.
const scheduleTick = 5 * time.Second

// lastFired is the latest cron occurrence started for each flow, so a
// finished or stopped run isn't started again inside the same window. Only
// the scheduler goroutine touches it.
var lastFired = map[int64]time.Time{}

// launchDue reports whether a run whose first step is due at firstAt should
// be started at this tick. It looks one tick ahead: a run due before the next
// tick is started now and sleeps until the exact second, so the first step
// isn't up to one tick late.
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

// runScheduleTick starts, for every enabled schedule flow, the first cron
// occurrence that hasn't been started yet and whose window (cron time +
// earliest offset ... cron time + latest offset) is open or opens before the
// next tick. If the app was down when the window opened it joins the run in
// progress; steps already in the past are skipped.
func runScheduleTick(now time.Time) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != triggerSchedule {
			continue // disabled, manual, or a player-join flow
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

		// The first occurrence whose last step hasn't passed yet, skipping
		// any already started.
		from := now.Add(-last - scheduleGrace)
		if lf, ok := lastFired[f.ID]; ok && lf.After(from) {
			from = lf
		}
		t, ok := c.next(from)
		if !ok || !launchDue(now, t.Add(first)) {
			continue
		}
		if err := launch(f, pf, t, sourceSchedule, nil); errors.Is(err, errFlowRunning) {
			continue // busy: retry next tick
		} else if err != nil {
			log.Printf("scheduler: %s: %v", f.Name, err)
		}
		lastFired[f.ID] = t
	}
}

// ---------------------------------------------------------------------------
// Player-join trigger
// ---------------------------------------------------------------------------

// lobbySuffix is what BattlEye appends to a name while the player is still on
// the loading screen.
const lobbySuffix = "(Lobby)"

func inLobby(p Player) bool {
	f := strings.Fields(p.Name)
	return len(f) > 0 && strings.EqualFold(f[len(f)-1], lobbySuffix)
}

// joinWatch remembers who was online at the last successful poll, by GUID
// (player numbers get reused). Only the poller goroutine (db.go) touches it.
var joinWatch = struct {
	seen   map[string]bool
	primed bool // false until the first successful poll after startup
}{seen: map[string]bool{}}

// playersPolled is called by the poller with every successful `players`
// result and starts the join flows for new arrivals. The first call after
// startup only records who is online, so a restart doesn't greet everybody.
func playersPolled(players []Player) {
	if joined := newArrivals(players); len(joined) > 0 {
		startJoinFlows(joined)
	}
}

// newArrivals updates joinWatch and returns the players who have just
// finished joining. A "(Lobby)" player is still loading, so they're left out
// until the tag is gone; one already counted stays counted.
func newArrivals(players []Player) []Player {
	online := map[string]bool{}
	var joined []Player
	for _, p := range players {
		if p.GUID == "" || (inLobby(p) && !joinWatch.seen[p.GUID]) {
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

// playersLost is called when the game server stops answering. Everyone is
// forgotten, so players are greeted again when they reconnect.
func playersLost() {
	joinWatch.seen = map[string]bool{}
}

// startJoinFlows runs every enabled join flow once for each player.
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
		for i := range joined {
			launch(f, pf, time.Now(), sourceJoin, &joined[i])
		}
	}
}