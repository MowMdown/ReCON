package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "time/tzdata" // lets the TZ env var work in minimal container images
)

// ============================================================================
// flows.go overview
// ----------------------------------------------------------------------------
// A flow is a list of steps built on the Flows page: send a message, wait,
// lock the server, kick the player, an If with Else ifs and an Else, and so
// on. Steps run top to bottom; only Wait takes time.
//
// Every flow has one trigger:
//
//	schedule  a cron schedule, while Enabled             flows_schedule.go
//	join      once for each player who finishes loading   flows_join.go
//	          in, while Enabled
//	chat      once for each chat message a player sends,  flows_message.go
//	          while Enabled
//	manual    only when Run is pressed                    flows_manual.go
//
// and an optional delay before step 1. "The player" is whoever triggered a
// join or chat run; {player_name}, {player_id}, {player_ping}, {player_ip},
// {player_guid}, {player_role} and {message} are filled in from them.
// Schedule and Manual flows have no player, so they message everyone only.
//
// This file has what every trigger shares, and the built-in chat commands
// (!help, !shout, !whisper, !players, !vote, !code, !kick, !ban).
//
// Sections: times, steps and conditions, validation, the runner (including
// For each), flow definitions, runs, starting player flows, and the chat
// commands.
// ============================================================================

// ---------------------------------------------------------------------------
// Times: a plain number of seconds, or h, m and s together (90, 1m20s, 2h 15m)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

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
//	if          Branches (the If, then any Else ifs) and Else
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
}

// Branch is the If or one Else if: a condition and the steps it runs. Every
// branch of one If compares the same Subject; only Op and Value differ.
type Branch struct {
	Subject string `json:"subject"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Steps   []Step `json:"steps"`
}

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
		s.Text, s.Reason, s.Cmd = cleanReason(s.Text), cleanReason(s.Reason), cleanReason(s.Cmd)
		s.Time = strings.TrimSpace(s.Time)
		for j := range s.Branches {
			b := &s.Branches[j]
			b.Subject, b.Op, b.Value = strings.TrimSpace(b.Subject), strings.TrimSpace(b.Op), cleanReason(b.Value)
			b.Steps = normalizeFlowSteps(b.Steps)
		}
		if s.Else != nil {
			e := normalizeFlowSteps(*s.Else)
			s.Else = &e
		}
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

// ---------------------------------------------------------------------------
// Placeholders and conditions
// ---------------------------------------------------------------------------

// Roles, by number: what is stored in players.role.
const (
	rolePlayer = 0
	roleMod    = 1
	roleAdmin  = 2
)

var roleNames = []string{"Player", "Mod", "Admin"}

func roleName(r int) string {
	if r < 0 || r >= len(roleNames) {
		return roleNames[0]
	}
	return roleNames[r]
}

// What a placeholder or If subject needs: nothing (any trigger), "player"
// (On join or Player message) or "chat" (Player message only).
const (
	needNothing = ""
	needPlayer  = "player"
	needChat    = "chat"
)

var needText = map[string]string{
	needPlayer: "needs a player: use the On join or Player message trigger",
	needChat:   "needs the Player message trigger",
}

// flowTokens are the placeholders steps can use, and what each needs.
var flowTokens = map[string]string{
	"player_name": needPlayer, "player_id": needPlayer, "player_ping": needPlayer,
	"player_ip": needPlayer, "player_guid": needPlayer, "player_role": needPlayer,
	"message": needChat, "players_online": needNothing,
}

var tokenRe = regexp.MustCompile(`\{(\w+)\}`)

// condSubject is something an If can compare. kind decides the operators:
//
//	word   the chat message: contains, starts with, is exactly
//	text   is, is not, matches (* wildcard), contains, ignoring case
//	ident  is, is not: a player number or GUID, which have no order
//	num    = != > >= < <=
//	role   is, is not, is at least, is at most, with the value picked
//	       from Player, Mod, Admin (stored as 0, 1, 2)
type condSubject struct{ kind, need string }

var condSubjects = map[string]condSubject{
	"message":     {"word", needChat},
	"player_name": {"text", needPlayer},
	"player_id":   {"ident", needPlayer},
	"player_ping": {"num", needPlayer},
	"player_ip":   {"text", needPlayer},
	"player_guid": {"ident", needPlayer},
	"player_role": {"role", needPlayer},
	// How many players are online (the tracker, players.go). Works on
	// every trigger: "If {players_online} > 0, Kick everyone, else Shut down".
	"players_online": {"num", needNothing},
}

var condOps = map[string][]string{
	"word":  {"has", "starts", "exact"},
	"text":  {"eq", "ne", "wild", "has"},
	"ident": {"eq", "ne"},
	"num":   {"eq", "ne", "gt", "ge", "lt", "le"},
	"role":  {"eq", "ne", "ge", "le"},
}

// opText is how each kind's operators read on the page and in the run log.
var opText = map[string]map[string]string{
	"word":  {"has": "contains", "starts": "starts with", "exact": "is exactly"},
	"text":  {"eq": "is", "ne": "is not", "wild": "matches", "has": "contains"},
	"ident": {"eq": "is", "ne": "is not"},
	"num":   {"eq": "=", "ne": "!=", "gt": ">", "ge": ">=", "lt": "<", "le": "<="},
	"role":  {"eq": "is", "ne": "is not", "ge": "is at least", "le": "is at most"},
}

// describeIfCondition writes a condition the way the page shows it, for the run log:
// {player_ping} >= 500, {player_name} is "Survivor", {player_role} is at least Mod.
func describeIfCondition(b Branch) string {
	sub := condSubjects[b.Subject]
	val := b.Value
	switch {
	case sub.kind == "role":
		n, _ := strconv.Atoi(b.Value)
		val = roleName(n)
	case sub.kind == "text" && b.Op != "wild", sub.kind == "word" && b.Op == "exact":
		val = `"` + b.Value + `"`
	}
	return "{" + b.Subject + "} " + opText[sub.kind][b.Op] + " " + val
}

// terms splits a {message} value into words and "quoted phrases", lower case.
func terms(value string) []string {
	var out []string
	rest := strings.TrimSpace(value)
	for rest != "" {
		var t string
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				t, rest = rest[1:], ""
			} else {
				t, rest = rest[1:end+1], rest[end+2:]
			}
		} else {
			end := strings.IndexFunc(rest, unicode.IsSpace)
			if end < 0 {
				end = len(rest)
			}
			t, rest = rest[:end], rest[end:]
		}
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
		rest = strings.TrimSpace(rest)
	}
	return out
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// containsWord reports whether t appears in s as whole words: "noob" is in
// "you noob!" but not in "noobish".
func containsWord(s, t string) bool {
	for from := 0; from <= len(s); {
		i := strings.Index(s[from:], t)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(t):])
		if (i == 0 || !isWordRune(before)) && (i+len(t) == len(s) || !isWordRune(after)) {
			return true
		}
		from = i + 1
	}
	return false
}

// matchWords checks {message}. contains: any of the words or phrases appears
// as whole words. starts with: the message's first word is one of the words
// (so !discord matches "!discord please" but not "!discordx"). is exactly: the
// whole message is the value. Case is ignored.
func matchWords(op, msg, value string) bool {
	if op == "exact" {
		return strings.EqualFold(strings.Join(strings.Fields(msg), " "), strings.Join(strings.Fields(value), " "))
	}
	m := strings.ToLower(strings.TrimSpace(msg))
	first := ""
	if f := strings.Fields(m); len(f) > 0 {
		first = f[0]
	}
	for _, t := range terms(value) {
		if op == "starts" && t == first || op == "has" && containsWord(m, t) {
			return true
		}
	}
	return false
}

// wildMatch matches text against a pattern where * is any run of characters
// and ? is one character, ignoring case.
func wildMatch(pattern, s string) bool {
	q := regexp.QuoteMeta(pattern)
	q = strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(q)
	re, err := regexp.Compile("(?is)^" + q + "$")
	return err == nil && re.MatchString(s)
}

// evaluateIfCondition checks one branch's condition against the subject's value.
func evaluateIfCondition(b Branch, v string) bool {
	sub := condSubjects[b.Subject]
	v = strings.TrimSpace(v)
	switch sub.kind {
	case "word":
		return matchWords(b.Op, v, b.Value)
	case "text":
		switch b.Op {
		case "eq":
			return strings.EqualFold(v, b.Value)
		case "ne":
			return !strings.EqualFold(v, b.Value)
		case "has":
			return strings.Contains(strings.ToLower(v), strings.ToLower(b.Value))
		}
		return wildMatch(b.Value, v)
	case "ident":
		same := strings.EqualFold(v, strings.TrimSpace(b.Value))
		if a, err1 := strconv.Atoi(v); err1 == nil {
			if c, err2 := strconv.Atoi(strings.TrimSpace(b.Value)); err2 == nil {
				same = a == c // player numbers: "03" is 3
			}
		}
		return same == (b.Op == "eq")
	}
	// num and role: role values are compared as 0 Player, 1 Mod, 2 Admin.
	a, err1 := strconv.Atoi(v)
	c, err2 := strconv.Atoi(strings.TrimSpace(b.Value))
	if err1 != nil || err2 != nil {
		return false
	}
	switch b.Op {
	case "eq":
		return a == c
	case "ne":
		return a != c
	case "gt":
		return a > c
	case "ge":
		return a >= c
	case "lt":
		return a < c
	case "le":
		return a <= c
	}
	return false
}

// ---------------------------------------------------------------------------
// Validation (the page checks the same things as you build; this is the
// check that counts)
// ---------------------------------------------------------------------------

// maxSteps and maxDepth keep a flow to a size the page can show.
const (
	maxSteps = 500
	maxDepth = 10
)

var stepTypes = map[string]bool{"message": true, "playerList": true, "wait": true, "lock": true,
	"unlock": true, "shutdown": true, "kick": true, "ban": true, "kickAll": true, "command": true,
	"stop": true, "if": true, "forEach": true}

// stopTypes are the steps allowed in "When stopped": they go out once, with no player.
var stopTypes = map[string]bool{"message": true, "lock": true, "unlock": true, "kickAll": true, "command": true}

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
	onStop bool   // checking "When stopped"
	inLoop bool   // inside a For each player
	where  string // "Step" or "When stopped, step"
}

func (c *stepChecker) fail(msg string) error { return fmt.Errorf("%s %d: %s", c.where, c.n, msg) }

// tokensOK reports the first placeholder in text the trigger can't fill.
func (c *stepChecker) tokensOK(text string) error {
	for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
		if need, ok := flowTokens[m[1]]; ok && need != needNothing && (c.onStop || !c.has[need]) {
			if c.onStop {
				return c.fail(m[0] + " can't be used here: When stopped has no player")
			}
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
		if c.onStop && !stopTypes[s.Type] {
			return c.fail("only Message, Lock, Unlock, Kick everyone and RCON command steps can go here")
		}
		player := c.has[needPlayer] && !c.onStop
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
	c.has = map[string]bool{needNothing: true, needPlayer: true, needChat: true}
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
		case sub.kind == "num" || first == "player_id":
			if _, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil {
				return fmt.Errorf("%s %d: Enter a whole number", c.where, num)
			}
		case sub.kind == "role":
			if n, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil || n < rolePlayer || n > roleAdmin {
				return fmt.Errorf("%s %d: pick Player, Mod or Admin", c.where, num)
			}
		case first == "player_guid":
			if !guidPattern.MatchString(strings.TrimSpace(b.Value)) {
				return fmt.Errorf("%s %d: Enter a GUID (32 letters and digits, 0-9 and a-f)", c.where, num)
			}
		case sub.kind == "word":
			if len(terms(b.Value)) == 0 {
				return fmt.Errorf(`%s %d: Enter a word, several words, or a "quoted phrase"`, c.where, num)
			}
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
// trigger. Errors name the step as the page numbers them.
func checkFlowSteps(trigger string, steps, onStop []Step) error {
	if len(steps) == 0 {
		return errors.New("add at least one step")
	}
	if countAllSteps(steps)+countAllSteps(onStop) > maxSteps {
		return fmt.Errorf("a flow can have %d steps at most", maxSteps)
	}
	c := &stepChecker{has: triggerHas(trigger), where: "Step"}
	if err := c.checkStepList(steps, 0); err != nil {
		return err
	}
	if len(onStop) > 0 && trigger != triggerSchedule && trigger != triggerManual {
		return errors.New("only Schedule and Manual flows have When stopped steps")
	}
	s := &stepChecker{has: map[string]bool{}, onStop: true, where: "When stopped, step"}
	return s.checkStepList(onStop, 0)
}

// ---------------------------------------------------------------------------
// Runner: one job is one run of a flow's steps (for one player, in join and
// chat flows)
// ---------------------------------------------------------------------------

// flowPlayer is the player a join or chat run is for, as they were when it
// started. Their number is looked up again (by GUID) whenever it's used,
// because numbers are reused when players leave.
type flowPlayer struct {
	Player
	role    int
	message string
}

type job struct {
	run *flowRun
	ctx context.Context
	p   *flowPlayer // nil for schedule and manual runs; each player in turn inside a For each player
	// inLoop: running a For each player's steps. Conditions are then only
	// logged when they match (with the player's name), so a check of 60
	// players doesn't fill the run log with "no".
	inLoop bool
}

// sendCommand sends one flow command. Tests replace it.
var sendCommand = runFlowCommand

// currentID is the player's number now, or false if they have left.
func (x *job) currentID() (int, bool) {
	if x.p == nil {
		return 0, false
	}
	if p, ok := onlineByGUID(x.p.GUID); ok {
		return p.ID, true
	}
	return 0, false
}

// placeholderValue is what a placeholder or If subject stands for in this run.
func (x *job) placeholderValue(name string) (string, bool) {
	if name == "players_online" {
		return strconv.Itoa(len(onlineNow())), true
	}
	if x.p == nil {
		return "", false
	}
	switch name {
	case "player_name":
		return bareName(x.p.Name), true
	case "player_id":
		if id, ok := x.currentID(); ok {
			return strconv.Itoa(id), true
		}
		return strconv.Itoa(x.p.ID), true
	case "player_ping":
		return strconv.Itoa(x.p.Ping), true
	case "player_ip":
		return x.p.IP, true
	case "player_guid":
		return x.p.GUID, true
	case "player_role":
		return roleName(x.p.role), true
	case "message":
		return x.p.message, true
	}
	return "", false
}

// ifSubjectValue is what an If compares: the same as value, except that a role
// is its number (0 Player, 1 Mod, 2 Admin) so "is at least Mod" can work.
func (x *job) ifSubjectValue(name string) (string, bool) {
	if name == "player_role" && x.p != nil {
		return strconv.Itoa(x.p.role), true
	}
	return x.placeholderValue(name)
}

// fillPlaceholders replaces the placeholders in a step's text. Unknown {words} are left
// as typed, and what is filled in is not expanded again.
func (x *job) fillPlaceholders(text string) string {
	return cleanReason(tokenRe.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := x.placeholderValue(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	}))
}

// sendChatMessage sends a message to everyone, or to the player. A private message is
// added to the player's chat history, as the Message dialog does.
func (x *job) sendChatMessage(to, text string) {
	if text == "" {
		return
	}
	if to != "player" {
		sendCommand(x.run, "say -1 "+text)
		return
	}
	id, ok := x.currentID()
	if !ok {
		x.run.logf("%s has left; message not sent", bareName(x.p.Name))
		return
	}
	cmd := fmt.Sprintf("say %d %s", id, text)
	sendCommand(x.run, cmd)
	recordSentMessage(x.p.GUID, cmd)
}

// recordSentMessage adds a private message ReCON sent to a player's chat history.
func recordSentMessage(guid, cmd string) {
	if globalDatabase == nil || guid == "" {
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), guid, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// listOnlineNames is the latest player list as "Online (N): a, b, c".
func listOnlineNames() string {
	online := onlineNow()
	names := make([]string, 0, len(online))
	for _, p := range online {
		names = append(names, bareName(p.Name))
	}
	if len(names) == 0 {
		return "Online (0): nobody"
	}
	return fmt.Sprintf("Online (%d): %s", len(names), strings.Join(names, ", "))
}

// banStep bans the player. Tests replace it.
var banStep = func(p Player, online bool, minutes int, reason, logAs string) banResult {
	return banByGUID(p, online, minutes, reason, logAs)
}

// execFlowSteps runs a list of steps. It returns false when the run must end: Stop
// was pressed during a wait, or a Stop flow step ran.
func (x *job) execFlowSteps(list []Step) bool {
	for i := range list {
		s := &list[i]
		if x.ctx.Err() != nil {
			return false
		}
		switch s.Type {
		case "message":
			x.sendChatMessage(s.To, x.fillPlaceholders(s.Text))
		case "playerList":
			x.sendChatMessage(s.To, listOnlineNames())
		case "wait":
			secs, _ := parseDur(s.Time)
			if !sleepUntil(x.ctx, time.Now().Add(time.Duration(secs)*time.Second)) {
				return false
			}
		case "lock", "unlock", "shutdown":
			sendCommand(x.run, "#"+s.Type)
		case "kickAll":
			sendCommand(x.run, strings.TrimSpace("kickall "+x.fillPlaceholders(s.Reason)))
		case "command":
			sendCommand(x.run, x.fillPlaceholders(s.Cmd))
		case "kick":
			id, ok := x.currentID()
			if !ok {
				x.run.logf("%s has left; kick skipped", bareName(x.p.Name))
				continue
			}
			reason := x.fillPlaceholders(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			sendCommand(x.run, fmt.Sprintf("kick %d %s", id, reason))
		case "ban":
			minutes, _ := strconv.Atoi(s.Len)
			reason := x.fillPlaceholders(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			p := x.p.Player
			id, online := x.currentID()
			p.ID = id
			res := banStep(p, online, minutes, reason, x.run.name)
			if !res.OK {
				x.run.logf("ban of %s failed: %s", bareName(p.Name), res.Error)
				x.run.markRunFailed()
			} else {
				x.run.logf("banned %s (%s)", bareName(p.Name), banLength(minutes))
			}
		case "stop":
			x.run.logf("Stop flow step: the run ends here")
			return false
		case "forEach":
			if !x.runForEachStep(s.Of, s.Steps) {
				return false
			}
		case "if":
			ran := false
			for k, b := range s.Branches {
				v, _ := x.ifSubjectValue(b.Subject)
				ok := evaluateIfCondition(b, v)
				word := "If "
				if k > 0 {
					word = "Else if "
				}
				if !x.inLoop {
					x.run.logf("%s%s: %s", word, describeIfCondition(b), map[bool]string{true: "yes", false: "no"}[ok])
				} else if ok {
					x.run.logf("%s (#%d): %s%s", bareName(x.p.Name), x.p.ID, word, describeIfCondition(b))
				}
				if ok {
					if !x.execFlowSteps(b.Steps) {
						return false
					}
					ran = true
					break
				}
			}
			if !ran && s.Else != nil {
				if x.inLoop {
					x.run.logf("%s (#%d): Else", bareName(x.p.Name), x.p.ID)
				} else {
					x.run.logf("Else")
				}
				if !x.execFlowSteps(*s.Else) {
					return false
				}
			}
		}
	}
	return true
}

// forEachMarks is, per flow, the Seq of the last server event its previous
// run's For each steps looked at, so a run only considers chat sent since
// the flow's last run. In memory: a restart starts again from the lines of
// the sessions in progress.
var forEachMarks = struct {
	sync.Mutex
	byFlow map[int64]int64
}{byFlow: map[int64]int64{}}

// chatWindowOf returns the chat lines (by server event Seq, (from, to])
// that this run's For each steps consider: everything since the flow's last
// run. The first call in a run fixes the window and moves the flow's mark,
// so every For each in the run sees the same lines.
func (r *flowRun) chatWindowOf() (from, to int64) {
	forEachMarks.Lock()
	defer forEachMarks.Unlock()
	if !r.chatWindow.set {
		r.chatWindow.from = forEachMarks.byFlow[r.flowID]
		r.chatWindow.to = serverEvents.lastSeq()
		r.chatWindow.set = true
		forEachMarks.byFlow[r.flowID] = r.chatWindow.to
	}
	return r.chatWindow.from, r.chatWindow.to
}

// runForEachStep runs a For each's steps, in order: once for every player online
// (of "player", by player number), or once for every chat line players
// online sent this session that this flow hasn't handled yet (of "message",
// oldest first). Each run has that player as "the player"; players BattlEye
// hasn't verified (no GUID) are left out. It returns false when the run must
// end (Stop pressed, or a Stop flow step ran).
func (x *job) runForEachStep(of string, steps []Step) bool {
	type item struct {
		p   Player
		msg string
	}
	var items []item
	from, to := x.run.chatWindowOf()
	x.run.polled.Do(func() { pollPlayersNow() })
	if of == forEachMessage {
		senders := map[string]bool{}
		for _, l := range sessionChatsAfter(from) {
			if l.chat.Seq > to {
				continue // sent after this run started looking
			}
			p, ok := onlineByGUID(l.guid)
			if !ok {
				continue // left since the line was sent
			}
			items = append(items, item{p, l.chat.Text})
			senders[l.guid] = true
		}
		x.run.logf("For each message: %d new from %d player(s)", len(items), len(senders))
	} else {
		for _, p := range onlineNow() { // by player number
			if p.GUID == "" {
				continue
			}
			msg := "" // only what they wrote since the flow's last run
			if c, ok := latestChatOf(p.GUID); ok && c.Seq > from && c.Seq <= to {
				msg = c.Text
			}
			items = append(items, item{p, msg})
		}
		x.run.logf("For each player: %d online", len(items))
	}
	for _, it := range items {
		if x.ctx.Err() != nil {
			return false
		}
		each := &job{run: x.run, ctx: x.ctx, inLoop: true,
			p: &flowPlayer{Player: it.p, role: lookupPlayerRole(it.p.GUID), message: it.msg}}
		if !each.execFlowSteps(steps) {
			return false
		}
	}
	return true
}

// banLength is "permanently" or "for 1h", for log lines.
func banLength(minutes int) string {
	if minutes <= 0 {
		return "permanent"
	}
	return "for " + fmtDur(minutes*60)
}

// runFlowCommand sends one flow command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
//
// An RCON error, or no reply (except to #shutdown), marks the run as failed;
// the run carries on, and ends with status "error".
func runFlowCommand(run *flowRun, cmd string) {
	if cmd == "" {
		return
	}
	lower := strings.ToLower(cmd)

	// kickall [reason] is this program's own command, not a BattlEye one: it
	// lists the players and kicks each (kickAllPlayers).
	if bare := strings.TrimPrefix(lower, "#"); bare == "kickall" || strings.HasPrefix(bare, "kickall ") {
		reason := strings.TrimSpace(strings.TrimPrefix(cmd, "#")[len("kickall"):])
		rep, err := kickAllWeb(reason, flowSource(run.name))
		if err != nil {
			run.logf("kickall failed: %v", err)
			run.markRunFailed()
			return
		}
		run.addToRunLog(rep.summary(reason))
		return
	}

	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := runWebCommand(cmd, 2*time.Second, retry, flowSource(run.name))
	run.addToRunLog(fmt.Sprintf("sent %q (acked=%v err=%v)", cmd, acked, err))
	if err != nil || (!acked && lower != "#shutdown") {
		run.markRunFailed()
	}
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
// Flow definitions (stored in the flows table, see db.go)
// ---------------------------------------------------------------------------

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
	// ScheduleEnabled turns the trigger on (schedule, join and chat flows).
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	NextRun         string `json:"nextRun,omitempty"` // for the page, not stored
	LastRun         string `json:"lastRun,omitempty"` // for the page, not stored
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
	return checkFlowSteps(f.Trigger, f.Steps, f.OnStop)
}

// initStoredFlows creates the flows table (empty on a new database) and logs any
// saved flow that can't run.
func initStoredFlows() error {
	if err := globalDatabase.EnsureFlowsTable(); err != nil {
		return err
	}
	flows, err := globalDatabase.ListStoredFlows()
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

// flowsWithNextRun lists every flow and fills NextRun and LastRun for the page.
func flowsWithNextRun() ([]Flow, error) {
	flows, err := globalDatabase.ListStoredFlows()
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

// validateAndSaveFlow checks a flow from the page (or an import) and stores
// it: a new flow when isNew, else an update of flow f.ID. Every error it
// returns is a user-facing message.
//
// New flows are saved disabled and manual flows are never enabled. A saved
// flow's trigger can't be changed (a restart flow must never turn into one
// that fires on every join), and saving an edit keeps it enabled or disabled
// as it was.
func validateAndSaveFlow(f Flow, isNew bool) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	f.Delay = strings.ToLower(strings.TrimSpace(f.Delay))
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if utf8.RuneCountInString(f.Name) > maxFlowNameLen {
		return 0, fmt.Errorf("Name: %d characters max", maxFlowNameLen)
	}
	trig, err := normalizeTriggerName(f.Trigger)
	if err != nil {
		return 0, err
	}
	f.Trigger = trig
	var old Flow
	if !isNew {
		if old, err = globalDatabase.GetStoredFlow(f.ID); err != nil {
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
	id, err := globalDatabase.SaveStoredFlow(f, isNew, stepsJSON(f.Steps), stepsJSON(f.OnStop))
	if err != nil {
		return 0, errors.New("could not save (is the name already used?)")
	}
	wakeFlowScheduler() // flows_schedule.go
	return id, nil
}

// checkFlowCanEnable reports why a flow can't be enabled, if it can't.
func checkFlowCanEnable(f Flow) error {
	if f.Trigger == triggerManual {
		return errors.New("manual flows have nothing to enable - press Run to start one, or edit the flow and pick another trigger")
	}
	return checkFlowDefinition(f)
}

// previewFlowSteps is the edit dialog's line under the trigger: what the
// schedule means and when the next runs start.
func previewFlowSteps(f Flow) map[string]any {
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

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

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
	// polled makes the run's first For each poll `players` once, for fresh
	// pings and anyone the console log missed (pollPlayersNow, rcon.go).
	polled   sync.Once
	stopOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
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
// commands, which runWebCommand already logs to the container.
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

// sendOnStop sends the When stopped steps, once per run.
func (r *flowRun) sendOnStop() {
	r.stopOnce.Do(func() {
		if len(r.onStop) == 0 {
			return
		}
		r.logf("sending the When stopped steps")
		(&job{run: r, ctx: context.Background()}).execFlowSteps(r.onStop)
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

func snapshotFlowRuns() []runView {
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

// deleteFlowAndRuns forgets a flow's last run and removes it from the database.
func deleteFlowAndRuns(id int64) error {
	flowRuns.Lock()
	if run := flowRuns.byFlow[id]; run != nil && run.active {
		flowRuns.Unlock()
		return errFlowBusy
	}
	delete(flowRuns.byFlow, id)
	flowRuns.Unlock()
	forEachMarks.Lock()
	delete(forEachMarks.byFlow, id) // a new flow reusing the number starts afresh
	forEachMarks.Unlock()
	err := globalDatabase.DeleteStoredFlow(id)
	wakeFlowScheduler()
	return err
}

// launchFlowRun starts a run of f whose first step goes at `start`. p is the player
// for join and chat flows (nil otherwise); a player run adds a job to the
// flow's active run if there is one, while any other start is refused with
// errFlowRunning.
func launchFlowRun(f Flow, start time.Time, source string, p *flowPlayer) error {
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
			onStop: f.OnStop, ctx: ctx, cancel: cancel}
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
		who = fmt.Sprintf(" for %s (#%d)", bareName(p.Name), p.ID)
		if p.message != "" {
			run.logf("%s: %s", bareName(p.Name), p.message)
		} else {
			run.logf("player %d (%s) joined", p.ID, bareName(p.Name))
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

// ---------------------------------------------------------------------------
// Starting player flows (join and message triggers)
// ---------------------------------------------------------------------------

// lookupPlayerRole reads a player's role (Player when unknown).
func lookupPlayerRole(guid string) int {
	if globalDatabase == nil {
		return rolePlayer
	}
	return globalDatabase.GetPlayerRole(guid)
}

// startPlayerFlows runs every enabled flow with the given trigger once for
// the player, after each flow's delay.
func startPlayerFlows(trigger, source string, p *flowPlayer) {
	flows, err := globalDatabase.ListStoredFlows()
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

// ---------------------------------------------------------------------------
// Chat commands
// ---------------------------------------------------------------------------

// chatCommandDef is one built-in chat command as the settings dialog shows
// it: its default cooldown, whether it has Announce and a vote length, its
// help text, and the messages it sends (each with its default wording and
// the placeholders it may use).
type chatCommandDef struct {
	Cmd      string      `json:"cmd"`
	Staff    bool        `json:"staff"`    // mods and admins only
	Cool     string      `json:"cool"`     // default cooldown
	Announce bool        `json:"announce"` // has the Announce setting
	Length   bool        `json:"length"`   // has the vote length setting
	Help     string      `json:"help"`     // what !help !<cmd> sends
	Fields   []chatField `json:"fields"`
}

type chatField struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Def   string   `json:"def"`
	Ph    []string `json:"ph"` // placeholders it may use
}

var chatCommandDefs = []chatCommandDef{
	{Cmd: "help", Cool: "0", Help: "!help lists the commands. !help !<command> explains one.",
		Fields: []chatField{{"list", "!help reply (to the player)", "Commands: {commands}. Type !help !<command> for details.", []string{"commands"}}}},
	{Cmd: "shout", Cool: "10s", Help: "!shout <message> sends your message to everyone on the server.",
		Fields: []chatField{{"msg", "Message (to everyone)", "({player_name}): {message}", []string{"player_name", "message"}}}},
	{Cmd: "whisper", Cool: "0", Help: "!whisper <player number> <message> sends a private message to that player.",
		Fields: []chatField{{"msg", "Message (to the player number they typed)", "({player_name}): {message}", []string{"player_name", "message"}}}},
	{Cmd: "players", Cool: "10s", Help: "!players lists everyone online with their player number, for !whisper.",
		Fields: []chatField{{"entry", "Each player in the list (5 per line, a line every 3 seconds, to the player)", "{target_name}:{target_id}", []string{"target_name", "target_id"}}}},
	{Cmd: "vote", Cool: "10m", Length: true, Help: "!vote <question> starts a server-wide vote on the topic you choose. Everyone answers with !vote y or !vote n.",
		Fields: []chatField{
			{"start", "Vote question (to everyone)", "({player_name}) {topic}? Yes or No? Type !vote y or !vote n", []string{"player_name", "topic"}},
			{"result", "Result (to everyone, when the vote ends)", "The server has spoken and the result is {vote_result}!", []string{"vote_result", "yes", "no", "topic"}},
		}},
	{Cmd: "code", Cool: "0", Help: "!code sends your player code to the admins and mods online, so they can find you.",
		Fields: []chatField{{"staff", "Message (to each mod and admin online)", "Code for {player_name} (#{player_id}): {code}", []string{"player_name", "player_id", "code"}}}},
	{Cmd: "kick", Staff: true, Cool: "0", Announce: true, Help: "!kick <code> <reason> kicks the player with that code. Ask them to type !code first.",
		Fields: []chatField{
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} has been kicked by {player_name}: {reason}", []string{"target_name", "player_name", "reason"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been kicked", []string{"target_name", "reason"}},
		}},
	{Cmd: "ban", Staff: true, Cool: "0", Announce: true, Help: "!ban <code> <reason> permanently bans the player with that code (the last 6 characters of their GUID). Ask them to type !code first.",
		Fields: []chatField{
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} has been banned by {player_name}: {reason}", []string{"target_name", "player_name", "reason"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been banned", []string{"target_name", "reason"}},
		}},
}

func chatDef(cmd string) *chatCommandDef {
	for i := range chatCommandDefs {
		if chatCommandDefs[i].Cmd == cmd {
			return &chatCommandDefs[i]
		}
	}
	return nil
}

// chatSetting is one command's settings as stored and edited.
type chatSetting struct {
	On       bool              `json:"on"`
	Cool     string            `json:"cool"`               // server-wide cooldown, "0" = none
	Announce bool              `json:"announce,omitempty"` // kick and ban: also tell everyone
	Length   string            `json:"length,omitempty"`   // vote: how long it stays open
	Msgs     map[string]string `json:"msgs"`
}

// chatSettings is every command's settings, by command name.
type chatSettings map[string]chatSetting

// chatSettingsKey is where the settings are stored in the settings table.
const chatSettingsKey = "chat_commands"

func defaultChatSettings() chatSettings {
	out := chatSettings{}
	for _, d := range chatCommandDefs {
		s := chatSetting{On: true, Cool: d.Cool, Announce: d.Announce, Msgs: map[string]string{}}
		if d.Length {
			s.Length = "5m"
		}
		for _, f := range d.Fields {
			s.Msgs[f.Key] = f.Def
		}
		out[d.Cmd] = s
	}
	return out
}

// mergeChatSettings lays saved settings over the defaults, so a command or
// message added later gets its default and unknown keys are dropped.
func mergeChatSettings(saved chatSettings) chatSettings {
	out := defaultChatSettings()
	for _, d := range chatCommandDefs {
		s, ok := saved[d.Cmd]
		if !ok {
			continue
		}
		m := out[d.Cmd]
		m.On, m.Cool = s.On, s.Cool
		if d.Announce {
			m.Announce = s.Announce
		}
		if d.Length && s.Length != "" {
			m.Length = s.Length
		}
		for _, f := range d.Fields {
			if v, ok := s.Msgs[f.Key]; ok {
				m.Msgs[f.Key] = v
			}
		}
		out[d.Cmd] = m
	}
	return out
}

// checkChatSettings validates settings from the dialog. Errors are
// user-facing.
func checkChatSettings(s chatSettings) error {
	for _, d := range chatCommandDefs {
		c := s[d.Cmd]
		if secs, ok := parseDur(c.Cool); !ok || secs > maxTimeSecs {
			return fmt.Errorf("/%s cooldown: enter seconds, or a time such as 1m30s (24h at most)", d.Cmd)
		}
		if d.Length {
			if secs, ok := parseDur(c.Length); !ok || secs < 1 || secs > 3600 {
				return fmt.Errorf("/%s length: enter a time from 1s to 1h, such as 5m", d.Cmd)
			}
		}
		for _, f := range d.Fields {
			v := c.Msgs[f.Key]
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("/%s: %s can't be empty", d.Cmd, f.Label)
			}
			for _, m := range tokenRe.FindAllStringSubmatch(v, -1) {
				ok := false
				for _, p := range f.Ph {
					ok = ok || p == m[1]
				}
				if !ok {
					return fmt.Errorf("/%s: %s can't use %s", d.Cmd, f.Label, m[0])
				}
			}
		}
	}
	return nil
}

// chatState is the commands' settings in memory, the time each command was
// last used (cooldowns are server-wide), and the vote in progress.
var chatState = struct {
	sync.Mutex
	settings chatSettings
	loaded   bool
	lastUsed map[string]time.Time
	vote     *chatVote
}{lastUsed: map[string]time.Time{}}

type chatVote struct {
	topic   string
	yes, no int
	voted   map[string]bool // by GUID: the first answer counts
}

// currentChatSettings returns the settings, reading them from the database
// the first time.
func currentChatSettings() chatSettings {
	chatState.Lock()
	defer chatState.Unlock()
	if !chatState.loaded {
		chatState.settings = loadChatSettings()
		chatState.loaded = true
	}
	return chatState.settings
}

func loadChatSettings() chatSettings {
	saved := chatSettings{}
	if globalDatabase != nil {
		raw, ok, err := globalDatabase.GetStoredSetting(chatSettingsKey)
		if err != nil {
			log.Printf("chat commands: reading settings: %v", err)
		}
		if ok {
			if err := json.Unmarshal([]byte(raw), &saved); err != nil {
				log.Printf("chat commands: stored settings unreadable, using defaults: %v", err)
			}
		}
	}
	return mergeChatSettings(saved)
}

// saveChatSettings checks and stores settings from the dialog.
func saveChatSettings(s chatSettings) (chatSettings, error) {
	s = mergeChatSettings(s)
	for cmd, c := range s {
		c.Cool = strings.ToLower(strings.TrimSpace(c.Cool))
		c.Length = strings.ToLower(strings.TrimSpace(c.Length))
		for k, v := range c.Msgs {
			c.Msgs[k] = cleanReason(v)
		}
		s[cmd] = c
	}
	if err := checkChatSettings(s); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(s)
	if err := globalDatabase.SaveStoredSetting(chatSettingsKey, string(b)); err != nil {
		return nil, err
	}
	chatState.Lock()
	chatState.settings, chatState.loaded = s, true
	chatState.Unlock()
	return s, nil
}

// fillTemplate replaces {placeholders} in a message from settings.
func fillTemplate(tpl string, vals map[string]string) string {
	return cleanReason(tokenRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := vals[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	}))
}

// chatSay sends `say <to> <text>` for a chat command (to -1 is everyone)
// and reports whether the server acknowledged it. A private message goes
// into that player's chat history. Tests replace it.
var chatSay = func(to Player, everyone bool, text string) bool {
	cmd := fmt.Sprintf("say %d %s", to.ID, text)
	if everyone {
		cmd = "say -1 " + text
	}
	_, acked, err := runWebCommand(cmd, 2*time.Second, false, "chat")
	if err == nil && acked && !everyone {
		recordSentMessage(to.GUID, cmd)
	}
	return err == nil && acked
}

// chatKick kicks a player for !kick. Tests replace it.
var chatKick = func(p Player, reason string) bool {
	_, acked, err := runWebCommand(fmt.Sprintf("kick %d %s", p.ID, reason), 2*time.Second, false, "chat")
	return err == nil && acked
}

// playersLinePause is the gap between the lines !players sends.
var playersLinePause = 3 * time.Second

// codeOf is a player's code: the last 6 characters of their GUID, lower case.
func codeOf(guid string) string {
	g := strings.ToLower(guid)
	if len(g) < 6 {
		return g
	}
	return g[len(g)-6:]
}

var codeRe = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

// onlineNow is a copy of the latest player list.
func onlineNow() []Player {
	players, _ := listOnlinePlayers()
	return players
}

// listOnlineStaff is every mod and admin online.
func listOnlineStaff() []Player {
	roles := map[string]int{}
	if globalDatabase != nil {
		if r, err := globalDatabase.AllPlayerRoles(); err == nil {
			roles = r
		}
	}
	var out []Player
	for _, p := range onlineNow() {
		if roles[strings.ToLower(p.GUID)] >= roleMod {
			out = append(out, p)
		}
	}
	return out
}

// coolingDown reports whether a command is still in its server-wide cooldown.
func coolingDown(cmd string, s chatSetting) bool {
	secs, _ := parseDur(s.Cool)
	if secs <= 0 {
		return false
	}
	chatState.Lock()
	defer chatState.Unlock()
	return time.Since(chatState.lastUsed[cmd]) < time.Duration(secs)*time.Second
}

func markUsed(cmd string) {
	chatState.Lock()
	chatState.lastUsed[cmd] = time.Now()
	chatState.Unlock()
}

// handleChatCommand runs a built-in command typed by p (whose role is role).
// Only a command that works gets a reply or does anything: an unknown
// command, one turned off or cooling down, one the player may not use, or
// one missing what it needs is dropped without a word. The chat line itself
// is already in the player's history, so nothing more is logged.
func handleChatCommand(p Player, role int, text string) {
	text = strings.TrimSpace(text)
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "!") {
		return
	}
	cmd := strings.ToLower(fields[0][1:])
	def := chatDef(cmd)
	if def == nil {
		return
	}
	all := currentChatSettings()
	set := all[cmd]
	if !set.On || def.Staff && role < roleMod {
		return
	}
	rest := strings.TrimSpace(text[len(fields[0]):])
	args := fields[1:]
	name := bareName(p.Name)
	if cmd != "vote" && coolingDown(cmd, set) {
		return
	}
	switch cmd {
	case "help":
		if len(args) == 0 {
			var names []string
			for _, d := range chatCommandDefs {
				if all[d.Cmd].On && (!d.Staff || role >= roleMod) {
					names = append(names, "!"+d.Cmd)
				}
			}
			if chatSay(p, false, fillTemplate(set.Msgs["list"], map[string]string{"commands": strings.Join(names, ", ")})) {
				markUsed(cmd)
			}
			return
		}
		d := chatDef(strings.ToLower(strings.TrimPrefix(args[0], "!")))
		if d == nil || !all[d.Cmd].On || d.Staff && role < roleMod {
			return
		}
		if chatSay(p, false, d.Help) {
			markUsed(cmd)
		}

	case "shout":
		if rest == "" {
			return
		}
		if chatSay(p, true, fillTemplate(set.Msgs["msg"], map[string]string{"player_name": name, "message": rest})) {
			markUsed(cmd)
		}

	case "whisper":
		if len(args) < 2 {
			return
		}
		id, err := strconv.Atoi(args[0])
		if err != nil || id == p.ID {
			return
		}
		target, ok := onlineByID(id)
		if !ok {
			return
		}
		msg := strings.TrimSpace(rest[len(args[0]):])
		if chatSay(target, false, fillTemplate(set.Msgs["msg"], map[string]string{"player_name": name, "message": msg})) {
			markUsed(cmd)
		}

	case "players":
		online := onlineNow()
		if len(online) == 0 {
			return
		}
		var lines []string
		for i := 0; i < len(online); i += 5 {
			var parts []string
			for _, o := range online[i:min(i+5, len(online))] {
				parts = append(parts, fillTemplate(set.Msgs["entry"], map[string]string{"target_name": bareName(o.Name), "target_id": strconv.Itoa(o.ID)}))
			}
			lines = append(lines, strings.Join(parts, " | "))
		}
		markUsed(cmd)
		go func() {
			for i, l := range lines {
				if i > 0 {
					time.Sleep(playersLinePause)
				}
				asker, ok := onlineByGUID(p.GUID) // their number now; stop if they left
				if !ok {
					return
				}
				chatSay(asker, false, l)
			}
		}()

	case "vote":
		chatVoteCommand(p, set, rest)

	case "code":
		staff := listOnlineStaff()
		if len(staff) == 0 {
			return
		}
		vals := map[string]string{"player_name": name, "player_id": strconv.Itoa(p.ID), "code": codeOf(p.GUID)}
		sent := false
		for _, s := range staff {
			sent = chatSay(s, false, fillTemplate(set.Msgs["staff"], vals)) || sent
		}
		if sent {
			markUsed(cmd)
		}

	case "kick", "ban":
		runPunishCommand(cmd, p, role, set, args)
	}
}

// chatVoteCommand handles !vote <question> (start a vote) and !vote y|n.
func chatVoteCommand(p Player, set chatSetting, rest string) {
	answer := strings.ToLower(rest)
	if answer == "y" || answer == "yes" || answer == "n" || answer == "no" {
		chatState.Lock()
		defer chatState.Unlock()
		v := chatState.vote
		if v == nil || v.voted[strings.ToLower(p.GUID)] {
			return // no vote running, or they already voted: the first answer counts
		}
		v.voted[strings.ToLower(p.GUID)] = true
		if answer[0] == 'y' {
			v.yes++
		} else {
			v.no++
		}
		return
	}
	topic := strings.TrimSpace(strings.TrimRight(rest, "?"))
	if topic == "" || coolingDown("vote", set) {
		return
	}
	chatState.Lock()
	if chatState.vote != nil {
		chatState.Unlock()
		return // one vote at a time
	}
	v := &chatVote{topic: topic, voted: map[string]bool{}}
	chatState.vote = v
	chatState.lastUsed["vote"] = time.Now()
	chatState.Unlock()

	chatSay(p, true, fillTemplate(set.Msgs["start"], map[string]string{"player_name": bareName(p.Name), "topic": topic}))
	length, _ := parseDur(set.Length)
	go func() {
		time.Sleep(time.Duration(length) * time.Second)
		chatState.Lock()
		chatState.vote = nil
		yes, no := v.yes, v.no
		chatState.Unlock()
		result := "FAILED" // a tie, or no votes at all, fails
		if yes > no {
			result = "PASSED"
		}
		log.Printf("chat: vote %q %s (%d yes, %d no)", v.topic, result, yes, no)
		chatSay(Player{}, true, fillTemplate(set.Msgs["result"], map[string]string{
			"vote_result": result, "yes": strconv.Itoa(yes), "no": strconv.Itoa(no), "topic": v.topic}))
	}()
}

// runPunishCommand handles !kick <code> [reason] and !ban <code> [reason]. Mods
// can kick and ban Players; Admins can also kick and ban Mods. Nobody can
// kick or ban themselves, or anyone of their own rank or higher, from chat.
// !ban is always permanent and also finds players who have left.
func runPunishCommand(cmd string, p Player, role int, set chatSetting, args []string) {
	if len(args) == 0 || !codeRe.MatchString(args[0]) {
		return
	}
	code := strings.ToLower(args[0])
	reason := cleanReason(strings.Join(args[1:], " "))
	if reason == "" {
		reason = "No reason given"
	}
	var matches []Player
	for _, o := range onlineNow() {
		if codeOf(o.GUID) == code {
			matches = append(matches, o)
		}
	}
	online := len(matches) > 0
	if !online && cmd == "ban" && globalDatabase != nil {
		known, err := globalDatabase.PlayersByCode(code)
		if err != nil {
			log.Printf("chat: !ban lookup: %v", err)
			return
		}
		matches = known
	}
	if len(matches) != 1 {
		return // nobody, or more than one player, has that code
	}
	t := matches[0]
	if strings.EqualFold(t.GUID, p.GUID) || lookupPlayerRole(t.GUID) >= role {
		return
	}
	done := "kicked"
	if cmd == "kick" {
		if !chatKick(t, reason) {
			return
		}
	} else {
		done = "banned"
		if res := banStep(t, online, 0, reason, "chat"); !res.OK {
			log.Printf("chat: !ban of %s failed: %s", bareName(t.Name), res.Error)
			return
		}
	}
	markUsed(cmd)
	vals := map[string]string{"target_name": bareName(t.Name), "player_name": bareName(p.Name), "reason": reason}
	for _, s := range listOnlineStaff() {
		if !strings.EqualFold(s.GUID, t.GUID) { // a kicked Mod is on their way out
			chatSay(s, false, fillTemplate(set.Msgs["staff"], vals))
		}
	}
	if set.Announce {
		chatSay(Player{}, true, fillTemplate(set.Msgs["announce"], vals))
	}
	log.Printf("chat: %s %s %s (%s)", bareName(p.Name), done, bareName(t.Name), reason)
}
