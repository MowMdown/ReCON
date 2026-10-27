package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// flows_schedule.go: the Schedule trigger
// ----------------------------------------------------------------------------
// A scheduled flow runs on a cron schedule while it is Enabled, its first
// step at the cron time plus the flow's delay. The scheduler has no tick: it
// watches the clock and sleeps until the next first step is due.
// ============================================================================

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

func parseCronExpression(expr string) (*cronExpr, error) {
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
// Scheduler
// ---------------------------------------------------------------------------

// scheduleGrace lets a run whose start just passed (ReCON restarting a few
// seconds before it, a flow still busy with its last run) still start
// instead of being skipped.
const scheduleGrace = 15 * time.Second

// scheduleBusyRetry is how soon a flow still busy with its last run is
// tried again, within scheduleGrace.
const scheduleBusyRetry = time.Second

// scheduleIdleWait is the longest the scheduler sleeps without looking
// again, so a change to the clock (or time zone rules) is noticed.
const scheduleIdleWait = time.Hour

// lastFired is the latest cron time started for each flow, so a finished or
// stopped run isn't started again for the same time. Only the scheduler
// goroutine touches it.
var lastFired = map[int64]time.Time{}

// scheduleChanged wakes the scheduler when flows change (saved, enabled,
// disabled or deleted), so it can work out its next wake-up again.
var scheduleChanged = make(chan struct{}, 1)

// wakeFlowScheduler tells the scheduler that flows changed.
func wakeFlowScheduler() {
	select {
	case scheduleChanged <- struct{}{}:
	default: // already told
	}
}

// startFlowScheduler runs the scheduled flows. There is no tick: it sleeps until
// the next run's first step is due (cron time plus the flow's delay), starts
// it right on time, and works out the next one; a change to the flows wakes
// it early.
func startFlowScheduler() {
	go func() {
		timer := time.NewTimer(0)
		for {
			select {
			case <-timer.C:
			case <-scheduleChanged:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			next := runDueSchedules(time.Now())
			timer.Reset(max(time.Until(next), 0))
		}
	}()
}

// runDueSchedules starts, for every enabled scheduled flow, the first cron
// time that hasn't been started yet and whose first step (cron time plus the
// delay) is due by now. A run more than scheduleGrace late is skipped. It
// returns when it should be called next: the earliest first step still to
// come.
func runDueSchedules(now time.Time) time.Time {
	wake := now.Add(scheduleIdleWait)
	flows, err := listFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return now.Add(time.Minute)
	}
	// Forget deleted flows: their numbers are reused by new flows, which
	// must not look as if they had already run.
	exists := make(map[int64]bool, len(flows))
	for _, f := range flows {
		exists[f.ID] = true
	}
	for id := range lastFired {
		if !exists[id] {
			delete(lastFired, id)
		}
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != triggerSchedule || checkFlowDefinition(f) != nil {
			continue
		}
		c, err := parseCronExpression(f.Schedule)
		if err != nil {
			continue
		}
		delay, _ := flowDelaySeconds(f)
		d := time.Duration(delay) * time.Second
		from := now.Add(-scheduleGrace - d)
		if lf, ok := lastFired[f.ID]; ok && lf.After(from) {
			from = lf
		}
		t, ok := c.next(from)
		if !ok {
			continue
		}
		if start := t.Add(d); start.After(now) {
			wake = minTime(wake, start) // not due yet
			continue
		}
		if err := launchFlowRun(f, t.Add(d), sourceSchedule, nil); errors.Is(err, errFlowRunning) {
			wake = minTime(wake, now.Add(scheduleBusyRetry)) // busy: try again shortly
			continue
		} else if err != nil {
			log.Printf("scheduler: %s: %v", f.Name, err)
		}
		lastFired[f.ID] = t
		if t2, ok := c.next(t); ok {
			wake = minTime(wake, t2.Add(d))
		}
	}
	return wake
}

// minTime is the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
