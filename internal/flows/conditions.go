package flows

import (
	"regexp"
	"strconv"
	"strings"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"
)

// Roles, by number: what is stored in players.role.
const (
	RolePlayer = 0
	roleMod    = 1
	RoleAdmin  = 2
)

var roleNames = []string{"Player", "Mod", "Admin"}

func RoleName(r int) string {
	if r < 0 || r >= len(roleNames) {
		return roleNames[0]
	}
	return roleNames[r]
}

// What a placeholder or If subject needs: nothing (any trigger), "player"
// (On join or Player message) or "chat" (Player message only), or, for
// {threshold}, a flow with a Counter threshold.
const (
	needNothing   = ""
	needPlayer    = "player"
	needChat      = "chat"
	needThreshold = "threshold"
)

var needText = map[string]string{
	needPlayer:    "needs a player: put it inside a For each, or use the On join or Player message trigger",
	needChat:      "needs the Player message trigger",
	needThreshold: "needs a Counter threshold: enter one (1 or more) next to the delay",
}

// flowTokens are the placeholders steps can use, and what each needs.
var flowTokens = map[string]string{
	"player_name": needPlayer, "player_id": needPlayer, "player_ping": needPlayer,
	"player_ip": needPlayer, "player_guid": needPlayer, "player_role": needPlayer,
	"message": needChat, "players_online": needNothing,
	"counter": needPlayer, "threshold": needThreshold,
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
	// How many players are online (the player tracker, package players). Works on
	// every trigger: "If {players_online} > 0, Kick everyone, else Shut down".
	"players_online": {"num", needNothing},
	// The player's counter in this run of the flow (see Branch.Counter).
	"counter": {"num", needPlayer},
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
		val = RoleName(n)
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
