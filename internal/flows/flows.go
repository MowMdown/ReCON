package flows

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

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
