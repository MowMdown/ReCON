package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
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
// A "flow" is a named, saved script of RCON steps timed around a cron schedule.
// Each time the cron expression fires, the flow runs: every step has an offset
// from that cron time, so steps can run before it, on it, or after it.
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
// $minutes / $seconds in a command become how long before the cron time that
// step runs (0 for steps on or after it).
//
// Schedule: a standard 5-field cron expression (minute hour day-of-month month
// day-of-week) in the server's local time, so set TZ in the container.
// Supports * , - / lists, month/day names and @hourly @daily @weekly @monthly
// @yearly. A flow only runs on its schedule while it is Enabled.
//
// API:
//	GET  /api/flows                    flows + current/last run of each
//	POST /api/flows                    create/update {id, name, script, schedule, scheduleEnabled}
//	POST /api/flows/<id>/run           optional body {"minutes": N} = minutes until the cron time
//	POST /api/flows/<id>/stop
//	POST /api/flows/<id>/enable        (needs a schedule)   /disable
//	POST /api/flows/<id>/delete
//	POST /api/flows/preview            {schedule, script} -> plain-English description
//	POST /api/flows/reorder            {ids: [...]} saves the order shown on the page
//	POST /api/flows/command            {command: lock|unlock|shutdown|kickall} the page's quick buttons
// Page: /flows
//
// Runs live in memory: restarting this program abandons a run in progress
// (a scheduled run that was mid-countdown is picked up again, see scheduler).
// ============================================================================

// ---------------------------------------------------------------------------
// Script parsing
// ---------------------------------------------------------------------------

type flowStep struct {
	OffsetSecs int // negative = before the cron time, positive = after
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
	return 0, fmt.Errorf("time %q needs a sign: -%s runs before the cron time, +%s after it", when, m[2]+m[3], m[2]+m[3])
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

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

type Flow struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Script          string `json:"script"`
	Schedule        string `json:"schedule"` // cron expression
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
	if _, err := globalDatabase.Exec(`
		CREATE TABLE IF NOT EXISTS flows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			script TEXT NOT NULL,
			schedule TEXT NOT NULL DEFAULT '',
			schedule_enabled INTEGER NOT NULL DEFAULT 0,
			position INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		return err
	}
	var n int
	if err := globalDatabase.QueryRow("SELECT COUNT(*) FROM flows").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := globalDatabase.Exec("INSERT INTO flows (name, script, schedule, position) VALUES (?, ?, ?, 1)",
			"Server restart", defaultRestartScript, "0 6 * * *")
		return err
	}
	return nil
}

func listFlows() ([]Flow, error) {
	rows, err := globalDatabase.Query("SELECT id, name, script, schedule, schedule_enabled FROM flows ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flows := []Flow{}
	for rows.Next() {
		var f Flow
		var en int
		if err := rows.Scan(&f.ID, &f.Name, &f.Script, &f.Schedule, &en); err != nil {
			return nil, err
		}
		f.ScheduleEnabled = en != 0
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

func getFlow(id int64) (Flow, error) {
	var f Flow
	var en int
	err := globalDatabase.QueryRow("SELECT id, name, script, schedule, schedule_enabled FROM flows WHERE id = ?", id).
		Scan(&f.ID, &f.Name, &f.Script, &f.Schedule, &en)
	f.ScheduleEnabled = en != 0
	return f, err
}

func saveFlow(f Flow) (int64, error) {
	en := 0
	if f.ScheduleEnabled {
		en = 1
	}
	if f.ID == 0 {
		res, err := globalDatabase.Exec("INSERT INTO flows (name, script, schedule, schedule_enabled, position) VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM flows))",
			f.Name, f.Script, f.Schedule, en)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := globalDatabase.Exec("UPDATE flows SET name = ?, script = ?, schedule = ?, schedule_enabled = ? WHERE id = ?",
		f.Name, f.Script, f.Schedule, en, f.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return f.ID, nil
}

func setScheduleEnabled(id int64, on bool) error {
	en := 0
	if on {
		en = 1
	}
	_, err := globalDatabase.Exec("UPDATE flows SET schedule_enabled = ? WHERE id = ?", en, id)
	return err
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
}

var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
}{byFlow: map[int64]*flowRun{}}

var errFlowRunning = errors.New("flow is already running")

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
	lead := 0
	if minutes > 0 {
		lead = minutes * 60
	} else if first := pf.Steps[0].OffsetSecs; first < 0 {
		lead = -first
	}
	return launchFlow(f, pf, time.Now().Add(time.Duration(lead)*time.Second), "manual run")
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

// sharedRunner adapts the shared RCON session to the commandRunner interface
// that kickAllUsing expects (single attempt per command, no retries).
type sharedRunner struct{}

func (sharedRunner) run(command string, wait time.Duration) (string, bool, error) {
	return runShared(command, wait, false)
}

// runFlowCommand sends one step's command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
func runFlowCommand(run *flowRun, command string, offsetSecs int) {
	before := 0
	if offsetSecs < 0 {
		before = -offsetSecs
	}
	cmd := strings.NewReplacer("$minutes", strconv.Itoa(before/60), "$seconds", strconv.Itoa(before)).Replace(command)
	cmd = cleanReason(cmd)
	lower := strings.ToLower(cmd)
	// kickall is this program's own command, not a BattlEye one: it lists the
	// players and kicks each (see kickAllUsing in main.go). Usage: kickall [reason]
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
			run.logf("skipped %q (its time had already passed)", st.Command)
			continue
		}
		if !sleepUntil(ctx, fireAt) {
			run.logf("stopped")
			// Cancel commands undo a countdown that never reached its cron time.
			if time.Now().Before(run.anchor) {
				for _, c := range pf.OnCancel {
					runFlowCommand(run, c, 0)
				}
			}
			run.set("stopped", false)
			return
		}
		runFlowCommand(run, st.Command, st.OffsetSecs)
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

// runScheduleTick starts, for every enabled flow, the first cron occurrence
// whose window (cron time + earliest offset ... cron time + latest offset) is
// open and not yet started. If the app was down when the window opened it
// joins the run in progress; steps already in the past are skipped.
func runScheduleTick(now time.Time) {
	flows, err := listFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled {
			continue
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
// HTTP handlers
// ---------------------------------------------------------------------------

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst)
}

func fmtWhen(t time.Time) string { return t.Format("Mon 02 Jan 15:04 MST") }

// handleFlowsAPI serves GET (list) and POST (create/update) on /api/flows.
func handleFlowsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		flows, err := listFlows()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		now := time.Now()
		for i := range flows {
			if !flows[i].ScheduleEnabled {
				continue
			}
			if c, err := parseCron(flows[i].Schedule); err == nil {
				if next, ok := c.next(now); ok {
					flows[i].NextRun = fmtWhen(next)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": flows, "runs": snapshotRuns()})
	case http.MethodPost:
		var f Flow
		if err := decodeBody(w, r, &f); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON")
			return
		}
		f.Name = strings.TrimSpace(f.Name)
		f.Schedule = strings.TrimSpace(f.Schedule)
		if f.Name == "" {
			fail(w, http.StatusBadRequest, "name is required")
			return
		}
		if _, err := parseFlowScript(f.Script); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if f.Schedule == "" {
			f.ScheduleEnabled = false
		} else if _, err := parseCron(f.Schedule); err != nil {
			fail(w, http.StatusBadRequest, "schedule: "+err.Error())
			return
		}
		id, err := saveFlow(f)
		if err != nil {
			fail(w, http.StatusBadRequest, "could not save (is the name already used?)")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		fail(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleFlowPreview serves POST /api/flows/preview: a plain-English summary of
// a schedule (and how the script's offsets fit around it) for the edit dialog.
func handleFlowPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule string `json:"schedule"`
		Script   string `json:"script"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	resp := map[string]any{"ok": true}
	pf, perr := parseFlowScript(req.Script)
	if perr == nil {
		resp["span"] = pf.span()
	}
	sched := strings.TrimSpace(req.Schedule)
	if sched == "" {
		resp["description"] = "No schedule - this flow only runs when you press Run."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	c, err := parseCron(sched)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
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
	writeJSON(w, http.StatusOK, resp)
}

// handleFlowReorder serves POST /api/flows/reorder: the body lists flow ids in
// the order they should appear.
func handleFlowReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeBody(w, r, &req); err != nil || len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	tx, err := globalDatabase.Begin()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	for i, id := range req.IDs {
		if _, err := tx.Exec("UPDATE flows SET position = ? WHERE id = ?", i+1, id); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleQuickCommand serves POST /api/flows/command for the page's static
// buttons. Only these four commands are accepted (not arbitrary RCON text).
func handleQuickCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Command), "#"))
	switch name {
	case "lock", "unlock":
		_, acked, err := runShared("#"+name, 2*time.Second, true)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		if !acked {
			fail(w, http.StatusBadGateway, "#"+name+" was not acknowledged")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "#" + name + " sent"})
	case "shutdown":
		// The server may die before it acknowledges, so only a transport error is a failure.
		_, acked, err := runShared("#shutdown", 2*time.Second, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "message": "#shutdown sent"})
	case "kickall":
		rep, err := kickAllUsing(sharedRunner{}, "Server restarting", false, false)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		msg := fmt.Sprintf("Kicked %d player(s)", len(rep.Order))
		if len(rep.Remaining) > 0 {
			msg += fmt.Sprintf(", %d still connected", len(rep.Remaining))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	default:
		fail(w, http.StatusBadRequest, "unknown command")
	}
}

// handleFlowAction serves POST /api/flows/<id>/<run|stop|enable|disable|delete>
// and the /api/flows/preview, /reorder and /command endpoints.
func handleFlowAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/preview" {
		handleFlowPreview(w, r)
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/command" {
		handleQuickCommand(w, r)
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") == "/api/flows/reorder" {
		handleFlowReorder(w, r)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/flows/"), "/"), "/")
	if len(parts) != 2 {
		fail(w, http.StatusNotFound, "invalid flow action")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusNotFound, "invalid flow id")
		return
	}
	f, err := getFlow(id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "flow not found")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch parts[1] {
	case "run":
		var body struct {
			Minutes int `json:"minutes"`
		}
		if r.ContentLength != 0 {
			if err := decodeBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
				fail(w, http.StatusBadRequest, "bad JSON")
				return
			}
		}
		if body.Minutes < 0 || body.Minutes > 10080 {
			fail(w, http.StatusBadRequest, "minutes must be 0-10080")
			return
		}
		if err := startFlow(f, body.Minutes); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errFlowRunning) {
				code = http.StatusConflict
			}
			fail(w, code, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "stop":
		flowRuns.Lock()
		run := flowRuns.byFlow[id]
		active := run != nil && run.active
		flowRuns.Unlock()
		if !active {
			fail(w, http.StatusConflict, "flow is not running")
			return
		}
		run.cancel()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "enable":
		if strings.TrimSpace(f.Schedule) == "" {
			fail(w, http.StatusBadRequest, "set a schedule first (Edit)")
			return
		}
		if _, err := parseCron(f.Schedule); err != nil {
			fail(w, http.StatusBadRequest, "schedule: "+err.Error())
			return
		}
		if _, err := parseFlowScript(f.Script); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := setScheduleEnabled(id, true); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "disable":
		if err := setScheduleEnabled(id, false); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "delete":
		flowRuns.Lock()
		run := flowRuns.byFlow[id]
		if run != nil && run.active {
			flowRuns.Unlock()
			fail(w, http.StatusConflict, "stop the running flow first")
			return
		}
		delete(flowRuns.byFlow, id)
		flowRuns.Unlock()
		if _, err := globalDatabase.Exec("DELETE FROM flows WHERE id = ?", id); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusNotFound, "unknown action")
	}
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

var flowsPage = template.Must(template.Must(baseTemplate.Clone()).New("flows").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Flows</title>
{{template "style" .}}
<style>
.box.wide { width: min(760px, 95vw); }
.top { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; flex-wrap: wrap; }
.top h1 { margin: 0 0 8px; }
.quick { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.quick button { margin-right: 0; }
#quickMsg { color: #aaa; font-size: 13px; margin-right: 8px; }
.box.helpbox { width: min(820px, 95vw); max-height: 85vh; overflow-y: auto; }
.helpbox h3 { margin: 18px 0 6px; font-size: 15px; }
.helpbox p, .helpbox li { font-size: 13px; line-height: 1.6; color: #ccc; }
.helpbox ul { margin: 4px 0; padding-left: 20px; }
.helpbox code { font-family: ui-monospace, Consolas, monospace; font-size: 12px; background: #111; padding: 1px 5px; border-radius: 3px; }
.helpbox pre { background: #111; padding: 10px; border-radius: 6px; font-size: 12px; overflow-x: auto; margin: 6px 0; }
textarea.script, #sched { font-family: ui-monospace, Consolas, monospace; font-size: 13px; }
textarea.script { white-space: pre; }
.help { color: #aaa; font-size: 13px; line-height: 1.6; margin-bottom: 10px; }
.help.bad { color: #f28b82; }
.run { margin-top: 24px; }
pre.log { background: #1a1a1a; padding: 10px; border-radius: 6px; font-size: 12px; white-space: pre-wrap; margin: 6px 0 0; }
td.actions { white-space: nowrap; }
button:disabled { opacity: 0.4; cursor: default; }
#flows tr[draggable="true"] { cursor: grab; }
#flows tr.dragging { opacity: 0.4; }
#flows tr.drop-above td { box-shadow: inset 0 3px 0 #8ab4f8; }
#flows tr.drop-below td { box-shadow: inset 0 -3px 0 #8ab4f8; }
</style>
</head>
<body>
<nav><a href="/players">Players</a><a href="/bans">Server Bans</a><a class="active" href="/flows">Flows</a></nav>
<div class="top">
	<h1>Flows</h1>
	<div class="quick">
		<span id="quickMsg"></span>
		<button onclick="quick('lock')">#Lock</button>
		<button onclick="quick('unlock')">#Unlock</button>
		<button onclick="quick('shutdown')">#Shutdown</button>
		<button onclick="quick('kickall')">#Kick All</button>
	</div>
</div>
<div id="status">Connecting...</div>
<p><button onclick="newFlow()">New flow</button><button onclick="openHelp()">Help</button></p>
<table>
	<colgroup>
		<col style="width: 200px">
		<col style="width: 140px">
		<col>
		<col style="width: 190px">
		<col style="width: 390px">
	</colgroup>
	<thead>
		<tr><th>Flow</th><th>Schedule</th><th>Status</th><th>Next run</th><th>Actions</th></tr>
	</thead>
	<tbody id="flows"></tbody>
</table>
<div id="runs"></div>

<div id="modal" class="modal">
	<div class="box wide">
		<h2 id="title"></h2>
		<label>Name</label>
		<input id="name" type="text">
		<textarea id="script" class="script" rows="14" spellcheck="false"></textarea>
		<label>Schedule (cron: minute hour day-of-month month day-of-week)</label>
		<input id="sched" type="text" placeholder="0 6 * * *" spellcheck="false">
		<div id="schedHelp" class="help"></div>
		<div class="buttons">
			<button onclick="closeModal()">Cancel</button>
			<button id="save">Save</button>
		</div>
	</div>
</div>
<div id="confirmModal" class="modal" onclick="if (event.target === this) closeConfirm()">
	<div class="box">
		<h2 id="confirmTitle"></h2>
		<p id="confirmText"></p>
		<div class="buttons">
			<button onclick="closeConfirm()">Cancel</button>
			<button id="confirmOk">Confirm</button>
		</div>
	</div>
</div>
<div id="helpModal" class="modal" onclick="if (event.target === this) closeHelp()">
	<div class="box helpbox">
		<h2>Flow help</h2>

		<h3>What a flow is</h3>
		<p>A flow is a saved list of steps that run around a schedule. When the schedule (a cron expression) fires, each step runs at its own offset from that time: before it, on it, or after it. Flows only run on their schedule while they are <b>Enabled</b>; <b>Run</b> starts one by hand.</p>

		<h3>Writing steps</h3>
		<p>One step per line: <code>offset | command</code>. Lines starting with <code>//</code> are comments.</p>
<pre>-60m | say -1 Server is going to restart in $minutes minutes!
-3m  | #lock
-1m  | #kickall Restarting now
0m   | #shutdown
+90  | #unlock
cancel | say -1 Server restart cancelled.</pre>
		<ul>
			<li><code>-60m</code> runs 60 minutes <b>before</b> the schedule time, <code>0m</code> runs <b>on</b> it, <code>+90</code> runs 90 seconds <b>after</b> it.</li>
			<li>Units are <code>s</code>, <code>m</code> and <code>h</code>. No unit means seconds, so <code>-90</code> is 90 seconds before.</li>
			<li>Any offset other than 0 needs a <code>+</code> or <code>-</code> sign.</li>
			<li><code>$minutes</code> and <code>$seconds</code> in a command become how long before the schedule time that step runs (0 for steps on or after it).</li>
			<li><code>cancel | command</code> lines run only if you press Stop before the schedule time arrives. Use them to undo a countdown, for example a "restart cancelled" message.</li>
		</ul>

		<h3>Commands you can use</h3>
		<ul>
			<li>Any BattlEye RCON command, such as <code>say -1 text</code>, <code>#lock</code>, <code>#unlock</code>, <code>#shutdown</code> or <code>kick 3 reason</code>.</li>
			<li><code>kickall [reason]</code> or <code>#kickall [reason]</code> kicks every connected player. With no reason it uses "Server restarting".</li>
		</ul>

		<h3>Schedule (cron)</h3>
		<p>Five fields: <code>minute hour day-of-month month day-of-week</code>, in the server's time zone. The text under the field explains in plain English what you typed and shows the next runs. The schedule time is the <code>0m</code> point, so a flow starting at <code>-60m</code> begins its countdown an hour earlier.</p>
		<ul>
			<li><code>0 6 * * *</code> every day at 06:00</li>
			<li><code>0 */6 * * *</code> every 6 hours (00:00, 06:00, 12:00, 18:00)</li>
			<li><code>30 4 * * 1,4</code> Mondays and Thursdays at 04:30</li>
			<li><code>0 5 * * 1-5</code> weekdays at 05:00</li>
			<li><code>@daily</code>, <code>@hourly</code>, <code>@weekly</code>, <code>@monthly</code> shortcuts also work.</li>
		</ul>

		<h3>Buttons</h3>
		<ul>
			<li><b>Enable / Disable</b> turns the schedule on or off. Enable needs a schedule.</li>
			<li><b>Run / Stop</b> switches with the flow's state. Run starts the flow immediately: the earliest step fires right away and the rest follow at their offsets. Stop halts a running flow.</li>
			<li><b>Edit</b> changes the steps or schedule. <b>Delete</b> removes the flow (not while it is running).</li>
			<li>Drag a row up or down to reorder the list.</li>
			<li>The <b>#Lock</b>, <b>#Unlock</b>, <b>#Shutdown</b> and <b>#Kick All</b> buttons at the top right send that command to the server immediately. Shutdown and Kick All open a confirm/cancel dialog first.</li>
		</ul>

		<h3>Tips</h3>
		<ul>
			<li>Put <code>#kickall</code> at least a minute before <code>0m | #shutdown</code>, since the sweep takes a few seconds.</li>
			<li>Set <code>+90</code> (or more) for <code>#unlock</code> to leave time for the server to restart and RCON to log in again.</li>
			<li>Use <code>say -1</code> for messages to everyone; <code>say &lt;id&gt;</code> messages one player.</li>
			<li>If the app restarts during a scheduled countdown it rejoins it and skips steps whose time has passed. Manually started runs are not resumed.</li>
			<li>A flow runs one copy at a time. To start one from a task scheduler, POST to <code>/api/flows/&lt;id&gt;/run</code>.</li>
		</ul>

		<div class="buttons"><button onclick="closeHelp()">Close</button></div>
	</div>
</div>
<script>
let flows = [];
let editing = null;
let previewTimer = null;
let dragging = false;
let dragId = null;

function esc(s) {
	return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

async function post(url, body) {
	let r = await fetch(url, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body || {})
	});
	return r.json();
}

function fmtRemaining(unix) {
	let s = Math.round(unix - Date.now() / 1000);
	if (s <= 0) return 'now';
	let h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
	return (h ? h + 'h ' : '') + m + 'm ' + String(s % 60).padStart(2, '0') + 's';
}

function statusText(run) {
	if (!run) return 'Idle';
	if (run.active) {
		if (run.anchorUnix > Date.now() / 1000) return 'Running - schedule time in ' + fmtRemaining(run.anchorUnix);
		return 'Running - finishing up';
	}
	return 'Last run: ' + run.status;
}

function btn(label, onclick, disabled) {
	return '<button' + (disabled ? ' disabled' : '') + ' onclick="' + onclick + '">' + label + '</button>';
}

async function refresh() {
	if (dragging) return; // do not rebuild rows under a drag in progress
	try {
		let r = await fetch('/api/flows');
		let d = await r.json();
		if (!d.ok) {
			document.getElementById('status').textContent = (d.error || 'Server unavailable') + ' — retrying...';
			return;
		}
		flows = d.flows || [];
		let runs = {};
		(d.runs || []).forEach(function(x) { runs[x.flowId] = x; });
		document.getElementById('status').textContent = 'Connected — ' + flows.length + ' flow(s)';
		document.getElementById('flows').innerHTML = flows.map(function(f) {
			let run = runs[f.id];
			let active = !!(run && run.active);
			let hasCron = !!(f.schedule && f.schedule.trim());
			let next = f.scheduleEnabled ? (f.nextRun || '-') : (hasCron ? 'Disabled' : '-');
			let buttons =
				btn(f.scheduleEnabled ? 'Disable' : 'Enable', 'toggleFlow(' + f.id + ')', false) +
				(active ? btn('Stop', 'stopFlow(' + f.id + ')', false) : btn('Run', 'runFlow(' + f.id + ')', false)) +
				btn('Edit', 'editFlow(' + f.id + ')', false) +
				btn('Delete', 'deleteFlow(' + f.id + ')', active);
			return '<tr draggable="true" data-id="' + f.id + '"><td>' + esc(f.name) + '</td>' +
				'<td class="mono">' + (hasCron ? esc(f.schedule) : '-') + '</td>' +
				'<td>' + esc(statusText(run)) + '</td>' +
				'<td>' + esc(next) + '</td>' +
				'<td class="actions">' + buttons + '</td></tr>';
		}).join('');
		document.getElementById('runs').innerHTML = flows.filter(function(f) { return runs[f.id]; }).map(function(f) {
			let x = runs[f.id];
			return '<div class="run"><b>' + esc(f.name) + '</b> - ' + esc(statusText(x)) +
				'<pre class="log">' + esc((x.log || []).join('\n')) + '</pre></div>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable — retrying...';
	}
}

function find(id) { return flows.find(function(x) { return x.id === id; }); }

async function toggleFlow(id) {
	let f = find(id);
	let d = await post('/api/flows/' + id + '/' + (f.scheduleEnabled ? 'disable' : 'enable'));
	if (!d.ok) alert(d.error || 'Failed');
	refresh();
}

async function runFlow(id) {
	let d = await post('/api/flows/' + id + '/run');
	if (!d.ok) alert(d.error || 'Run failed');
	refresh();
}

async function stopFlow(id) {
	if (!confirm('Stop the running flow?')) return;
	let d = await post('/api/flows/' + id + '/stop');
	if (!d.ok) alert(d.error || 'Stop failed');
	refresh();
}

async function deleteFlow(id) {
	let f = find(id);
	if (!confirm('Delete flow "' + f.name + '"?')) return;
	let d = await post('/api/flows/' + id + '/delete');
	if (!d.ok) alert(d.error || 'Delete failed');
	refresh();
}

let quickTimer = null;

const quickConfirm = {
	shutdown: { title: 'Shut down server?', text: 'This sends #shutdown to the server immediately.', ok: 'Shut down' },
	kickall: { title: 'Kick all players?', text: 'This kicks every connected player immediately.', ok: 'Kick all' }
};

function quick(cmd) {
	let c = quickConfirm[cmd];
	if (!c) { sendQuick(cmd); return; }
	document.getElementById('confirmTitle').textContent = c.title;
	document.getElementById('confirmText').textContent = c.text;
	let ok = document.getElementById('confirmOk');
	ok.textContent = c.ok;
	ok.onclick = function() { closeConfirm(); sendQuick(cmd); };
	document.getElementById('confirmModal').style.display = 'flex';
}

function closeConfirm() { document.getElementById('confirmModal').style.display = 'none'; }

async function sendQuick(cmd) {
	let msg = document.getElementById('quickMsg');
	msg.textContent = 'Sending...';
	try {
		let d = await post('/api/flows/command', { command: cmd });
		msg.textContent = d.ok ? (d.message || 'Sent') : (d.error || 'Failed');
	} catch(e) {
		msg.textContent = 'Request failed';
	}
	clearTimeout(quickTimer);
	quickTimer = setTimeout(function() { msg.textContent = ''; }, 8000);
	refresh();
}

function openHelp() { document.getElementById('helpModal').style.display = 'flex'; }
function closeHelp() { document.getElementById('helpModal').style.display = 'none'; }

function openModal(f, title) {
	editing = f;
	document.getElementById('title').textContent = title;
	document.getElementById('name').value = f.name;
	document.getElementById('script').value = f.script;
	document.getElementById('sched').value = f.schedule || '';
	document.getElementById('modal').style.display = 'flex';
	updatePreview();
}

function newFlow() {
	openModal({ id: 0, name: '', script: '-5m | say -1 Restarting in $minutes minutes!\n0m | #shutdown\n', schedule: '', scheduleEnabled: false }, 'New flow');
}

function editFlow(id) { openModal(find(id), 'Edit flow'); }

function closeModal() { document.getElementById('modal').style.display = 'none'; }

function queuePreview() {
	clearTimeout(previewTimer);
	previewTimer = setTimeout(updatePreview, 300);
}

async function updatePreview() {
	let help = document.getElementById('schedHelp');
	try {
		let d = await post('/api/flows/preview', {
			schedule: document.getElementById('sched').value,
			script: document.getElementById('script').value
		});
		if (!d.ok) {
			help.className = 'help bad';
			help.textContent = d.error || 'Invalid schedule';
			return;
		}
		help.className = 'help';
		let html = '<b>' + esc(d.description) + '</b>';
		if (d.next && d.next.length) html += '<br>Next runs: ' + d.next.map(esc).join(' &middot; ');
		if (d.starts) html += '<br>Next countdown starts: ' + esc(d.starts);
		if (d.span) html += '<br>' + esc(d.span);
		help.innerHTML = html;
	} catch(e) {
		help.textContent = '';
	}
}

document.getElementById('sched').addEventListener('input', queuePreview);
document.getElementById('script').addEventListener('input', queuePreview);

document.getElementById('save').onclick = async () => {
	let d = await post('/api/flows', {
		id: editing.id,
		name: document.getElementById('name').value,
		script: document.getElementById('script').value,
		schedule: document.getElementById('sched').value,
		scheduleEnabled: !!editing.scheduleEnabled
	});
	if (!d.ok) { alert(d.error || 'Save failed'); return; }
	closeModal();
	refresh();
};

function clearDragMarks() {
	Array.prototype.forEach.call(document.querySelectorAll('#flows tr'), function(tr) {
		tr.classList.remove('dragging', 'drop-above', 'drop-below');
	});
}

function endDrag() {
	dragging = false;
	dragId = null;
	clearDragMarks();
}

function dropBefore(e, tr) {
	let rect = tr.getBoundingClientRect();
	return e.clientY < rect.top + rect.height / 2;
}

let flowsBody = document.getElementById('flows');

flowsBody.addEventListener('dragstart', function(e) {
	let tr = e.target.closest('tr');
	if (!tr) return;
	dragging = true;
	dragId = parseInt(tr.getAttribute('data-id'), 10);
	e.dataTransfer.effectAllowed = 'move';
	e.dataTransfer.setData('text/plain', String(dragId)); // Firefox needs data to start a drag
	tr.classList.add('dragging');
});

flowsBody.addEventListener('dragover', function(e) {
	if (!dragging) return;
	let tr = e.target.closest('tr');
	if (!tr) return;
	e.preventDefault();
	e.dataTransfer.dropEffect = 'move';
	tr.classList.remove('drop-above', 'drop-below');
	if (parseInt(tr.getAttribute('data-id'), 10) !== dragId) {
		tr.classList.add(dropBefore(e, tr) ? 'drop-above' : 'drop-below');
	}
});

flowsBody.addEventListener('dragleave', function(e) {
	let tr = e.target.closest('tr');
	if (tr) tr.classList.remove('drop-above', 'drop-below');
});

flowsBody.addEventListener('drop', async function(e) {
	e.preventDefault();
	let tr = e.target.closest('tr');
	if (!tr || !dragging) { endDrag(); return; }
	let targetId = parseInt(tr.getAttribute('data-id'), 10);
	let before = dropBefore(e, tr);
	let moved = dragId;
	endDrag();
	if (targetId === moved) return;
	let ids = flows.map(function(f) { return f.id; }).filter(function(id) { return id !== moved; });
	let at = ids.indexOf(targetId);
	if (!before) at++;
	ids.splice(at, 0, moved);
	let d = await post('/api/flows/reorder', { ids: ids });
	if (!d.ok) alert(d.error || 'Reorder failed');
	refresh();
});

flowsBody.addEventListener('dragend', endDrag);

refresh();
setInterval(refresh, {{.RefreshInterval.Milliseconds}});
</script>
</body>
</html>`))

func handleFlowsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := flowsPage.Execute(w, pageSettings); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

// setupFlows is the single entry point main.go needs: it creates the
// flows table, registers the page and API routes on mux, and starts the scheduler.
func setupFlows(mux *http.ServeMux) error {
	if err := initFlows(); err != nil {
		return err
	}
	mux.HandleFunc("/flows", handleFlowsPage)
	mux.HandleFunc("/api/flows", handleFlowsAPI)
	mux.HandleFunc("/api/flows/", handleFlowAction)
	startScheduler()
	return nil
}
