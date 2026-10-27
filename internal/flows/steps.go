package flows

import (
	"regexp"
	"strconv"
	"strings"
	_ "time/tzdata"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// maxTimeSecs is the longest time a flow can use (a wait or the delay). It
// matches the 24 hour safety limit on a run.
const maxTimeSecs = 86400

var durRe = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// parseDur reads a time as typed on the pages: a plain number is seconds
// ("90"), or h, m and s can be combined, largest first ("90s", "1m20s",
// "2h 15m"). Spaces and case are ignored. ok is false for anything else.
func parseDur(s string) (int, bool) {
	s = strings.ToLower(strings.Join(strings.Fields(s), ""))
	if s == "" {
		return 0, false
	}
	m := durRe.FindStringSubmatch(s)
	plain := m == nil && strings.Trim(s, "0123456789") == ""
	if m == nil && !plain {
		return 0, false
	}
	if plain {
		m = []string{s, "", "", s}
	}
	if m[1] == "" && m[2] == "" && m[3] == "" {
		return 0, false
	}
	total := 0
	for i, mul := range []int{3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n > 10*maxTimeSecs {
			return 10 * maxTimeSecs, true // too long; callers check the limit
		}
		total += n * mul
	}
	return total, true
}

// fmtDur writes seconds for log lines: 1h 5m 20s, 45s, 0s.
func fmtDur(secs int) string {
	if secs <= 0 {
		return "0s"
	}
	var parts []string
	if h := secs / 3600; h > 0 {
		parts = append(parts, strconv.Itoa(h)+"h")
	}
	if m := secs % 3600 / 60; m > 0 {
		parts = append(parts, strconv.Itoa(m)+"m")
	}
	if s := secs % 60; s > 0 {
		parts = append(parts, strconv.Itoa(s)+"s")
	}
	return strings.Join(parts, " ")
}

// Step is one row of a flow, stored as JSON in the flows table. Which fields
// are used depends on Type:
//
//	message     To (all | player), Text
//	playerList  To: sends "Online (N): name, name, ..."
//	wait        Time ("30", "1m20s")
//	lock, unlock, shutdown
//	kick        Who (player), Reason
//	ban         Who (player), Len (minutes, 0 = permanent), Reason
//	kickAll     Reason
//	command     Cmd: any RCON command
//	stop        ends the run
//	if          Branches (the If, then any Else ifs) and Else. Each branch,
//	            and the Else (ElseCounter), can change the player's counter
//	            when it runs (see Branch.Counter)
//	forEach     Of (player | message) and Steps. player: the steps run once
//	            for every player online, as "the player", with {message} their
//	            latest chat line this session. message: once for every chat
//	            line players online sent this session that this flow hasn't
//	            handled yet, as the player who sent it, with {message} the
//	            line. Ifs inside decide who or what qualifies.
type Step struct {
	Type     string   `json:"type"`
	To       string   `json:"to,omitempty"`
	Text     string   `json:"text,omitempty"`
	Time     string   `json:"time,omitempty"`
	Who      string   `json:"who,omitempty"`
	Len      string   `json:"len,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Cmd      string   `json:"cmd,omitempty"`
	Branches []Branch `json:"branches,omitempty"`
	Else     *[]Step  `json:"else,omitempty"`  // nil: no Else
	Steps    []Step   `json:"steps,omitempty"` // forEach: the steps run for each player or message
	Of       string   `json:"of,omitempty"`    // forEach: "player" or "message"
	// ElseCounter is what the Else does to the player's counter, like Branch.Counter.
	ElseCounter string `json:"elseCounter,omitempty"`
}

// Branch is the If or one Else if: a condition and the steps it runs. Every
// branch of one If compares the same Subject; only Op and Value differ.
type Branch struct {
	Subject string `json:"subject"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Steps   []Step `json:"steps"`
	// Counter is what this branch does to the player's counter for this
	// run when it is the one that runs, before its steps: "" nothing,
	// "inc" +1, "dec" -1 (not below 0) or "reset" back to 0.
	Counter string `json:"counter,omitempty"`
}

// Counter changes an If branch or Else can make (Branch.Counter).
var counterOps = map[string]string{"": "", "inc": "++", "dec": "--", "reset": "reset"}

// maxCounterThreshold is the highest flow counter threshold. 0 means the
// flow has none, and then {threshold} can't be used.
const maxCounterThreshold = 1000

// counterThresholdToken is the one placeholder a number If may compare to.
const counterThresholdToken = "{threshold}"

// What a For each goes through.
const (
	forEachPlayer  = "player"
	forEachMessage = "message"
)

// isBlock reports whether a step holds other steps (If, For each).
func isBlock(s *Step) bool { return s.Type == "if" || s.Type == "forEach" }

// branchLists is a block's step lists in order: an If's branches, then its
// Else; a For each player's one list.
func branchLists(s *Step) [][]Step {
	if s.Type == "forEach" {
		return [][]Step{s.Steps}
	}
	out := make([][]Step, 0, len(s.Branches)+1)
	for _, b := range s.Branches {
		out = append(out, b.Steps)
	}
	if s.Else != nil {
		out = append(out, *s.Else)
	}
	return out
}

// normalizeFlowSteps gives every branch and Else a non-nil list, so the page always
// gets [] rather than null, and trims the text fields.
func normalizeFlowSteps(list []Step) []Step {
	if list == nil {
		return []Step{}
	}
	for i := range list {
		s := &list[i]
		s.Type = strings.TrimSpace(s.Type)
		s.To, s.Who, s.Len = strings.TrimSpace(s.To), strings.TrimSpace(s.Who), strings.TrimSpace(s.Len)
		s.Text, s.Reason, s.Cmd = battleye.CleanReason(s.Text), battleye.CleanReason(s.Reason), battleye.CleanReason(s.Cmd)
		s.Time = strings.TrimSpace(s.Time)
		for j := range s.Branches {
			b := &s.Branches[j]
			b.Subject, b.Op, b.Value = strings.TrimSpace(b.Subject), strings.TrimSpace(b.Op), battleye.CleanReason(b.Value)
			b.Counter = strings.TrimSpace(b.Counter)
			b.Steps = normalizeFlowSteps(b.Steps)
		}
		if s.Else != nil {
			e := normalizeFlowSteps(*s.Else)
			s.Else = &e
		} else {
			s.ElseCounter = ""
		}
		s.ElseCounter = strings.TrimSpace(s.ElseCounter)
		if s.Type == "forEach" {
			s.Steps = normalizeFlowSteps(s.Steps)
			if s.Of = strings.TrimSpace(s.Of); s.Of == "" {
				s.Of = forEachPlayer
			}
		}
	}
	return list
}

// countAllSteps counts every step, inside Ifs and For each player too.
func countAllSteps(list []Step) int {
	n := 0
	for i := range list {
		n++
		if isBlock(&list[i]) {
			for _, l := range branchLists(&list[i]) {
				n += countAllSteps(l)
			}
		}
	}
	return n
}

// flowRunLength is how long the waits take, counting the longest branch of each
// If, and a For each player's waits once (they repeat for every player).
func flowRunLength(list []Step) int {
	total := 0
	for i := range list {
		s := &list[i]
		switch s.Type {
		case "wait":
			secs, _ := parseDur(s.Time)
			total += secs
		case "if":
			longest := 0
			for _, l := range branchLists(s) {
				if n := flowRunLength(l); n > longest {
					longest = n
				}
			}
			total += longest
		case "forEach":
			total += flowRunLength(s.Steps)
		}
	}
	return total
}
