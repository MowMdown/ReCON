package flows

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/MowMdown/ReCON/internal/battleye"
)

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
	// pings and anyone the console log missed (players.PollNow).
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
// commands, which rcon.Run already logs to the container.
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
