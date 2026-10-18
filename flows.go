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
//	schedule  a cron schedule, while Enabled
//	join      once for each player who finishes loading in, while Enabled
//	chat      once for each chat message a player sends, while Enabled
//	manual    only when Run is pressed
//
// and an optional delay before step 1. "The player" is whoever triggered a
// join or chat run; {player_name}, {player_id}, {player_ping}, {player_ip},
// {player_guid}, {player_role} and {message} are filled in from them.
// Schedule and Manual flows have no player, so they message everyone only.
//
// The same file runs the built-in chat commands (/help, /shout, /whisper,
// /players, /vote, /code, /kick, /ban), since they also react to chat.
//
// Sections: times, steps and conditions, validation, the runner, cron, flow
// definitions, run bookkeeping, the scheduler, the join and chat triggers,
// and the chat commands.
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
	Else     *[]Step  `json:"else,omitempty"` // nil: no Else
}

// Branch is the If or one Else if: a condition and the steps it runs. Every
// branch of one If compares the same Subject; only Op and Value differ.
type Branch struct {
	Subject string `json:"subject"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Steps   []Step `json:"steps"`
}

// branchLists is an If's step lists in order: each branch, then the Else.
func branchLists(s *Step) [][]Step {
	out := make([][]Step, 0, len(s.Branches)+1)
	for _, b := range s.Branches {
		out = append(out, b.Steps)
	}
	if s.Else != nil {
		out = append(out, *s.Else)
	}
	return out
}

// fixSteps gives every branch and Else a non-nil list, so the page always
// gets [] rather than null, and trims the text fields.
func fixSteps(list []Step) []Step {
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
			b.Steps = fixSteps(b.Steps)
		}
		if s.Else != nil {
			e := fixSteps(*s.Else)
			s.Else = &e
		}
	}
	return list
}

// countSteps counts every step, inside Ifs too.
func countSteps(list []Step) int {
	n := 0
	for i := range list {
		n++
		if list[i].Type == "if" {
			for _, l := range branchLists(&list[i]) {
				n += countSteps(l)
			}
		}
	}
	return n
}

// runLength is how long the waits take, counting the longest branch of each If.
func runLength(list []Step) int {
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
				if n := runLength(l); n > longest {
					longest = n
				}
			}
			total += longest
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
//	word  the chat message: contains, starts with
//	text  ==, !=, matches (* wildcard), ignoring case
//	num   == != > >= < <=
//	role  like num, with the value picked from Player, Mod, Admin
type condSubject struct{ kind, need string }

var condSubjects = map[string]condSubject{
	"message":     {"word", needChat},
	"player_name": {"text", needPlayer},
	"player_id":   {"num", needPlayer},
	"player_ping": {"num", needPlayer},
	"player_ip":   {"text", needPlayer},
	"player_guid": {"text", needPlayer},
	"player_role": {"role", needPlayer},
	// How many players are online, from the 5-second player check. Works on
	// every trigger: "If {players_online} > 0, Kick everyone, else Shut down".
	"players_online": {"num", needNothing},
}

var condOps = map[string][]string{
	"word": {"has", "starts"},
	"text": {"eq", "ne", "wild"},
	"num":  {"eq", "ne", "gt", "ge", "lt", "le"},
	"role": {"eq", "ne", "gt", "ge", "lt", "le"},
}

var opText = map[string]string{"has": "contains", "starts": "starts with", "eq": "==", "ne": "!=",
	"wild": "matches", "gt": ">", "ge": ">=", "lt": "<", "le": "<="}

// describeCond writes a condition the way the page shows it, for the run log:
// {player_ping} >= 500, {player_name} == "Survivor".
func describeCond(b Branch) string {
	sub := condSubjects[b.Subject]
	val := b.Value
	switch {
	case sub.kind == "role":
		n, _ := strconv.Atoi(b.Value)
		val = roleName(n)
	case sub.kind == "text" && b.Op != "wild":
		val = `"` + b.Value + `"`
	}
	return "{" + b.Subject + "} " + opText[b.Op] + " " + val
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
// (so /discord matches "/discord please" but not "/discordx"). Case is ignored.
func matchWords(op, msg, value string) bool {
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

// evalCond checks one branch's condition against the subject's value.
func evalCond(b Branch, v string) bool {
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
		}
		return wildMatch(b.Value, v)
	}
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
	"stop": true, "if": true}

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

func (c *stepChecker) check(list []Step, depth int) error {
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
			if err := c.checkIf(s, depth); err != nil {
				return err
			}
			continue // the branches checked their own steps
		}
		for _, t := range []string{s.Text, s.Reason, s.Cmd} {
			if err := c.tokensOK(t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *stepChecker) checkIf(s *Step, depth int) error {
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
		switch sub.kind {
		case "num", "role":
			if _, err := strconv.Atoi(strings.TrimSpace(b.Value)); err != nil {
				return fmt.Errorf("%s %d: Enter a whole number", c.where, num)
			}
		case "word":
			if len(terms(b.Value)) == 0 {
				return fmt.Errorf(`%s %d: Enter a word, several words, or a "quoted phrase"`, c.where, num)
			}
		}
	}
	for _, l := range branchLists(s) {
		if err := c.check(l, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// checkSteps validates a flow's steps and its When stopped steps for its
// trigger. Errors name the step as the page numbers them.
func checkSteps(trigger string, steps, onStop []Step) error {
	if len(steps) == 0 {
		return errors.New("add at least one step")
	}
	if countSteps(steps)+countSteps(onStop) > maxSteps {
		return fmt.Errorf("a flow can have %d steps at most", maxSteps)
	}
	c := &stepChecker{has: triggerHas(trigger), where: "Step"}
	if err := c.check(steps, 0); err != nil {
		return err
	}
	if len(onStop) > 0 && trigger != triggerSchedule && trigger != triggerManual {
		return errors.New("only Schedule and Manual flows have When stopped steps")
	}
	s := &stepChecker{has: map[string]bool{}, onStop: true, where: "When stopped, step"}
	return s.check(onStop, 0)
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
	p   *flowPlayer // nil for schedule and manual runs
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

// value is what a placeholder or If subject stands for in this run.
func (x *job) value(name string) (string, bool) {
	if name == "players_online" {
		latest.Lock()
		n := len(latest.players)
		latest.Unlock()
		return strconv.Itoa(n), true
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

// fill replaces the placeholders in a step's text. Unknown {words} are left
// as typed, and what is filled in is not expanded again.
func (x *job) fill(text string) string {
	return cleanReason(tokenRe.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := x.value(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	}))
}

// say sends a message to everyone, or to the player. A private message is
// added to the player's chat history, as the Message dialog does.
func (x *job) say(to, text string) {
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
	recordSent(x.p.GUID, cmd)
}

// recordSent adds a private message ReCON sent to a player's chat history.
func recordSent(guid, cmd string) {
	if globalDatabase == nil || guid == "" {
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), guid, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// onlineNames is the latest player list as "Online (N): a, b, c".
func onlineNames() string {
	latest.Lock()
	names := make([]string, 0, len(latest.players))
	for _, p := range latest.players {
		names = append(names, bareName(p.Name))
	}
	latest.Unlock()
	if len(names) == 0 {
		return "Online (0): nobody"
	}
	return fmt.Sprintf("Online (%d): %s", len(names), strings.Join(names, ", "))
}

// banStep bans the player. Tests replace it.
var banStep = func(p Player, online bool, minutes int, reason, logAs string) banResult {
	return banByGUID(p, online, minutes, reason, logAs)
}

// exec runs a list of steps. It returns false when the run must end: Stop
// was pressed during a wait, or a Stop flow step ran.
func (x *job) exec(list []Step) bool {
	for i := range list {
		s := &list[i]
		if x.ctx.Err() != nil {
			return false
		}
		switch s.Type {
		case "message":
			x.say(s.To, x.fill(s.Text))
		case "playerList":
			x.say(s.To, onlineNames())
		case "wait":
			secs, _ := parseDur(s.Time)
			if !sleepUntil(x.ctx, time.Now().Add(time.Duration(secs)*time.Second)) {
				return false
			}
		case "lock", "unlock", "shutdown":
			sendCommand(x.run, "#"+s.Type)
		case "kickAll":
			sendCommand(x.run, strings.TrimSpace("kickall "+x.fill(s.Reason)))
		case "command":
			sendCommand(x.run, x.fill(s.Cmd))
		case "kick":
			id, ok := x.currentID()
			if !ok {
				x.run.logf("%s has left; kick skipped", bareName(x.p.Name))
				continue
			}
			reason := x.fill(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			sendCommand(x.run, fmt.Sprintf("kick %d %s", id, reason))
		case "ban":
			minutes, _ := strconv.Atoi(s.Len)
			reason := x.fill(s.Reason)
			if reason == "" {
				reason = "No reason specified"
			}
			p := x.p.Player
			id, online := x.currentID()
			p.ID = id
			res := banStep(p, online, minutes, reason, x.run.name)
			if !res.OK {
				x.run.logf("ban of %s failed: %s", bareName(p.Name), res.Error)
				x.run.markFailed()
			} else {
				x.run.logf("banned %s (%s)", bareName(p.Name), banLength(minutes))
			}
		case "stop":
			x.run.logf("Stop flow step: the run ends here")
			return false
		case "if":
			ran := false
			for k, b := range s.Branches {
				v, _ := x.value(b.Subject)
				ok := evalCond(b, v)
				word := "If "
				if k > 0 {
					word = "Else if "
				}
				x.run.logf("%s%s: %s", word, describeCond(b), map[bool]string{true: "yes", false: "no"}[ok])
				if ok {
					if !x.exec(b.Steps) {
						return false
					}
					ran = true
					break
				}
			}
			if !ran && s.Else != nil {
				x.run.logf("Else")
				if !x.exec(*s.Else) {
					return false
				}
			}
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
	// lists the players and kicks each (kickAll).
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
	ScheduleEnabled bool `json:"scheduleEnabled"`
	// Broken says why a stored flow couldn't be read (one written for the
	// old script format). It is never stored.
	Broken  string `json:"broken,omitempty"`
	NextRun string `json:"nextRun,omitempty"` // for the page, not stored
	LastRun string `json:"lastRun,omitempty"` // for the page, not stored
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
// "* * * * *" count as manual.
func triggerOf(schedule string) string {
	switch strings.ToLower(strings.Join(strings.Fields(schedule), " ")) {
	case scheduleManual, "", "* * * * *":
		return triggerManual
	case scheduleJoin:
		return triggerJoin
	case scheduleChat:
		return triggerChat
	}
	return triggerSchedule
}

// storedSchedule is what goes in the schedule column.
func storedSchedule(f Flow) string {
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
// schedule column and its steps from JSON. A flow saved in the old script
// format can't be read; Broken says so and it doesn't run.
func flowFromRow(f *Flow, script, cancel string) {
	f.Trigger = triggerOf(f.Schedule)
	if f.Trigger != triggerSchedule {
		f.Schedule = ""
	}
	if f.Trigger == triggerManual {
		f.ScheduleEnabled = false
	}
	f.Steps, f.OnStop = []Step{}, []Step{}
	if err := json.Unmarshal([]byte(script), &f.Steps); err != nil {
		f.Steps = []Step{}
		f.Broken = "This flow was written for the old script format. Delete it and build it again with steps."
		return
	}
	if strings.TrimSpace(cancel) != "" {
		if err := json.Unmarshal([]byte(cancel), &f.OnStop); err != nil {
			f.OnStop = []Step{}
		}
	}
	f.Steps, f.OnStop = fixSteps(f.Steps), fixSteps(f.OnStop)
}

// stepsJSON is how steps are stored.
func stepsJSON(list []Step) string {
	b, _ := json.Marshal(fixSteps(list))
	return string(b)
}

// normalizeTrigger checks the trigger the page sent. Empty means manual.
func normalizeTrigger(t string) (string, error) {
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
	if triggerOf(schedule) != triggerSchedule {
		return errors.New("Schedule: " + cronExamples)
	}
	if _, err := parseCron(schedule); err != nil {
		return errors.New("Schedule: " + err.Error() + ".\n" + cronExamples)
	}
	return nil
}

// delayOf reads a flow's delay in seconds. Errors are user-facing.
func delayOf(f Flow) (int, error) {
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

// checkFlow runs every check a flow must pass to be saved or run.
func checkFlow(f Flow) error {
	if f.Broken != "" {
		return errors.New(f.Broken)
	}
	if f.Trigger == triggerSchedule {
		if err := checkCronSchedule(f.Schedule); err != nil {
			return err
		}
	}
	if _, err := delayOf(f); err != nil {
		return err
	}
	return checkSteps(f.Trigger, f.Steps, f.OnStop)
}

// initFlows creates the flows table (empty on a new database) and logs any
// saved flow that can't run.
func initFlows() error {
	if err := globalDatabase.EnsureFlowsTable(); err != nil {
		return err
	}
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		return err
	}
	for _, f := range flows {
		if err := checkFlow(f); err != nil {
			log.Printf("flow[%s] won't run until it is fixed: %v", f.Name, err)
		}
	}
	return nil
}

// flowsWithNextRun lists every flow and fills NextRun and LastRun for the page.
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
		case f.Trigger == triggerChat:
			f.NextRun = "On player message"
		default:
			if c, err := parseCron(f.Schedule); err == nil {
				if next, ok := c.next(now); ok {
					delay, _ := delayOf(*f)
					f.NextRun = fmtWhen(next.Add(time.Duration(delay) * time.Second))
				}
			}
		}
	}
	return flows, nil
}

// validateAndSaveFlow checks a flow from the page (or an import) and stores
// it. Every error it returns is a user-facing message.
//
// New flows are saved disabled, manual flows are never enabled, and an edit
// that changes a flow's trigger saves it disabled, so switching an enabled
// restart flow to On player join can't make it fire without pressing Enable.
func validateAndSaveFlow(f Flow) (int64, error) {
	f.Name = strings.TrimSpace(f.Name)
	f.Schedule = strings.TrimSpace(f.Schedule)
	f.Delay = strings.ToLower(strings.TrimSpace(f.Delay))
	f.Broken = ""
	if f.Name == "" {
		return 0, errors.New("name is required")
	}
	if utf8.RuneCountInString(f.Name) > maxFlowNameLen {
		return 0, fmt.Errorf("Name: %d characters max", maxFlowNameLen)
	}
	trig, err := normalizeTrigger(f.Trigger)
	if err != nil {
		return 0, err
	}
	f.Trigger = trig
	f.Steps, f.OnStop = fixSteps(f.Steps), fixSteps(f.OnStop)
	if f.Trigger != triggerSchedule && f.Trigger != triggerManual {
		f.OnStop = []Step{}
	}
	if err := checkFlow(f); err != nil {
		return 0, err
	}
	f.Schedule = storedSchedule(f)
	if f.Trigger == triggerManual || f.ID == 0 {
		f.ScheduleEnabled = false
	} else if old, err := globalDatabase.GetFlow(f.ID); err == nil && old.Trigger != f.Trigger {
		f.ScheduleEnabled = false
	}
	id, err := globalDatabase.SaveFlow(f, stepsJSON(f.Steps), stepsJSON(f.OnStop))
	if err != nil {
		return 0, errors.New("could not save (is the name already used?)")
	}
	return id, nil
}

// checkFlowCanEnable reports why a flow can't be enabled, if it can't.
func checkFlowCanEnable(f Flow) error {
	if f.Trigger == triggerManual {
		return errors.New("manual flows have nothing to enable - press Run to start one, or edit the flow and pick another trigger")
	}
	return checkFlow(f)
}

// previewFlow is the edit dialog's line under the trigger: what the
// schedule means and when the next runs start.
func previewFlow(f Flow) map[string]any {
	trig, err := normalizeTrigger(f.Trigger)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	f.Trigger = trig
	delay, err := delayOf(f)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if trig != triggerSchedule {
		return map[string]any{"ok": true}
	}
	if err := checkCronSchedule(f.Schedule); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	c, _ := parseCron(f.Schedule)
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
	flowID   int64
	name     string
	status   string // running | done | stopped | error
	active   bool
	failed   bool      // a command failed; the run ends as "error"
	stopped  bool      // the Stop button was pressed
	anchor   time.Time // when step 1 starts (after the delay)
	lastRun  time.Time // when the flow was last started
	jobs     int       // jobs still working through the steps
	log      []string
	onStop   []Step // sent once if Stop is pressed
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
// commands, which runShared already logs to the container as rcon[name].
func (r *flowRun) addToRunLog(msg string) {
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	if len(r.log) > maxRunLog {
		r.log = append([]string(nil), r.log[len(r.log)-maxRunLog:]...)
	}
	flowRuns.Unlock()
}

func (r *flowRun) markFailed() {
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
		(&job{run: r, ctx: context.Background()}).exec(r.onStop)
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

// startFlow starts a run now (the Run button). minutes > 0 replaces the
// flow's delay for this run. Join and chat flows run for a player, so Run
// can't start them.
func startFlow(f Flow, minutes int) error {
	if err := checkFlow(f); err != nil {
		return err
	}
	switch f.Trigger {
	case triggerJoin:
		return errors.New("this flow runs for each player who joins, so Run can't start it")
	case triggerChat:
		return errors.New("this flow runs for each player message, so Run can't start it")
	}
	delay, _ := delayOf(f)
	if minutes > 0 {
		delay = minutes * 60
	}
	return launch(f, time.Now().Add(time.Duration(delay)*time.Second), sourceManual, nil)
}

// stopFlow cancels the active run of a flow (the Stop button). The run sends
// the When stopped steps as it stops.
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

// launch starts a run of f whose first step goes at `start`. p is the player
// for join and chat flows (nil otherwise); a player run adds a job to the
// flow's active run if there is one, while any other start is refused with
// errFlowRunning.
func launch(f Flow, start time.Time, source string, p *flowPlayer) error {
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
	go func() { run.jobDone(runJob(run, f, start, p)) }()
	return nil
}

// runJob works through one job and reports whether it was stopped.
func runJob(run *flowRun, f Flow, start time.Time, p *flowPlayer) (stopped bool) {
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
	if !x.exec(f.Steps) && run.ctx.Err() != nil {
		run.logf("stopped%s", who)
		run.sendOnStop()
		return true
	}
	run.logf("done%s", who)
	return false
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// scheduleGrace lets a run whose start just passed (a late tick, a restart a
// few seconds in) still start instead of being skipped.
const scheduleGrace = 15 * time.Second

// scheduleTick is how often the scheduler checks for runs to start.
const scheduleTick = 5 * time.Second

// lastFired is the latest cron time started for each flow, so a finished or
// stopped run isn't started again for the same time. Only the scheduler
// goroutine touches it.
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
// time that hasn't been started yet and whose first step (cron time plus the
// delay) is now or before the next tick. A run more than scheduleGrace late
// is skipped.
func runScheduleTick(now time.Time) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != triggerSchedule || checkFlow(f) != nil {
			continue
		}
		c, err := parseCron(f.Schedule)
		if err != nil {
			continue
		}
		delay, _ := delayOf(f)
		d := time.Duration(delay) * time.Second
		from := now.Add(-scheduleGrace - d)
		if lf, ok := lastFired[f.ID]; ok && lf.After(from) {
			from = lf
		}
		t, ok := c.next(from)
		if !ok || !launchDue(now, t.Add(d)) {
			continue
		}
		if err := launch(f, t.Add(d), sourceSchedule, nil); errors.Is(err, errFlowRunning) {
			continue // busy: retry next tick
		} else if err != nil {
			log.Printf("scheduler: %s: %v", f.Name, err)
		}
		lastFired[f.ID] = t
	}
}

// ---------------------------------------------------------------------------
// Player-join trigger, and the latest player list
// ---------------------------------------------------------------------------

// latest is the poller's most recent player list. Flows and chat commands
// read it, so they don't send their own `players` command.
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
// result. It keeps the latest list and starts the join flows for new
// arrivals. The first call after startup only records who is online, so a
// restart doesn't greet everybody.
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
// forgotten, so players are greeted again when they reconnect.
func playersLost() {
	joinWatch.seen = map[string]bool{}
	latest.Lock()
	latest.players = nil
	latest.Unlock()
}

// playerRole reads a player's role (Player when unknown).
func playerRole(guid string) int {
	if globalDatabase == nil {
		return rolePlayer
	}
	return globalDatabase.PlayerRole(guid)
}

// startPlayerFlows runs every enabled flow with the given trigger once for
// the player, after each flow's delay.
func startPlayerFlows(trigger, source string, p *flowPlayer) {
	flows, err := globalDatabase.ListFlows()
	if err != nil {
		log.Printf("%s flows: %v", trigger, err)
		return
	}
	for _, f := range flows {
		if !f.ScheduleEnabled || f.Trigger != trigger || checkFlow(f) != nil {
			continue
		}
		delay, _ := delayOf(f)
		pp := *p
		launch(f, time.Now().Add(time.Duration(delay)*time.Second), source, &pp)
	}
}

// startJoinFlows runs the join flows for each player who just joined.
func startJoinFlows(joined []Player) {
	for _, p := range joined {
		startPlayerFlows(triggerJoin, sourceJoin, &flowPlayer{Player: p, role: playerRole(p.GUID)})
	}
}

// ---------------------------------------------------------------------------
// Chat: Player message flows and the built-in chat commands
// ---------------------------------------------------------------------------

// startChatWatcher reacts to every chat line for as long as ReCON runs:
// built-in chat commands first, then the Player message flows.
//
// The sender is found by exact name among the players online (without the
// "(Lobby)" tag). If nobody online has that name, or more than one player
// does, the line is ignored; what they may do comes from their GUID's role.
func startChatWatcher() {
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind == eventChat {
				handleChatLine(ev.Name, ev.Message)
			}
		}
	}()
}

func handleChatLine(name, message string) {
	p, ok := onlineByUniqueName(name)
	if !ok || p.GUID == "" {
		return
	}
	role := playerRole(p.GUID)
	handleChatCommand(p, role, message)
	startPlayerFlows(triggerChat, sourceChat, &flowPlayer{Player: p, role: role, message: strings.TrimSpace(message)})
}

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
	Help     string      `json:"help"`     // what /help /<cmd> sends
	Fields   []chatField `json:"fields"`
}

type chatField struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Def   string   `json:"def"`
	Ph    []string `json:"ph"` // placeholders it may use
}

var chatCommandDefs = []chatCommandDef{
	{Cmd: "help", Cool: "0", Help: "/help lists the commands. /help /<command> explains one.",
		Fields: []chatField{{"list", "/help reply (to the player)", "Commands: {commands}. Type /help /<command> for details.", []string{"commands"}}}},
	{Cmd: "shout", Cool: "10s", Help: "/shout <message> sends your message to everyone on the server.",
		Fields: []chatField{{"msg", "Message (to everyone)", "({player_name}): {message}", []string{"player_name", "message"}}}},
	{Cmd: "whisper", Cool: "0", Help: "/whisper <player number> <message> sends a private message to that player.",
		Fields: []chatField{{"msg", "Message (to the player number they typed)", "({player_name}): {message}", []string{"player_name", "message"}}}},
	{Cmd: "players", Cool: "10s", Help: "/players lists everyone online with their player number, for /whisper.",
		Fields: []chatField{{"entry", "Each player in the list (5 per line, a line every 3 seconds, to the player)", "{target_name}:{target_id}", []string{"target_name", "target_id"}}}},
	{Cmd: "vote", Cool: "10m", Length: true, Help: "/vote <question> starts a server-wide vote on the topic you choose. Everyone answers with /vote y or /vote n.",
		Fields: []chatField{
			{"start", "Vote question (to everyone)", "({player_name}) {topic}? Yes or No? Type /vote y or /vote n", []string{"player_name", "topic"}},
			{"result", "Result (to everyone, when the vote ends)", "The server has spoken and the result is {vote_result}!", []string{"vote_result", "yes", "no", "topic"}},
		}},
	{Cmd: "code", Cool: "0", Help: "/code sends your player code to the admins and mods online, so they can find you.",
		Fields: []chatField{{"staff", "Message (to each mod and admin online)", "Code for {player_name} (#{player_id}): {code}", []string{"player_name", "player_id", "code"}}}},
	{Cmd: "kick", Staff: true, Cool: "0", Announce: true, Help: "/kick <code> <reason> kicks the player with that code. Ask them to type /code first.",
		Fields: []chatField{
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} has been kicked by {player_name}: {reason}", []string{"target_name", "player_name", "reason"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been kicked", []string{"target_name", "reason"}},
		}},
	{Cmd: "ban", Staff: true, Cool: "0", Announce: true, Help: "/ban <code> <reason> permanently bans the player with that code (the last 6 characters of their GUID). Ask them to type /code first.",
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
		raw, ok, err := globalDatabase.Setting(chatSettingsKey)
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
	if err := globalDatabase.SaveSetting(chatSettingsKey, string(b)); err != nil {
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
	_, acked, err := runShared(cmd, 2*time.Second, false, "chat")
	if err == nil && acked && !everyone {
		recordSent(to.GUID, cmd)
	}
	return err == nil && acked
}

// chatKick kicks a player for /kick. Tests replace it.
var chatKick = func(p Player, reason string) bool {
	_, acked, err := runShared(fmt.Sprintf("kick %d %s", p.ID, reason), 2*time.Second, false, "chat")
	return err == nil && acked
}

// playersLinePause is the gap between the lines /players sends.
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
	latest.Lock()
	defer latest.Unlock()
	return append([]Player(nil), latest.players...)
}

// onlineStaff is every mod and admin online.
func onlineStaff() []Player {
	roles := map[string]int{}
	if globalDatabase != nil {
		if r, err := globalDatabase.Roles(); err == nil {
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
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
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
					names = append(names, "/"+d.Cmd)
				}
			}
			if chatSay(p, false, fillTemplate(set.Msgs["list"], map[string]string{"commands": strings.Join(names, ", ")})) {
				markUsed(cmd)
			}
			return
		}
		d := chatDef(strings.ToLower(strings.TrimPrefix(args[0], "/")))
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
		staff := onlineStaff()
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
		chatPunish(cmd, p, role, set, args)
	}
}

// chatVoteCommand handles /vote <question> (start a vote) and /vote y|n.
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

// chatPunish handles /kick <code> [reason] and /ban <code> [reason]. Mods
// can kick and ban Players; Admins can also kick and ban Mods. Nobody can
// kick or ban themselves, or anyone of their own rank or higher, from chat.
// /ban is always permanent and also finds players who have left.
func chatPunish(cmd string, p Player, role int, set chatSetting, args []string) {
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
			log.Printf("chat: /ban lookup: %v", err)
			return
		}
		matches = known
	}
	if len(matches) != 1 {
		return // nobody, or more than one player, has that code
	}
	t := matches[0]
	if strings.EqualFold(t.GUID, p.GUID) || playerRole(t.GUID) >= role {
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
			log.Printf("chat: /ban of %s failed: %s", bareName(t.Name), res.Error)
			return
		}
	}
	markUsed(cmd)
	vals := map[string]string{"target_name": bareName(t.Name), "player_name": bareName(p.Name), "reason": reason}
	for _, s := range onlineStaff() {
		if !strings.EqualFold(s.GUID, t.GUID) { // a kicked Mod is on their way out
			chatSay(s, false, fillTemplate(set.Msgs["staff"], vals))
		}
	}
	if set.Announce {
		chatSay(Player{}, true, fillTemplate(set.Msgs["announce"], vals))
	}
	log.Printf("chat: %s %s %s (%s)", bareName(p.Name), done, bareName(t.Name), reason)
}
