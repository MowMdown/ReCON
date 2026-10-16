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
// flows.go overview (flows v2)
// ----------------------------------------------------------------------------
// A "flow" is a small script that runs once per second ("tick"). Every line
// that sends something to the server takes one tick; `wait 10s` takes ten;
// $variable lines take none. The full design is claude/flows-v2-design.md.
//
// Each flow can have a Timer, and $timer holds the seconds left on it:
//
//	no Timer   the script runs from the top when the flow is triggered
//	delay      wait out the Timer, then run from the top
//	           (manual and join flows; scheduled flows set to "after")
//	countdown  scheduled flows set to "before": the run starts Timer before
//	           the cron time and $timer counts down to 0 at it. Only here can
//	           timed lines be used:
//
//	$warn = 60m 30m 15m 10m 5m 3m 2m
//	$warn | say -1 Server is going to restart in @minutes minutes!
//	2m    | if $players > 0
//	  say -1 Locking the server now.
//	  #lock
//	end
//	0     | #shutdown
//
// `5m | ...` is shorthand for `if $timer == 5m ... end`. Commands sent when
// Stop is pressed live in the flow's separate Cancel field.
//
// Sections: time values, parsing, values and conditions, the runner (ticks,
// sequences, sending commands), save-time analysis and preview, cron, flow
// definitions and validation, run bookkeeping, the scheduler, and the
// player-join trigger.
// ============================================================================

// ---------------------------------------------------------------------------
// Time values: 5s, 10m, 1h, 1m30s (and a bare 0)
// ---------------------------------------------------------------------------

// maxTimeSecs is the longest time a flow can use (Timer, offsets, waits).
// It matches the 24 hour safety limit on a run.
const maxTimeSecs = 86400

var (
	timeRe    = regexp.MustCompile(`(?i)^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)
	numberRe  = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	varNameRe = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	varRe     = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	assignRe  = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)
	// timeLeftRe finds the @ words. Plurals come first so @hours isn't read
	// as @hour followed by "s".
	timeLeftRe = regexp.MustCompile(`@(hours|minutes|seconds|hour|minute|second)\b`)
	// offsetLikeRe is what can stand left of "|" in a timed line. Signed
	// offsets and "cancel" are matched so they get a helpful error.
	offsetLikeRe = regexp.MustCompile(`(?i)^(cancel|[+-]?\s*\d[\dhms\s]*|\$[A-Za-z_][A-Za-z0-9_]*)$`)
)

// timeLit reads a time with units or a bare "0", as total seconds. Units
// can be combined, largest first: "90s", "5m", "1h", "1m30s", "1h3m15s"
// (= 3795).
func timeLit(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, true
	}
	m := timeRe.FindStringSubmatch(s)
	if m == nil || s == "" {
		return 0, false
	}
	secs := 0
	for i, mul := range []int{3600, 60, 1} {
		if m[i+1] != "" {
			n, err := strconv.Atoi(m[i+1])
			if err != nil || n > maxTimeSecs {
				return maxTimeSecs + 1, true
			}
			secs += n * mul
		}
	}
	return secs, true
}

// parseTime reads a time where one is required, with a user-facing error.
func parseTime(s string) (int, error) {
	s = strings.TrimSpace(s)
	secs, ok := timeLit(s)
	switch {
	case !ok && numberRe.MatchString(s):
		return 0, fmt.Errorf("%q needs a unit, such as %ss or %sm", s, s, s)
	case !ok:
		return 0, fmt.Errorf("%q is not a time (examples: 30s, 5m, 1h, 1m30s)", s)
	case secs > maxTimeSecs:
		return 0, fmt.Errorf("%q is too long (24h at most)", s)
	}
	return secs, nil
}

// fmtLeft writes a number of seconds the way scripts do: 1h, 60m is shown as
// 1h, 90s as 1m30s, 0 as 0.
func fmtLeft(secs int) string {
	if secs == 0 {
		return "0"
	}
	var b strings.Builder
	if h := secs / 3600; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m := secs % 3600 / 60; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s := secs % 60; s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// fillTimeLeft replaces the @ words with the time left on $timer.
func fillTimeLeft(s string, timer int) string {
	if timer < 0 {
		timer = 0
	}
	return timeLeftRe.ReplaceAllStringFunc(s, func(m string) string {
		switch strings.ToLower(m[1:]) {
		case "hours", "hour":
			return strconv.Itoa(timer / 3600)
		case "minutes":
			return strconv.Itoa(timer / 60)
		case "seconds":
			return strconv.Itoa(timer)
		case "minute":
			return fmt.Sprintf("%02d", timer%3600/60)
		}
		return fmt.Sprintf("%02d", timer%60) // second
	})
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type nodeKind int

const (
	nodeCmd  nodeKind = iota // an RCON command
	nodeSet                  // $var = value
	nodeIf                   // if / else / end, and the wrapper of a timed line
	nodeDo                   // do ... end: a group of lines
	nodeEach                 // for each player ... end
	nodeWait                 // wait <time>
	nodeStop                 // stop: end the run now
)

type flowNode struct {
	kind  nodeKind
	line  int
	text  string    // the command, or the value of a $var = value line
	name  string    // the variable, for nodeSet
	cond  *flowCond // for nodeIf (nil on a timed line's wrapper)
	at    *timedAt  // set on a timed line's wrapper: run when $timer is one of these
	then  []*flowNode
	els   []*flowNode
	ticks int  // for nodeWait
	open  bool // parse-time: still waiting for its "end"
	inEls bool // parse-time: lines now go to els
}

// timedAt is the left side of a timed line: fixed seconds ("5m | ...") or a
// list variable ("$warn | ...").
type timedAt struct {
	secs    []int
	listVar string
	text    string // as written, for log lines
}

type varUse struct {
	name   string
	line   int
	inEach bool // inside for each player
}

type parsedFlow struct {
	Top []*flowNode // the script's top-level lines, in order

	setup      []*flowNode      // leading $var = value lines, run before the first tick
	lists      map[string][]int // lists set from fixed times at the top level
	timedLines []int            // lines with "offset |"
	timerUses  []int            // lines that use $timer at all
	timerSets  []int            // lines that assign $timer
	playerUses []varUse         // $player, $name, $ip, $guid, $ping
}

// isWatcher reports whether a top-level node is checked on every tick
// rather than run once when the top-to-bottom sequence reaches it. Timed
// lines always are; in a countdown, so is every top-level if. In other flows
// a top-level if runs in its place, like any other line.
func isWatcher(n *flowNode, countdown bool) bool {
	return n.kind == nodeIf && (n.at != nil || countdown)
}

// Variables the program fills in. Only $timer can be set by a script (and
// only in a countdown).
var reservedVars = map[string]bool{"timer": true, "players": true, "player": true, "name": true, "ip": true, "guid": true, "ping": true}

var playerVars = map[string]bool{"player": true, "name": true, "ip": true, "guid": true, "ping": true}

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

// parseFlowScript turns a script into nodes. Every error names the line.
// Checks that depend on the flow's trigger and Timer are in analyzeFlow.
func parseFlowScript(script string) (*parsedFlow, error) {
	pf := &parsedFlow{lists: map[string][]int{}}
	var stack []*flowNode // open blocks, innermost last
	assigned := map[string]bool{}
	var condVars []varUse
	listNames := map[string]bool{} // every variable ever set to a list
	type textUse struct {
		name string
		line int
	}
	var textVars []textUse
	eachDepth := 0

	add := func(n *flowNode) {
		if len(stack) == 0 {
			pf.Top = append(pf.Top, n)
			return
		}
		top := stack[len(stack)-1]
		if top.inEls {
			top.els = append(top.els, n)
		} else {
			top.then = append(top.then, n)
		}
	}
	push := func(n *flowNode) {
		n.open = true
		if n.kind == nodeEach {
			eachDepth++
		}
		stack = append(stack, n)
	}
	noteVars := func(s string, ln int, inCond bool) {
		for _, m := range varRe.FindAllStringSubmatch(s, -1) {
			name := strings.ToLower(m[1])
			if name == "timer" {
				pf.timerUses = append(pf.timerUses, ln)
			}
			if playerVars[name] {
				pf.playerUses = append(pf.playerUses, varUse{name, ln, eachDepth > 0})
			}
			if inCond {
				condVars = append(condVars, varUse{name, ln, eachDepth > 0})
			}
		}
	}

	// block reads a line that opens a block ("if ...", "do", "for each
	// player"); ok is false if the line isn't one.
	block := func(text string, ln int) (*flowNode, bool, error) {
		low := strings.ToLower(text)
		switch {
		case low == "if" || strings.HasPrefix(low, "if "):
			c, err := parseCond(strings.TrimSpace(text[2:]))
			if err != nil {
				return nil, true, fmt.Errorf("line %d: %v", ln, err)
			}
			for _, and := range c.any {
				for _, cmp := range and {
					noteVars(cmp.left+" "+cmp.right, ln, true)
					if err := checkTimerCmp(cmp); err != nil {
						return nil, true, fmt.Errorf("line %d: %v", ln, err)
					}
				}
			}
			return &flowNode{kind: nodeIf, line: ln, cond: c}, true, nil
		case low == "do":
			return &flowNode{kind: nodeDo, line: ln}, true, nil
		case strings.Join(strings.Fields(low), " ") == "for each player":
			return &flowNode{kind: nodeEach, line: ln}, true, nil
		case strings.HasPrefix(low, "for ") || low == "for" || strings.HasPrefix(low, "while ") || low == "while":
			return nil, true, fmt.Errorf("line %d: only \"for each player\" is supported for now", ln)
		case strings.HasPrefix(low, "do "):
			return nil, true, fmt.Errorf("line %d: \"do\" goes on a line of its own, with the lines below it and an \"end\"", ln)
		}
		return nil, false, nil
	}

	// simple reads a line that isn't a block keyword: wait, stop, $var =
	// value, or a command.
	simple := func(text string, ln int) (*flowNode, error) {
		low := strings.ToLower(text)
		switch {
		case low == "wait" || strings.HasPrefix(low, "wait "):
			arg := strings.TrimSpace(text[4:])
			if arg == "" {
				return nil, fmt.Errorf("line %d: wait needs a time, such as wait 10s", ln)
			}
			secs, err := parseTime(arg)
			if err != nil {
				return nil, fmt.Errorf("line %d: wait: %v", ln, err)
			}
			if secs == 0 {
				return nil, fmt.Errorf("line %d: wait 0 does nothing; use a time such as wait 5s", ln)
			}
			return &flowNode{kind: nodeWait, line: ln, ticks: secs}, nil
		case low == "stop":
			return &flowNode{kind: nodeStop, line: ln}, nil
		case low == "else" || strings.HasPrefix(low, "else "):
			return nil, fmt.Errorf("line %d: \"else\" goes on a line of its own, inside an if", ln)
		case low == "end" || strings.HasPrefix(low, "end "):
			return nil, fmt.Errorf("line %d: \"end\" goes on a line of its own", ln)
		}
		if m := assignRe.FindStringSubmatch(text); m != nil {
			name := strings.ToLower(m[1])
			val := strings.TrimSpace(m[2])
			if strings.HasPrefix(val, "=") {
				return nil, fmt.Errorf("line %d: == compares, so it goes in an if (use a single = to set a variable)", ln)
			}
			if val == "" {
				return nil, fmt.Errorf("line %d: $%s = needs a value", ln, name)
			}
			if reservedVars[name] && name != "timer" {
				return nil, fmt.Errorf("line %d: $%s is filled in automatically and can't be set", ln, name)
			}
			list, isList, err := listLit(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", ln, err)
			}
			if name == "timer" {
				pf.timerSets = append(pf.timerSets, ln)
				pf.timerUses = append(pf.timerUses, ln)
				if isList {
					return nil, fmt.Errorf("line %d: $timer holds one time, not a list", ln)
				}
				if numberRe.MatchString(val) && val != "0" {
					return nil, fmt.Errorf("line %d: %q needs a unit, such as $timer = %sm", ln, val, val)
				}
			}
			if isList {
				listNames[name] = true
				if len(stack) == 0 {
					pf.lists[name] = list
				}
			}
			noteVars(val, ln, false)
			assigned[name] = true
			return &flowNode{kind: nodeSet, line: ln, name: name, text: val}, nil
		}
		noteVars(text, ln, false)
		for _, m := range varRe.FindAllStringSubmatch(text, -1) {
			textVars = append(textVars, textUse{strings.ToLower(m[1]), ln})
		}
		return &flowNode{kind: nodeCmd, line: ln, text: cleanReason(text)}, nil
	}

	for i, raw := range strings.Split(script, "\n") {
		ln := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		low := strings.ToLower(line)

		// Timed line: "<offset> | ..." or "$list | ...". A "||" in a
		// condition never looks like this, because what's before it isn't
		// an offset.
		if when, rest, ok := strings.Cut(line, "|"); ok && !strings.HasPrefix(rest, "|") {
			w := strings.TrimSpace(when)
			if offsetLikeRe.MatchString(w) {
				at, err := parseTimedAt(w)
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", ln, err)
				}
				if len(stack) > 0 {
					return nil, fmt.Errorf("line %d: a timed line can't go inside a block; put the block under the time instead (%s | if ...)", ln, w)
				}
				if at.listVar != "" {
					condVars = append(condVars, varUse{at.listVar, ln, false})
				}
				rest = strings.TrimSpace(rest)
				if rest == "" {
					return nil, fmt.Errorf("line %d: %s | needs a command after it", ln, w)
				}
				pf.timedLines = append(pf.timedLines, ln)
				pf.timerUses = append(pf.timerUses, ln)
				wrap := &flowNode{kind: nodeIf, line: ln, at: at}
				add(wrap)
				inner, isBlock, err := block(rest, ln)
				if err != nil {
					return nil, err
				}
				if isBlock {
					wrap.then = []*flowNode{inner}
					push(inner)
					continue
				}
				rl := strings.ToLower(rest)
				if rl == "wait" || strings.HasPrefix(rl, "wait ") || rl == "stop" {
					return nil, fmt.Errorf("line %d: %q can't be timed; put it inside a do block (%s | do ... end)", ln, rest, w)
				}
				n, err := simple(rest, ln)
				if err != nil {
					return nil, err
				}
				wrap.then = []*flowNode{n}
				continue
			}
		}

		switch {
		case low == "else":
			if len(stack) == 0 || stack[len(stack)-1].kind != nodeIf {
				return nil, fmt.Errorf("line %d: \"else\" without an \"if\"", ln)
			}
			top := stack[len(stack)-1]
			if top.inEls {
				return nil, fmt.Errorf("line %d: second \"else\" for the \"if\" on line %d", ln, top.line)
			}
			top.inEls = true
			continue
		case low == "end":
			if len(stack) == 0 {
				return nil, fmt.Errorf("line %d: \"end\" without an \"if\", \"do\" or \"for each player\"", ln)
			}
			top := stack[len(stack)-1]
			top.open = false
			if top.kind == nodeEach {
				eachDepth--
			}
			stack = stack[:len(stack)-1]
			continue
		case strings.HasPrefix(low, "else "):
			return nil, fmt.Errorf("line %d: \"else\" goes on a line of its own (for else-if, put an if inside the else)", ln)
		}

		n, isBlock, err := block(line, ln)
		if err != nil {
			return nil, err
		}
		if isBlock {
			add(n)
			push(n)
			continue
		}
		if n, err = simple(line, ln); err != nil {
			return nil, err
		}
		add(n)
	}
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		return nil, fmt.Errorf("line %d: this block has no \"end\"", top.line)
	}
	if len(pf.Top) == 0 {
		return nil, errors.New("the script has no lines yet")
	}
	for _, v := range condVars {
		if !reservedVars[v.name] && !assigned[v.name] {
			return nil, fmt.Errorf("line %d: unknown variable $%s (set it first with $%s = ...)", v.line, v.name, v.name)
		}
	}
	for _, v := range textVars {
		if listNames[v.name] {
			return nil, fmt.Errorf("line %d: $%s is a list; use @minutes for the time left, or use the list as an offset or with ==", v.line, v.name)
		}
	}
	for _, n := range pf.Top {
		if n.kind != nodeSet {
			break
		}
		pf.setup = append(pf.setup, n)
	}
	return pf, nil
}

// parseTimedAt reads the left side of a timed line.
func parseTimedAt(w string) (*timedAt, error) {
	at := &timedAt{text: w}
	switch {
	case strings.EqualFold(w, "cancel"):
		return nil, errors.New("cancel lines have moved to the Cancel field of the flow")
	case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+"):
		bare := strings.TrimSpace(w[1:])
		return nil, fmt.Errorf("write %s: time left, no sign", bare)
	case strings.HasPrefix(w, "$"):
		at.listVar = strings.ToLower(w[1:])
		if at.listVar == "timer" || reservedVars[at.listVar] {
			return nil, fmt.Errorf("$%s can't be used as a time here", at.listVar)
		}
		return at, nil
	}
	secs, err := parseTime(strings.ReplaceAll(w, " ", ""))
	if err != nil {
		return nil, err
	}
	at.secs = []int{secs}
	return at, nil
}

// listLit reports whether a value is a list of times ("60m 30m 15m"). The
// list is sorted greatest first; equal values keep their written order.
func listLit(val string) ([]int, bool, error) {
	f := strings.Fields(val)
	if len(f) < 2 {
		return nil, false, nil
	}
	allNum := true
	for _, tok := range f {
		if _, ok := timeLit(tok); !ok && !numberRe.MatchString(tok) {
			allNum = false
			break
		}
	}
	if !allNum {
		return nil, false, nil
	}
	out := make([]int, 0, len(f))
	for _, tok := range f {
		secs, err := parseTime(tok)
		if err != nil {
			return nil, false, fmt.Errorf("list: %v", err)
		}
		out = append(out, secs)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a] > out[b] })
	return out, true, nil
}

// checkTimerCmp rejects a bare number compared with $timer ($timer == 5).
func checkTimerCmp(c flowCmp) error {
	l, r := strings.ToLower(c.left), strings.ToLower(c.right)
	other := ""
	switch {
	case l == "$timer":
		other = c.right
	case r == "$timer":
		other = c.left
	default:
		return nil
	}
	if numberRe.MatchString(other) && other != "0" {
		return fmt.Errorf("%q needs a unit, such as $timer %s %sm", other, c.op, other)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Conditions
// ---------------------------------------------------------------------------

type flowCmp struct{ left, op, right string } // op "" = the left side alone is tested (if $quick)

// flowCond is any of several all-of groups: (a && b) || (c) ...
type flowCond struct {
	text string
	any  [][]flowCmp
}

var cmpOps = []string{"==", "!=", "<=", ">=", "<", ">"}

func parseCond(s string) (*flowCond, error) {
	if s == "" {
		return nil, errors.New("\"if\" needs a condition, e.g. if $players > 0")
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
	if len(strings.Fields(s)) != 1 {
		return flowCmp{}, fmt.Errorf("%q needs a comparison: == != < <= > >=", s)
	}
	return flowCmp{left: s}, nil // a lone value: if $quick
}

// unquote drops one pair of surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

type valKind int

const (
	vStr valKind = iota
	vNum
	vTime // seconds
	vList // times in seconds, greatest first
	vBool
)

type value struct {
	kind valKind
	s    string
	n    float64
	list []int
	b    bool
}

func strVal(s string) value  { return value{kind: vStr, s: s} }
func numVal(n float64) value { return value{kind: vNum, n: n} }
func timeVal(s int) value    { return value{kind: vTime, n: float64(s)} }

func (v value) text() string {
	switch v.kind {
	case vNum, vTime:
		return strconv.FormatFloat(v.n, 'f', -1, 64)
	case vBool:
		return strconv.FormatBool(v.b)
	case vList:
		parts := make([]string, len(v.list))
		for i, s := range v.list {
			parts[i] = fmtLeft(s)
		}
		return strings.Join(parts, " ")
	}
	return v.s
}

// number reports a value's numeric form, if it has one.
func (v value) number() (float64, bool) {
	switch v.kind {
	case vNum, vTime:
		return v.n, true
	case vStr:
		if numberRe.MatchString(strings.TrimSpace(v.s)) {
			n, _ := strconv.ParseFloat(strings.TrimSpace(v.s), 64)
			return n, true
		}
		if secs, ok := timeLit(v.s); ok {
			return float64(secs), true
		}
	}
	return 0, false
}

func (v value) truthy() bool {
	switch v.kind {
	case vBool:
		return v.b
	case vNum, vTime:
		return v.n != 0
	case vList:
		return len(v.list) > 0
	}
	s := strings.ToLower(strings.TrimSpace(v.s))
	return s != "" && s != "false" && s != "0"
}

// ---------------------------------------------------------------------------
// Runner: one job is one run of a flow (or one player, for join flows)
// ---------------------------------------------------------------------------

// job holds a run's state between ticks. Variables start fresh every run.
type job struct {
	run       *flowRun
	pf        *parsedFlow
	p         *Player // the joining player, join flows only
	vars      map[string]value
	countdown bool
	timer     int  // $timer: seconds left in a countdown; 0 otherwise
	newTimer  *int // a $timer assignment, applied from the next tick
	halted    bool // a stop line ran

	plain    *seq              // the top-level lines that aren't watchers (see isWatcher)
	watching map[int]*seq      // a running block started by Top[i]
	plainIdx map[*flowNode]int // top-level index of each plain node
}

// seq walks through a list of lines, one command per tick.
type seq struct {
	x      *job
	frames []*frame
	wait   int // ticks still to wait
}

type frame struct {
	nodes   []*flowNode
	i       int
	players []Player // for each player: the players being gone through
	pi      int
}

func newJob(run *flowRun, pf *parsedFlow, p *Player, countdown bool, timer int) *job {
	x := &job{run: run, pf: pf, p: p, vars: map[string]value{}, countdown: countdown, timer: timer,
		watching: map[int]*seq{}, plainIdx: map[*flowNode]int{}}
	var plain []*flowNode
	for i, n := range pf.Top {
		if !isWatcher(n, countdown) {
			x.plainIdx[n] = i
			plain = append(plain, n)
		}
	}
	// The setup lines at the top run before the first tick.
	if len(pf.setup) > 0 {
		s := &seq{x: x, frames: []*frame{{nodes: pf.setup}}}
		s.step()
		plain = plain[len(pf.setup):]
	}
	if len(plain) > 0 {
		x.plain = &seq{x: x, frames: []*frame{{nodes: plain}}}
	}
	return x
}

func (s *seq) done() bool { return len(s.frames) == 0 && s.wait == 0 }

// pos is the top-level index of the line the plain sequence is on: the next
// line to run, or the line whose block or wait it is still in. -1 when done.
func (s *seq) pos() int {
	if len(s.frames) == 0 {
		return -1
	}
	b := s.frames[0]
	switch {
	case (len(s.frames) > 1 || s.wait > 0) && b.i > 0:
		return s.x.plainIdx[b.nodes[b.i-1]]
	case b.i < len(b.nodes):
		return s.x.plainIdx[b.nodes[b.i]]
	case b.i > 0:
		return s.x.plainIdx[b.nodes[b.i-1]]
	}
	return -1
}

// step runs this sequence's part of one tick: lines that take no time, then
// one command (or one tick of a wait). It returns when the tick is used up or
// the sequence has finished.
func (s *seq) step() {
	for !s.x.halted {
		if s.wait > 0 {
			if s.wait--; s.wait == 0 {
				s.trim()
			}
			return
		}
		if len(s.frames) == 0 {
			return
		}
		f := s.frames[len(s.frames)-1]
		if f.i >= len(f.nodes) {
			if f.players != nil && f.pi+1 < len(f.players) {
				f.pi++
				f.i = 0
				continue
			}
			s.frames = s.frames[:len(s.frames)-1]
			continue
		}
		n := f.nodes[f.i]
		f.i++
		switch n.kind {
		case nodeCmd:
			s.x.send(s.fill(n.text))
			s.trim()
			return
		case nodeSet:
			s.x.set(n, s.eval(n.text))
		case nodeWait:
			s.wait = n.ticks
		case nodeStop:
			s.x.run.logf("stop (line %d)", n.line)
			s.x.halted = true
			return
		case nodeDo:
			s.frames = append(s.frames, &frame{nodes: n.then})
		case nodeIf:
			if n.at != nil { // a timed line reached inside a sequence: already matched
				s.frames = append(s.frames, &frame{nodes: n.then})
				continue
			}
			ok := s.evalCond(n.cond)
			s.x.run.logf("if %s -> %v (line %d)", n.cond.text, ok, n.line)
			if ok {
				s.frames = append(s.frames, &frame{nodes: n.then})
			} else if len(n.els) > 0 {
				s.frames = append(s.frames, &frame{nodes: n.els})
			}
		case nodeEach:
			ps := s.x.onlinePlayers()
			s.x.run.logf("for each player: %d online (line %d)", len(ps), n.line)
			if len(ps) > 0 {
				s.frames = append(s.frames, &frame{nodes: n.then, players: ps})
			}
		}
	}
}

// trim drops finished blocks, so a sequence whose last line has just gone
// out counts as done straight away.
func (s *seq) trim() {
	for len(s.frames) > 0 {
		f := s.frames[len(s.frames)-1]
		if f.i < len(f.nodes) || (f.players != nil && f.pi+1 < len(f.players)) {
			return
		}
		s.frames = s.frames[:len(s.frames)-1]
	}
}

// tick runs one second of the job: every if and timed line is checked and
// every running sequence moves on, in script order.
func (x *job) tick() {
	plainDone := x.plain == nil || x.plain.done()
	plainAt := -1
	if !plainDone {
		plainAt = x.plain.pos()
	}
	for i, n := range x.pf.Top {
		if x.halted {
			return
		}
		if !plainDone && i == plainAt {
			x.plain.step()
			plainDone = true // at most once per tick
		}
		if !isWatcher(n, x.countdown) {
			continue
		}
		if s := x.watching[i]; s != nil {
			s.step()
			if s.done() {
				delete(x.watching, i)
			}
			continue
		}
		var body []*flowNode
		if n.at != nil {
			if x.atMatches(n.at) {
				x.run.logf("%s | (line %d)", n.at.text, n.line)
				body = n.then
			}
		} else {
			probe := &seq{x: x}
			if probe.evalCond(n.cond) {
				x.run.logf("if %s -> true (line %d)", n.cond.text, n.line)
				body = n.then
			} else {
				body = n.els
			}
		}
		if len(body) == 0 {
			continue
		}
		s := &seq{x: x, frames: []*frame{{nodes: body}}}
		s.step()
		if !s.done() {
			x.watching[i] = s
		}
	}
	if !plainDone && !x.halted {
		x.plain.step()
	}
}

// idle reports whether nothing is left to send (non-countdown runs end then).
func (x *job) idle() bool {
	return (x.plain == nil || x.plain.done()) && len(x.watching) == 0
}

// unsent counts sequences still going, for the log when a countdown ends.
func (x *job) unsent() int {
	n := len(x.watching)
	if x.plain != nil && !x.plain.done() {
		n++
	}
	return n
}

func (x *job) atMatches(at *timedAt) bool {
	if at.listVar != "" {
		v, ok := x.vars[at.listVar]
		if !ok {
			return false
		}
		switch v.kind {
		case vList:
			for _, s := range v.list {
				if s == x.timer {
					return true
				}
			}
			return false
		}
		n, ok := v.number()
		return ok && int(n) == x.timer
	}
	for _, s := range at.secs {
		if s == x.timer {
			return true
		}
	}
	return false
}

func (x *job) set(n *flowNode, v value) {
	if n.name == "timer" {
		secs, ok := v.number()
		if !ok || secs < 0 {
			x.run.logf("line %d: $timer = %q is not a time (ignored)", n.line, v.text())
			return
		}
		t := int(secs)
		x.newTimer = &t
		x.run.logf("$timer = %s from the next tick (line %d)", fmtLeft(t), n.line)
		return
	}
	x.vars[n.name] = v
	x.run.logf("set $%s = %s", n.name, v.text())
}

// onlinePlayers is the poller's latest list (see playersPolled), or a fresh
// read before the poller's first result.
func (x *job) onlinePlayers() []Player {
	latest.Lock()
	ps, ok := append([]Player(nil), latest.players...), latest.ok
	latest.Unlock()
	if ok {
		return ps
	}
	out, acked, err := runShared("players", 3*time.Second, true, "")
	if err != nil || !acked {
		x.run.logf("couldn't read the player list (err=%v)", err)
		x.run.markFailed()
		return nil
	}
	return parsePlayers(out)
}

// sendCommand sends one flow command. Tests replace it.
var sendCommand = runFlowCommand

// send sends one command, with its variables already filled in.
func (x *job) send(cmd string) {
	sendCommand(x.run, cmd)
}

// player is the player the per-player variables refer to: the innermost for
// each player, else the joining player.
func (s *seq) player() *Player {
	for i := len(s.frames) - 1; i >= 0; i-- {
		if f := s.frames[i]; f.players != nil {
			return &f.players[f.pi]
		}
	}
	return s.x.p
}

// lookup returns a variable's value; ok is false if it has none, and then
// the "$name" text is left as typed.
func (s *seq) lookup(name string) (value, bool) {
	switch name {
	case "timer":
		return timeVal(s.x.timer), true
	case "players":
		return numVal(float64(len(s.x.onlinePlayers()))), true
	}
	if playerVars[name] {
		p := s.player()
		if p == nil {
			return value{}, false
		}
		switch name {
		case "player":
			return numVal(float64(p.ID)), true
		case "name":
			return strVal(p.Name), true
		case "ip":
			return strVal(p.IP), true
		case "guid":
			return strVal(p.GUID), true
		}
		return numVal(float64(p.Ping)), true
	}
	v, ok := s.x.vars[name]
	return v, ok
}

// fill expands $variables, then the @ words. Values are not expanded again,
// so a player named "$players" stays literal text.
func (s *seq) fill(text string) string {
	out := varRe.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := s.lookup(strings.ToLower(m[1:])); ok {
			return v.text()
		}
		return m
	})
	return cleanReason(fillTimeLeft(out, s.x.timer))
}

// term reads one value: $var, a time, a number, true/false, "quoted" or text.
func (s *seq) term(tok string) value {
	tok = strings.TrimSpace(tok)
	if m := varNameRe.FindStringSubmatch(tok); m != nil {
		if v, ok := s.lookup(strings.ToLower(m[1])); ok {
			return v
		}
		return strVal(tok)
	}
	if numberRe.MatchString(tok) {
		n, _ := strconv.ParseFloat(tok, 64)
		return numVal(n)
	}
	if secs, ok := timeLit(tok); ok {
		return timeVal(secs)
	}
	switch strings.ToLower(tok) {
	case "true":
		return value{kind: vBool, b: true}
	case "false":
		return value{kind: vBool, b: false}
	}
	if strings.HasPrefix(tok, `"`) {
		return strVal(unquote(tok))
	}
	return strVal(s.fill(tok))
}

// eval reads the right side of $var = ...: a list of times, a sum such as
// $timer + 5m, one value, or text.
func (s *seq) eval(expr string) value {
	if list, ok, _ := listLit(expr); ok {
		return value{kind: vList, list: list}
	}
	f := strings.Fields(expr)
	if len(f) >= 3 && len(f)%2 == 1 {
		isSum := true
		for i := 1; i < len(f); i += 2 {
			if f[i] != "+" && f[i] != "-" {
				isSum = false
			}
		}
		if isSum {
			total, anyTime := 0.0, false
			for i := 0; i < len(f); i += 2 {
				v := s.term(f[i])
				n, ok := v.number()
				if !ok {
					isSum = false
					break
				}
				anyTime = anyTime || v.kind == vTime
				if i > 0 && f[i-1] == "-" {
					n = -n
				}
				total += n
			}
			if isSum {
				if anyTime {
					return timeVal(int(total))
				}
				return numVal(total)
			}
		}
	}
	if len(f) == 1 {
		return s.term(f[0])
	}
	return strVal(unquote(s.fill(expr)))
}

func (s *seq) evalCond(c *flowCond) bool {
	for _, and := range c.any {
		all := true
		for _, cmp := range and {
			if !s.evalCmp(cmp) {
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

func (s *seq) evalCmp(c flowCmp) bool {
	l := s.eval(c.left)
	if c.op == "" {
		return l.truthy()
	}
	r := s.eval(c.right)
	// A list matches if the other side equals any value in it.
	if l.kind == vList || r.kind == vList {
		list, other := l, r
		if r.kind == vList {
			list, other = r, l
		}
		n, ok := other.number()
		in := false
		for _, v := range list.list {
			if ok && float64(v) == n {
				in = true
			}
		}
		switch c.op {
		case "==":
			return in
		case "!=":
			return !in
		}
		s.x.run.logf("can't use %s with a list (counted as false)", c.op)
		return false
	}
	if a, ok := l.number(); ok {
		if b, ok := r.number(); ok {
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
	}
	ls := strings.ToLower(strings.TrimSpace(l.text()))
	rs := strings.ToLower(strings.TrimSpace(r.text()))
	switch c.op {
	case "==":
		return ls == rs
	case "!=":
		return ls != rs
	}
	s.x.run.logf("can't use %s on %q and %q: both sides must be numbers or times (counted as false)", c.op, ls, rs)
	return false
}

// runFlowCommand sends one command (variables already filled in). Only
// #lock/#unlock are retried (repeating them is harmless); `say` must not be
// repeated, and #shutdown is expected to go unanswered because the server
// dies.
//
// An RCON error, or no reply (except to #shutdown), marks the run as failed;
// the run carries on, and ends with status "error".
func runFlowCommand(run *flowRun, cmd string) {
	if cmd == "" {
		return
	}
	lower := strings.ToLower(cmd)

	// kickall [reason] (or #kickall) is this program's own command, not a
	// BattlEye one: it lists the players and kicks each (kickAll).
	if bare := strings.TrimPrefix(lower, "#"); bare == "kickall" || strings.HasPrefix(bare, "kickall ") {
		reason := strings.TrimSpace(strings.TrimPrefix(cmd, "#")[len("kickall"):])
		if reason == "" {
			reason = "Server restarting"
		}
		rep, err := kickAll(sharedRunner{}, reason)
		if err != nil {
			run.logf("kickall failed: %v", err)
			run.markFailed()
			return
		}
		msg := rep.summary(reason)
		log.Printf("rcon[%s] > %s", run.name, msg)
		run.addToRunLog(msg)
		return
	}

	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := runShared(cmd, 2*time.Second, retry, run.name) // logs rcon[<flow name>]
	run.addToRunLog(fmt.Sprintf("sent %q (acked=%v err=%v)", cmd, acked, err))
	if err != nil || (!acked && lower != "#shutdown") {
		run.markFailed()
	}
}

// runCfg says how one run is timed.
type runCfg struct {
	first     time.Time // when the first tick happens (after any delay)
	countdown bool
	timer     int       // $timer on the first tick (countdown only)
	event     time.Time // countdown: when $timer reaches 0
}

// runJob works through one job and reports whether it was stopped.
func runJob(run *flowRun, pf *parsedFlow, cfg runCfg, p *Player) (stopped bool) {
	if p != nil {
		run.logf("player %d (%s) joined", p.ID, p.Name)
	}
	if d := time.Until(cfg.first); d > time.Second && !cfg.countdown {
		run.logf("waiting %s before the first line", fmtLeft(int(d.Round(time.Second)/time.Second)))
	}
	stoppedNow := func() bool {
		if p != nil {
			run.logf("stopped for player %d (%s)", p.ID, p.Name)
		} else {
			run.logf("stopped")
		}
		run.sendCancel()
		return true
	}
	if !sleepUntil(run.ctx, cfg.first) {
		return stoppedNow()
	}
	x := newJob(run, pf, p, cfg.countdown, cfg.timer)
	lagged := false
	for k := 0; ; k++ {
		at := cfg.first.Add(time.Duration(k) * time.Second)
		if !sleepUntil(run.ctx, at) {
			return stoppedNow()
		}
		if late := time.Since(at); late > 5*time.Second && !lagged {
			run.logf("running %.0fs behind; catching up", late.Seconds())
			lagged = true
		}
		if k > maxTimeSecs {
			run.logf("still running after 24 hours; stopped")
			run.markFailed()
			return false
		}
		x.tick()
		if x.endOfTick() {
			if x.halted {
				return false
			}
			break
		}
	}
	if p == nil {
		run.logf("done")
	}
	return false
}

// endOfTick moves $timer on after a tick and reports whether the job is
// over: a stop line ran, a countdown reached 0, or (other runs) nothing is
// left to send.
func (x *job) endOfTick() bool {
	if x.halted {
		return true
	}
	if !x.countdown {
		return x.idle()
	}
	switch {
	case x.newTimer != nil:
		x.timer, x.newTimer = *x.newTimer, nil
		x.run.setAnchor(time.Now().Add(time.Duration(x.timer) * time.Second))
	case x.timer == 0:
		if n := x.unsent(); n > 0 {
			x.run.logf("the countdown reached 0 with %d block(s) still sending; the rest is not sent", n)
		}
		return true
	default:
		x.timer--
	}
	return false
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

// ---------------------------------------------------------------------------
// Save-time analysis and the edit dialog's preview
// ---------------------------------------------------------------------------

// flowCfg is how a flow's Timer is used (from the edit dialog's fields).
type flowCfg struct {
	trigger   string
	timer     int  // seconds; 0 = no Timer
	countdown bool // scheduled, with a Timer, set to "before"
}

// timedBlock is one point in a countdown where something is sent.
type timedBlock struct {
	line     int
	secs     []int
	ticks    int    // how many ticks the block can take
	firstCmd string // the first command it sends
}

type flowAnalysis struct {
	blocks     []timedBlock
	sends      []int // every send point, greatest first
	plainTicks int   // ticks the plain lines take
	plainFirst string
}

// blockTicks is the most ticks a list of lines can take (both sides of an
// if are counted as the longer one; for each player counts one player).
func blockTicks(nodes []*flowNode) int {
	t := 0
	for _, n := range nodes {
		switch n.kind {
		case nodeCmd:
			t++
		case nodeWait:
			t += n.ticks
		case nodeIf:
			a, b := blockTicks(n.then), blockTicks(n.els)
			if b > a {
				a = b
			}
			t += a
		case nodeDo, nodeEach:
			t += blockTicks(n.then)
		}
	}
	return t
}

func firstCmd(nodes []*flowNode) string {
	for _, n := range nodes {
		switch n.kind {
		case nodeCmd:
			return n.text
		case nodeIf, nodeDo, nodeEach:
			if c := firstCmd(n.then); c != "" {
				return c
			}
			if c := firstCmd(n.els); c != "" {
				return c
			}
		}
	}
	return ""
}

// timerEquals finds `$timer == <time or list>` in a condition that has a
// single all-of group, so the analysis can treat it like a timed line.
func timerEquals(c *flowCond, lists map[string][]int) []int {
	if c == nil || len(c.any) != 1 {
		return nil
	}
	for _, cmp := range c.any[0] {
		if cmp.op != "==" {
			continue
		}
		other := ""
		switch {
		case strings.EqualFold(cmp.left, "$timer"):
			other = cmp.right
		case strings.EqualFold(cmp.right, "$timer"):
			other = cmp.left
		default:
			continue
		}
		if secs, ok := timeLit(other); ok {
			return []int{secs}
		}
		if m := varNameRe.FindStringSubmatch(other); m != nil {
			return lists[strings.ToLower(m[1])]
		}
	}
	return nil
}

// analyzeFlow runs the checks that depend on the Timer and trigger, and works
// out the timeline the preview shows. Every error is a user-facing message.
func analyzeFlow(pf *parsedFlow, cfg flowCfg) (*flowAnalysis, error) {
	if !cfg.countdown {
		// Manual and join flows can't be scheduled, so they never count down.
		need := "a Timer set to Before schedule"
		if cfg.trigger != triggerSchedule {
			need = "a countdown, which only scheduled flows have"
		}
		if len(pf.timedLines) > 0 {
			return nil, fmt.Errorf("line %d: timed lines need %s; use wait to space out commands", pf.timedLines[0], need)
		}
		if len(pf.timerUses) > 0 {
			return nil, fmt.Errorf("line %d: $timer needs %s", pf.timerUses[0], need)
		}
	}
	for _, u := range pf.playerUses {
		if !u.inEach && cfg.trigger != triggerJoin {
			return nil, fmt.Errorf("line %d: $%s only works in flows triggered On player join, or inside for each player", u.line, u.name)
		}
	}
	a := &flowAnalysis{}
	var plain []*flowNode
	for _, n := range pf.Top {
		if !isWatcher(n, cfg.countdown) {
			plain = append(plain, n)
			continue
		}
		var secs []int
		body := n.then
		switch {
		case n.at != nil && n.at.listVar != "":
			secs = pf.lists[n.at.listVar]
		case n.at != nil:
			secs = n.at.secs
		default:
			secs = timerEquals(n.cond, pf.lists)
		}
		if len(secs) == 0 {
			continue
		}
		a.blocks = append(a.blocks, timedBlock{line: n.line, secs: secs, ticks: blockTicks(body), firstCmd: firstCmd(body)})
	}
	a.plainTicks = blockTicks(plain)
	a.plainFirst = firstCmd(plain)
	if !cfg.countdown {
		return a, nil
	}
	seen := map[int]bool{}
	for _, b := range a.blocks {
		for _, s := range b.secs {
			if s > cfg.timer {
				return nil, fmt.Errorf("line %d: %s is earlier than the Timer (%s), so it would never run", b.line, fmtLeft(s), fmtLeft(cfg.timer))
			}
			if last := s - (b.ticks - 1); b.ticks > 0 && last < 0 {
				return nil, fmt.Errorf("line %d: the last line of this block would go out at -%s; start it earlier", b.line, fmtLeft(-last))
			}
			if !seen[s] {
				seen[s] = true
				a.sends = append(a.sends, s)
			}
		}
	}
	if last := cfg.timer - (a.plainTicks - 1); a.plainTicks > 0 && last < 0 {
		return nil, fmt.Errorf("the lines without a time take %s, longer than the Timer (%s)", fmtLeft(a.plainTicks), fmtLeft(cfg.timer))
	}
	sort.Sort(sort.Reverse(sort.IntSlice(a.sends)))
	return a, nil
}

// summary describes the timeline for the edit dialog. event is the next cron
// time for scheduled flows (zero otherwise).
func (a *flowAnalysis) summary(pf *parsedFlow, cfg flowCfg, event time.Time) string {
	clock := func(t time.Time) string { return t.Format("15:04:05") }
	var b strings.Builder
	if cfg.countdown {
		start := event.Add(-time.Duration(cfg.timer) * time.Second)
		fmt.Fprintf(&b, "Starts %s (%s before the schedule)", clock(start), fmtLeft(cfg.timer))
		first, firstAt := a.plainFirst, cfg.timer
		if first == "" {
			firstAt = -1
			for _, bl := range a.blocks {
				for _, s := range bl.secs {
					if s > firstAt {
						first, firstAt = bl.firstCmd, s
					}
				}
			}
		}
		if first != "" {
			fmt.Fprintf(&b, "; first command at %s: %s", clock(event.Add(-time.Duration(firstAt)*time.Second)), fillTimeLeft(first, firstAt))
		}
		fmt.Fprintf(&b, ". Ends %s", clock(event))
		for _, bl := range a.blocks {
			for _, s := range bl.secs {
				if s == 0 && bl.firstCmd != "" {
					fmt.Fprintf(&b, " with %s", bl.firstCmd)
				}
			}
		}
		b.WriteString(".")
		if len(a.sends) > 0 {
			parts := make([]string, len(a.sends))
			for i, s := range a.sends {
				parts[i] = fmtLeft(s)
			}
			fmt.Fprintf(&b, " Timed lines at %s.", strings.Join(parts, ", "))
		}
		if len(pf.timerSets) > 0 {
			lines := make([]string, len(pf.timerSets))
			for i, l := range pf.timerSets {
				lines[i] = strconv.Itoa(l)
			}
			fmt.Fprintf(&b, " The length can change: $timer is set on line %s.", strings.Join(lines, ", "))
		}
		return b.String()
	}
	when := map[string]string{
		triggerManual:   "when Run is pressed",
		triggerJoin:     "when a player joins",
		triggerSchedule: "at the scheduled time",
	}[cfg.trigger]
	after := map[string]string{
		triggerManual:   "Run is pressed",
		triggerJoin:     "the player joins",
		triggerSchedule: "the scheduled time",
	}[cfg.trigger]
	if cfg.timer > 0 {
		fmt.Fprintf(&b, "Starts %s after %s", fmtLeft(cfg.timer), after)
		if !event.IsZero() {
			fmt.Fprintf(&b, " (%s)", clock(event.Add(time.Duration(cfg.timer)*time.Second)))
		}
	} else {
		fmt.Fprintf(&b, "Starts %s", when)
		if !event.IsZero() {
			fmt.Fprintf(&b, " (%s)", clock(event))
		}
	}
	if a.plainFirst != "" {
		fmt.Fprintf(&b, " with %s", a.plainFirst)
	}
	if a.plainTicks > 1 {
		fmt.Fprintf(&b, ". The lines take %s", fmtLeft(a.plainTicks))
	}
	b.WriteString(".")
	return b.String()
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
	dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
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

// Timer modes for scheduled flows with a Timer.
const (
	timerAfter  = "after"  // delay: run the script Timer after the cron time
	timerBefore = "before" // countdown: start Timer before the cron time
)

type Flow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Script string `json:"script"`
	// Cancel holds commands sent when Stop is pressed, one per line.
	Cancel string `json:"cancel"`
	// Timer is a time such as "60m", or "" for no Timer.
	Timer string `json:"timer"`
	// TimerMode is "after" or "before" for a scheduled flow with a Timer,
	// "" otherwise.
	TimerMode string `json:"timerMode"`
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

// configOf reads a flow's Timer fields. Errors are user-facing.
func configOf(f Flow) (flowCfg, error) {
	cfg := flowCfg{trigger: f.Trigger}
	if t := strings.TrimSpace(f.Timer); t != "" {
		secs, err := parseTime(t)
		if err != nil {
			return cfg, fmt.Errorf("Timer: %v", err)
		}
		cfg.timer = secs
	}
	// Only a scheduled flow can count down to its time; for manual and join
	// flows the Timer is always a delay, whatever mode is sent.
	if cfg.trigger == triggerSchedule && cfg.timer > 0 {
		switch strings.ToLower(strings.TrimSpace(f.TimerMode)) {
		case timerBefore, "":
			cfg.countdown = true
		case timerAfter:
		default:
			return cfg, fmt.Errorf("Timer mode: use %q or %q", timerBefore, timerAfter)
		}
	}
	return cfg, nil
}

// storedTimer returns the Timer (as typed, lower case) and Timer mode as
// saved, both empty where they don't apply.
func storedTimer(f Flow, cfg flowCfg) (string, string) {
	if cfg.timer == 0 {
		return "", ""
	}
	t := strings.ToLower(strings.TrimSpace(f.Timer))
	switch {
	case cfg.countdown:
		return t, timerBefore
	case cfg.trigger == triggerSchedule:
		return t, timerAfter
	}
	return t, ""
}

const defaultRestartScript = `// Server restart. The run starts 60 minutes (the Timer) before the
// scheduled time, and $timer counts down to 0 at it. "5m | ..." runs a
// line when 5 minutes are left.
$warn = 60m 30m 15m 10m 5m 3m
$warn | say -1 Server is going to restart in @minutes minutes!
2m    | #lock
1m    | if $players > 0
  say -1 Server is going to restart in 1 minute!
  #kickall Server Restart
end
0     | #shutdown`

const defaultRestartCancel = `say -1 Server restart cancelled.
#unlock`

// initFlows creates the table, seeds an example restart flow on first run,
// and logs any saved flow written for the old script format.
func initFlows() error {
	seed := Flow{Name: "Server restart", Script: defaultRestartScript, Cancel: defaultRestartCancel,
		Timer: "60m", TimerMode: timerBefore, Schedule: "0 6 * * *"}
	if err := globalDatabase.EnsureFlowsTable(seed); err != nil {
		return err
	}
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		return err
	}
	for _, f := range flows {
		if err := checkFlow(f); err != nil {
			log.Printf("flow[%s] needs updating for the new script format and won't run until it is saved again: %v", f.Name, err)
		}
	}
	return nil
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

// checkCancel validates the Cancel field: plain commands, one per line.
func checkCancel(cancel string) error {
	for i, l := range strings.Split(cancel, "\n") {
		l = strings.TrimSpace(l)
		low := strings.ToLower(l)
		if l == "" || strings.HasPrefix(l, "//") {
			continue
		}
		first := strings.Fields(low)[0]
		switch first {
		case "if", "else", "end", "do", "for", "while", "wait", "stop":
			return fmt.Errorf("Cancel line %d: only plain commands go here, sent one after another when Stop is pressed", i+1)
		}
		if strings.HasPrefix(l, "$") && strings.Contains(l, "=") {
			return fmt.Errorf("Cancel line %d: variables can't be set here", i+1)
		}
		for _, m := range varRe.FindAllStringSubmatch(l, -1) {
			if name := strings.ToLower(m[1]); playerVars[name] {
				return fmt.Errorf("Cancel line %d: $%s can't be used here (Cancel is sent once for the whole run)", i+1, name)
			}
		}
	}
	return nil
}

// cancelLines are the Cancel field's commands, in order.
func cancelLines(cancel string) []string {
	var out []string
	for _, l := range strings.Split(cancel, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "//") {
			out = append(out, l)
		}
	}
	return out
}

// parseAndAnalyze parses a flow's script and checks it against its trigger
// and Timer. Errors are user-facing.
func parseAndAnalyze(f Flow) (*parsedFlow, flowCfg, *flowAnalysis, error) {
	cfg, err := configOf(f)
	if err != nil {
		return nil, cfg, nil, err
	}
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return nil, cfg, nil, err
	}
	a, err := analyzeFlow(pf, cfg)
	if err != nil {
		return nil, cfg, nil, err
	}
	return pf, cfg, a, nil
}

// checkFlow runs every check a stored flow must pass to run.
func checkFlow(f Flow) error {
	if f.Trigger == triggerSchedule {
		if err := checkCronSchedule(f.Schedule); err != nil {
			return err
		}
	}
	if err := checkCancel(f.Cancel); err != nil {
		return err
	}
	_, _, _, err := parseAndAnalyze(f)
	return err
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
	f.Cancel = normalizeScript(f.Cancel)
	if err := checkFlow(f); err != nil {
		return 0, err
	}
	cfg, _ := configOf(f)
	f.Timer, f.TimerMode = storedTimer(f, cfg)
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
	return checkFlow(f)
}

// previewFlow builds the plain-English summary the edit dialog shows: what
// the trigger means, the next few run times, and the script's timeline.
func previewFlow(f Flow) map[string]any {
	trig, err := normalizeTrigger(f.Trigger)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	f.Trigger = trig
	cfg, err := configOf(f)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if err := checkCancel(f.Cancel); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	// Script mistakes show in red while typing, with the line number.
	var a *flowAnalysis
	pf, perr := parseFlowScript(f.Script)
	if perr == nil {
		a, perr = analyzeFlow(pf, cfg)
	}
	if perr != nil && strings.TrimSpace(f.Script) != "" {
		return map[string]any{"ok": false, "error": "Script: " + perr.Error()}
	}
	resp := map[string]any{"ok": true}

	switch trig {
	case triggerManual:
		resp["description"] = "Manual only - this flow runs when you press Run (or POST /api/flows/<id>/run). There is nothing to enable."
		if a != nil {
			resp["span"] = a.summary(pf, cfg, time.Time{})
		}
		return resp
	case triggerJoin:
		resp["description"] = "While Enabled, this flow runs once for each player who joins. $player, $name, $ip, $guid and $ping are that player."
		if a != nil {
			resp["span"] = a.summary(pf, cfg, time.Time{})
		}
		return resp
	}

	// Schedule trigger.
	if err := checkCronSchedule(f.Schedule); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	c, _ := parseCron(f.Schedule)
	resp["description"] = c.describe()
	next := []string{}
	t := time.Now()
	for i := 0; i < 3; i++ {
		n, ok := c.next(t)
		if !ok {
			break
		}
		if i == 0 && a != nil {
			resp["span"] = a.summary(pf, cfg, n)
			if cfg.countdown {
				resp["starts"] = fmtWhen(n.Add(-time.Duration(cfg.timer) * time.Second))
			}
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
// Runs
// ---------------------------------------------------------------------------

// flowRun is the live (or last finished) run of one flow. A scheduled or
// manual run has one job; a join run has one job per player in progress, all
// sharing this run's log, context and Stop button. Mutable fields are guarded
// by flowRuns' mutex.
type flowRun struct {
	flowID     int64
	name       string
	status     string // running | done | stopped | error
	active     bool
	failed     bool      // a command failed; the run ends as "error"
	stopped    bool      // the Stop button was pressed
	anchor     time.Time // countdown: when $timer reaches 0; otherwise when the script starts
	lastRun    time.Time // when the flow was last started (join flows: last player)
	jobs       int       // jobs still working through the script
	log        []string
	cancelCmds []string // the Cancel field, sent once if Stop is pressed
	cancelOnce sync.Once
	ctx        context.Context
	cancel     context.CancelFunc
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

// logf writes a line to the container log as flow[name] and to the run log
// shown on the Flows page.
func (r *flowRun) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("flow[%s] %s", r.name, msg)
	r.addToRunLog(msg)
}

// addToRunLog adds a line to the Flows page's run log only. Used for sent
// commands, which runShared already logs to the container as rcon[name].
func (r *flowRun) addToRunLog(msg string) {
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	flowRuns.Unlock()
}

func (r *flowRun) markFailed() {
	flowRuns.Lock()
	r.failed = true
	flowRuns.Unlock()
}

func (r *flowRun) setAnchor(t time.Time) {
	flowRuns.Lock()
	r.anchor = t
	flowRuns.Unlock()
}

// sendCancel sends the Cancel field's commands, once per run.
func (r *flowRun) sendCancel() {
	r.cancelOnce.Do(func() {
		if len(r.cancelCmds) == 0 {
			return
		}
		r.logf("sending the Cancel commands")
		x := &seq{x: &job{run: r, vars: map[string]value{}}}
		for _, c := range r.cancelCmds {
			sendCommand(r, x.fill(c))
		}
	})
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

// startFlow starts a run now (the Run button), whatever the flow's trigger.
// minutes > 0 replaces the Timer for this run: a countdown of that length,
// or that long a delay.
func startFlow(f Flow, minutes int) error {
	pf, cfg, _, err := parseAndAnalyze(f)
	if err != nil {
		return err
	}
	for _, u := range pf.playerUses {
		if !u.inEach {
			return fmt.Errorf("this flow uses $%s, so it only runs when a player joins", u.name)
		}
	}
	if minutes > 0 {
		cfg.timer = minutes * 60
	}
	now := time.Now()
	rc := runCfg{first: now, countdown: cfg.countdown, timer: cfg.timer}
	if cfg.countdown {
		rc.event = now.Add(time.Duration(cfg.timer) * time.Second)
	} else {
		rc.first = now.Add(time.Duration(cfg.timer) * time.Second)
	}
	return launch(f, pf, rc, sourceManual, nil)
}

// stopFlow cancels the active run of a flow (the Stop button). The run sends
// the Cancel commands as it stops.
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

// launch starts a run of f. p is the joining player for join flows (nil
// otherwise); a join adds a job to the flow's active run if there is one,
// while any other start is refused with errFlowRunning.
func launch(f Flow, pf *parsedFlow, rc runCfg, source string, p *Player) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[f.ID]
	active := run != nil && run.active
	if active && p == nil {
		flowRuns.Unlock()
		return errFlowRunning
	}
	if !active {
		ctx, cancel := context.WithCancel(context.Background())
		run = &flowRun{flowID: f.ID, name: f.Name, status: "running", active: true,
			cancelCmds: cancelLines(f.Cancel), ctx: ctx, cancel: cancel}
		flowRuns.byFlow[f.ID] = run
	}
	run.jobs++
	run.anchor = rc.first
	if rc.countdown {
		run.anchor = rc.event
	}
	run.lastRun = time.Now()
	if rc.first.After(run.lastRun) {
		run.lastRun = rc.first
	}
	flowRuns.Unlock()
	if !active {
		run.logf("Started by %s", source)
	}
	go func() { run.jobDone(runJob(run, pf, rc, p)) }()
	return nil
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// scheduleGrace lets a run whose start just passed (a late tick, a restart a
// few seconds in) still start instead of being skipped.
const scheduleGrace = 15 * time.Second

// scheduleTick is how often the scheduler checks for runs to start.
const scheduleTick = 5 * time.Second

// lastFired is the latest cron occurrence started for each flow, so a
// finished or stopped run isn't started again inside the same window. Only
// the scheduler goroutine touches it.
var lastFired = map[int64]time.Time{}

// launchDue reports whether a run that starts at `start` should be launched
// at this tick. It looks one tick ahead: a run due before the next tick is
// launched now and sleeps until the exact second.
func launchDue(now, start time.Time) bool {
	return !now.Add(scheduleTick).Before(start)
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

// runScheduleTick starts, for every enabled scheduled flow, the first cron
// occurrence that hasn't been started yet and whose start is now or before
// the next tick:
//
//	countdown  starts Timer before the cron time. If ReCON was down when the
//	           countdown should have started, it joins it, with $timer at the
//	           time left; timed lines that already passed never match.
//	delay      the script starts Timer after the cron time.
//	no Timer   the script starts at the cron time.
//
// A delay or no-Timer run more than scheduleGrace late is skipped.
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
		pf, cfg, _, err := parseAndAnalyze(f)
		if err != nil {
			continue
		}
		timer := time.Duration(cfg.timer) * time.Second

		// The first occurrence whose start (or, for a countdown, whose event)
		// hasn't passed yet, skipping any already started.
		from := now.Add(-scheduleGrace)
		if !cfg.countdown {
			from = from.Add(-timer)
		}
		if lf, ok := lastFired[f.ID]; ok && lf.After(from) {
			from = lf
		}
		t, ok := c.next(from)
		if !ok {
			continue
		}
		rc := runCfg{countdown: cfg.countdown, first: t.Add(timer)}
		if cfg.countdown {
			rc.event = t
			rc.first = t.Add(-timer)
			rc.timer = cfg.timer
			if now.After(rc.first) { // joining a countdown already under way
				left := int(t.Sub(now) / time.Second)
				if left < 0 {
					left = 0
				}
				rc.timer = left
				rc.first = t.Add(-time.Duration(left) * time.Second)
			}
		}
		if !launchDue(now, rc.first) {
			continue
		}
		if err := launch(f, pf, rc, sourceSchedule, nil); errors.Is(err, errFlowRunning) {
			continue // busy: retry next tick
		} else if err != nil {
			log.Printf("scheduler: %s: %v", f.Name, err)
		}
		lastFired[f.ID] = t
	}
}

// ---------------------------------------------------------------------------
// Player-join trigger, and the latest player list for $players
// ---------------------------------------------------------------------------

// latest is the poller's most recent player list. $players and for each
// player read it, so flows don't send their own `players` command.
var latest struct {
	sync.Mutex
	players []Player
	ok      bool // false until the first successful poll
}

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
// result. It keeps the latest list for $players and starts the join flows
// for new arrivals. The first call after startup only records who is online,
// so a restart doesn't greet everybody.
func playersPolled(players []Player) {
	latest.Lock()
	latest.players, latest.ok = append([]Player(nil), players...), true
	latest.Unlock()
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
// forgotten, so players are greeted again when they reconnect, and $players
// reads 0.
func playersLost() {
	joinWatch.seen = map[string]bool{}
	latest.Lock()
	latest.players = nil
	latest.Unlock()
}

// startJoinFlows runs every enabled join flow once for each player, after
// the flow's Timer (if any).
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
		pf, cfg, _, err := parseAndAnalyze(f)
		if err != nil {
			continue
		}
		for i := range joined {
			rc := runCfg{first: time.Now().Add(time.Duration(cfg.timer) * time.Second)}
			launch(f, pf, rc, sourceJoin, &joined[i])
		}
	}
}
