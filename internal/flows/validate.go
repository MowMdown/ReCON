package flows

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	_ "time/tzdata"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// maxSteps and maxDepth keep a flow to a size the page can show.
const (
	maxSteps = 500
	maxDepth = 10
)

var stepTypes = map[string]bool{"message": true, "playerList": true, "wait": true, "lock": true,
	"unlock": true, "shutdown": true, "kick": true, "ban": true, "kickAll": true, "command": true,
	"stop": true, "if": true, "forEach": true}

// triggerHas is what a trigger provides: a player, and a chat message.
func triggerHas(trigger string) map[string]bool {
	return map[string]bool{
		needNothing: true,
		needPlayer:  trigger == triggerJoin || trigger == triggerChat,
		needChat:    trigger == triggerChat,
	}
}

type stepChecker struct {
	has    map[string]bool
	n      int    // step number, counted as the page numbers them
	inLoop bool   // inside a For each player
	where  string // "Step" or "When stopped, step"
}

func (c *stepChecker) fail(msg string) error { return fmt.Errorf("%s %d: %s", c.where, c.n, msg) }

// tokensOK reports the first placeholder in text the trigger can't fill.
func (c *stepChecker) tokensOK(text string) error {
	for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
		need, ok := flowTokens[m[1]]
		if need == needThreshold && !c.has[need] {
			return c.fail(m[0] + " " + needText[need])
		}
		if ok && need != needNothing && need != needThreshold && !c.has[need] {
			return c.fail(m[0] + " " + needText[need])
		}
	}
	return nil
}

func (c *stepChecker) checkStepList(list []Step, depth int) error {
	if depth > maxDepth {
		return c.fail(fmt.Sprintf("Ifs can go %d deep at most", maxDepth))
	}
	for i := range list {
		s := &list[i]
		c.n++
		if !stepTypes[s.Type] {
			return c.fail(fmt.Sprintf("unknown step %q", s.Type))
		}
		player := c.has[needPlayer]
		switch s.Type {
		case "message", "playerList":
			switch s.To {
			case "all":
			case "player":
				if !player {
					return c.fail(`"Player" ` + needText[needPlayer])
				}
			default:
				return c.fail("pick who it goes to")
			}
			if s.Type == "message" && s.Text == "" {
				return c.fail("Add the message text")
			}
		case "wait":
			secs, ok := parseDur(s.Time)
			if !ok || secs <= 0 {
				return c.fail("Enter a time: 30 (seconds), 90s, 1m20s or 1h")
			}
			if secs > maxTimeSecs {
				return c.fail("A wait can be 24h at most")
			}
		case "kick", "ban":
			if !player {
				return c.fail("Kick and Ban " + needText[needPlayer])
			}
			s.Who = "player"
			if s.Type == "ban" {
				if n, err := strconv.Atoi(s.Len); err != nil || n < 0 {
					return c.fail("pick how long the ban lasts")
				}
			}
		case "command":
			if s.Cmd == "" {
				return c.fail("Type the command to send")
			}
		case "if":
			if err := c.checkIfStep(s, depth); err != nil {
				return err
			}
			continue // the branches checked their own steps
		case "forEach":
			if err := c.checkForEach(s, depth); err != nil {
				return err
			}
			continue
		}
		for _, t := range []string{s.Text, s.Reason, s.Cmd} {
			if err := c.tokensOK(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkForEach checks a For each's steps with a player (each one in turn)
// and a {message} (that player's line) available.
func (c *stepChecker) checkForEach(s *Step, depth int) error {
	if c.inLoop {
		return c.fail("a For each can't go inside another one")
	}
	if s.Of != forEachPlayer && s.Of != forEachMessage {
		return c.fail("pick what For each goes through: player or message")
	}
	outer := c.has
	c.has = map[string]bool{needNothing: true, needPlayer: true, needChat: true, needThreshold: outer[needThreshold]}
	c.inLoop = true
	err := c.checkStepList(s.Steps, depth+1)
	c.has, c.inLoop = outer, false
	return err
}

func (c *stepChecker) checkIfStep(s *Step, depth int) error {
	if len(s.Branches) == 0 {
		return c.fail("this If has no condition")
	}
	first := s.Branches[0].Subject
	sub, ok := condSubjects[first]
	if !ok {
		return c.fail(fmt.Sprintf("unknown If subject %q", first))
	}
	if !c.has[sub.need] {
		return c.fail("{" + first + "} " + needText[sub.need])
	}
	num := c.n // the If's own number, for its conditions' errors
	for _, b := range s.Branches {
		if b.Subject != first {
			return fmt.Errorf("%s %d: every Else if must compare {%s}, like its If", c.where, num, first)
		}
		okOp := false
		for _, op := range condOps[sub.kind] {
			okOp = okOp || op == b.Op
		}
		if !okOp {
			return fmt.Errorf("%s %d: pick an operator", c.where, num)
		}
		switch {
		case sub.kind == "num" && strings.TrimSpace(b.Value) == counterThresholdToken:
			if !c.has[needThreshold] {
				return fmt.Errorf("%s %d: %s %s", c.where, num, counterThresholdToken, needText[needThreshold])
			}
		case sub.kind == "num" || first == "player_id":
			if _, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil {
				return fmt.Errorf("%s %d: Enter a whole number", c.where, num)
			}
		case sub.kind == "role":
			if n, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil || n < RolePlayer || n > RoleAdmin {
				return fmt.Errorf("%s %d: pick Player, Mod or Admin", c.where, num)
			}
		case first == "player_guid":
			if !battleye.GUIDPattern.MatchString(strings.TrimSpace(b.Value)) {
				return fmt.Errorf("%s %d: Enter a GUID (32 letters and digits, 0-9 and a-f)", c.where, num)
			}
		case sub.kind == "word":
			if len(terms(b.Value)) == 0 {
				return fmt.Errorf(`%s %d: Enter a word, several words, or a "quoted phrase"`, c.where, num)
			}
		}
	}
	counters := []string{s.ElseCounter}
	for _, b := range s.Branches {
		counters = append(counters, b.Counter)
	}
	for _, op := range counters {
		if _, ok := counterOps[op]; !ok {
			return fmt.Errorf("%s %d: pick what the counter does: 0, ++, -- or reset", c.where, num)
		}
		if op != "" && !c.has[needPlayer] {
			return fmt.Errorf("%s %d: the counter is kept for each player, so it needs a player: use it inside a For each, or with the On join or Player message trigger", c.where, num)
		}
	}
	for _, l := range branchLists(s) {
		if err := c.checkStepList(l, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// checkFlowSteps validates a flow's steps and its When stopped steps for its
// trigger and Counter threshold (0: none). When stopped takes every kind of
// step, with the same rules. Errors name the step as the page numbers them.
func checkFlowSteps(trigger string, threshold int, steps, onStop []Step) error {
	if len(steps) == 0 {
		return errors.New("add at least one step")
	}
	if countAllSteps(steps)+countAllSteps(onStop) > maxSteps {
		return fmt.Errorf("a flow can have %d steps at most", maxSteps)
	}
	c := &stepChecker{has: triggerHas(trigger), where: "Step"}
	c.has[needThreshold] = threshold > 0
	if err := c.checkStepList(steps, 0); err != nil {
		return err
	}
	if len(onStop) > 0 && trigger != triggerSchedule && trigger != triggerManual {
		return errors.New("only Schedule and Manual flows have When stopped steps")
	}
	s := &stepChecker{has: triggerHas(trigger), where: "When stopped, step"}
	s.has[needThreshold] = threshold > 0
	return s.checkStepList(onStop, 0)
}
