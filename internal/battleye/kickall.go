package battleye

import (
	"fmt"
	"strings"
	"time"
)

// KickReport summarises what a kickall run did (returned as JSON by /kickall).
type KickReport struct {
	Order     []int  `json:"order"`         // IDs in the order they were kicked
	Unacked   []int  `json:"unacked"`       // kick commands the server never acknowledged
	Remaining []int  `json:"remaining"`     // IDs still connected after the sweep
	Raw       string `json:"raw,omitempty"` // raw `players` output, only set when nothing could be parsed
}

// summary is the one log line for a kickall, e.g.
//
//	kickall "Server restarting": kicked [3 0], no ack for [], still connected []
func (rep *KickReport) Summary(reason string) string {
	return fmt.Sprintf("kickall %q: kicked %v, no ack for %v, still connected %v", CleanReason(reason), rep.Order, rep.Unacked, rep.Remaining)
}

// commandRunner is what KickAll needs: a Session (CLI,
// console) or webRunner (server mode).
type commandRunner interface {
	Run(command string, wait time.Duration) (string, bool, error)
}

// KickAll kicks every connected player and nothing else (#lock,
// #shutdown and the like are separate commands, sent before or after):
//  1. "players", then "kick <id> <reason>" for each, highest ID first
//  2. wait 500ms and re-read "players" to report who is still connected
//
// A transport error in step 1 aborts with an error. Unacknowledged kicks and
// a failed verification are reported in the report.
func KickAll(s commandRunner, reason string) (*KickReport, error) {
	reason = CleanReason(reason)
	out, _, err := s.Run("players", 3*time.Second)
	if err != nil {
		return nil, err
	}
	rep := &KickReport{Order: playerIDs(out), Unacked: []int{}, Remaining: []int{}}
	if len(rep.Order) == 0 {
		rep.Raw = out // help the caller debug why nothing was parsed
	}
	for _, id := range rep.Order {
		cmd := strings.TrimSpace(fmt.Sprintf("kick %d %s", id, reason))
		if _, acked, err := s.Run(cmd, time.Second); err != nil {
			return nil, err
		} else if !acked {
			rep.Unacked = append(rep.Unacked, id)
		}
	}
	if len(rep.Order) > 0 {
		time.Sleep(500 * time.Millisecond) // let the server process the kicks
		if out, _, err := s.Run("players", 3*time.Second); err == nil {
			rep.Remaining = playerIDs(out)
		}
	}
	return rep, nil
}
