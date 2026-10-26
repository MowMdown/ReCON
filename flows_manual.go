package main

import (
	"context"
	"errors"
	"time"
)

// ============================================================================
// flows_manual.go: the Manual trigger
// ----------------------------------------------------------------------------
// A manual flow runs only when Run is pressed on the Flows page. Run also
// starts a scheduled flow now, and Stop ends any flow's active run.
// ============================================================================

// startFlowNow starts a run now (the Run button). minutes > 0 replaces the
// flow's delay for this run. Join and chat flows run for a player, so Run
// can't start them.
func startFlowNow(f Flow, minutes int) error {
	if err := checkFlowDefinition(f); err != nil {
		return err
	}
	switch f.Trigger {
	case triggerJoin:
		return errors.New("this flow runs for each player who joins, so Run can't start it")
	case triggerChat:
		return errors.New("this flow runs for each player message, so Run can't start it")
	}
	delay, _ := flowDelaySeconds(f)
	if minutes > 0 {
		delay = minutes * 60
	}
	return launchFlowRun(f, time.Now().Add(time.Duration(delay)*time.Second), sourceManual, nil)
}

// stopFlowRun cancels the active run of a flow (the Stop button). The run
// then runs its When stopped steps; pressing Stop again while they run ends
// them too.
func stopFlowRun(id int64) error {
	flowRuns.Lock()
	run := flowRuns.byFlow[id]
	active := run != nil && run.active
	var stopCancel context.CancelFunc
	if active {
		stopCancel = run.stopCancel
	}
	flowRuns.Unlock()
	if !active {
		return errFlowNotRunning
	}
	run.cancel()
	if stopCancel != nil {
		stopCancel()
	}
	return nil
}
