package flows

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
)

// chatCommandDef is one built-in chat command as the settings dialog shows
// it: its default cooldown, whether it has Announce, a vote length or a
// warning threshold, its help text, and the messages it sends (each with its
// default wording and the placeholders it may use).
type chatCommandDef struct {
	Cmd      string      `json:"cmd"`
	Staff    bool        `json:"staff"`    // mods and admins only
	Cool     string      `json:"cool"`     // default cooldown
	Announce bool        `json:"announce"` // has the Announce setting
	Length   bool        `json:"length"`   // has the vote length setting
	Warn     bool        `json:"warn"`     // has the warning threshold and the Kick/Ban setting
	Help     string      `json:"help"`     // what !help !<cmd> sends
	Fields   []chatField `json:"fields"`
}

type chatField struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Def   string   `json:"def"`
	Ph    []string `json:"ph"` // placeholders it may use
}

var ChatCommandDefs = []chatCommandDef{
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
	{Cmd: "kick", Staff: true, Cool: "0", Announce: true, Help: "!kick <code or player number> <reason> kicks that player. Their code comes from !code, their number from !players.",
		Fields: []chatField{
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} has been kicked by {player_name}: {reason}", []string{"target_name", "player_name", "reason"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been kicked", []string{"target_name", "reason"}},
		}},
	{Cmd: "ban", Staff: true, Cool: "0", Announce: true, Help: "!ban <code or player number> <reason> permanently bans that player. Their code (the last 6 characters of their GUID) comes from !code, their number from !players.",
		Fields: []chatField{
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} has been banned by {player_name}: {reason}", []string{"target_name", "player_name", "reason"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been banned", []string{"target_name", "reason"}},
		}},
	{Cmd: "warning", Staff: true, Cool: "0", Announce: true, Warn: true, Help: "!warning <code or player number> <reason> warns that player. Enough warnings in one visit and they are kicked or banned.",
		Fields: []chatField{
			{"player", "Warning (to the player)", "Warning {warnings}/{warn_count} from {player_name}: {reason}", []string{"target_name", "player_name", "reason", "warnings", "warn_count"}},
			{"staff", "Notice (to each mod and admin online, always)", "{target_name} was warned by {player_name} ({warnings}/{warn_count}): {reason}", []string{"target_name", "player_name", "reason", "warnings", "warn_count"}},
			{"announce", "Announcement (to everyone, when Announce is on)", "{target_name} has been warned: {reason}", []string{"target_name", "reason", "warnings", "warn_count"}},
			{"limit_staff", "At the threshold: notice (to each mod and admin online, always)", "{target_name} reached {warn_count} warnings and has been {action}: {reason}", []string{"target_name", "player_name", "reason", "warn_count", "action"}},
			{"limit_announce", "At the threshold: announcement (to everyone, when Announce is on)", "{target_name} has been {action}", []string{"target_name", "reason", "action"}},
		}},
}

// Defaults for !warning: kick or ban at the second warning.
const (
	defaultWarnThreshold = 2
	defaultWarnAction    = "ban"
	maxWarnThreshold     = 100
)

func chatDef(cmd string) *chatCommandDef {
	for i := range ChatCommandDefs {
		if ChatCommandDefs[i].Cmd == cmd {
			return &ChatCommandDefs[i]
		}
	}
	return nil
}

// chatSetting is one command's settings as stored and edited.
type chatSetting struct {
	On       bool   `json:"on"`
	Cool     string `json:"cool"`               // server-wide cooldown, "0" = none
	Announce bool   `json:"announce,omitempty"` // kick and ban: also tell everyone
	Length   string `json:"length,omitempty"`   // vote: how long it stays open
	// warning: how many warnings in one visit set off Action ("kick" or
	// "ban"). Action is read when a player reaches the threshold.
	Threshold int               `json:"threshold,omitempty"`
	Action    string            `json:"action,omitempty"`
	Msgs      map[string]string `json:"msgs"`
}

// ChatSettings is every command's settings, by command name.
type ChatSettings map[string]chatSetting

// chatSettingsKey is where the settings are stored in the settings table.
const chatSettingsKey = "chat_commands"

func DefaultChatSettings() ChatSettings {
	out := ChatSettings{}
	for _, d := range ChatCommandDefs {
		s := chatSetting{On: true, Cool: d.Cool, Announce: d.Announce, Msgs: map[string]string{}}
		if d.Length {
			s.Length = "5m"
		}
		if d.Warn {
			s.Threshold, s.Action = defaultWarnThreshold, defaultWarnAction
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
func mergeChatSettings(saved ChatSettings) ChatSettings {
	out := DefaultChatSettings()
	for _, d := range ChatCommandDefs {
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
		if d.Warn {
			m.Threshold, m.Action = s.Threshold, s.Action
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
func checkChatSettings(s ChatSettings) error {
	for _, d := range ChatCommandDefs {
		c := s[d.Cmd]
		if secs, ok := parseDur(c.Cool); !ok || secs > maxTimeSecs {
			return fmt.Errorf("/%s cooldown: enter seconds, or a time such as 1m30s (24h at most)", d.Cmd)
		}
		if d.Length {
			if secs, ok := parseDur(c.Length); !ok || secs < 1 || secs > 3600 {
				return fmt.Errorf("/%s length: enter a time from 1s to 1h, such as 5m", d.Cmd)
			}
		}
		if d.Warn {
			if c.Threshold < 1 || c.Threshold > maxWarnThreshold {
				return fmt.Errorf("/%s threshold: enter a whole number from 1 to %d", d.Cmd, maxWarnThreshold)
			}
			if c.Action != "kick" && c.Action != "ban" {
				return fmt.Errorf("/%s: pick Kick or Ban", d.Cmd)
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
	settings ChatSettings
	loaded   bool
	lastUsed map[string]time.Time
	vote     *chatVote
}{lastUsed: map[string]time.Time{}}

type chatVote struct {
	topic   string
	yes, no int
	voted   map[string]bool // by GUID: the first answer counts
}

// CurrentChatSettings returns the settings, reading them from the database
// the first time.
func CurrentChatSettings() ChatSettings {
	chatState.Lock()
	defer chatState.Unlock()
	if !chatState.loaded {
		chatState.settings = loadChatSettings()
		chatState.loaded = true
	}
	return chatState.settings
}

func loadChatSettings() ChatSettings {
	saved := ChatSettings{}
	if database != nil {
		raw, ok, err := database.GetStoredSetting(chatSettingsKey)
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

// SaveChatSettings checks and stores settings from the dialog.
func SaveChatSettings(s ChatSettings) (ChatSettings, error) {
	s = mergeChatSettings(s)
	for cmd, c := range s {
		c.Cool = strings.ToLower(strings.TrimSpace(c.Cool))
		c.Length = strings.ToLower(strings.TrimSpace(c.Length))
		for k, v := range c.Msgs {
			c.Msgs[k] = battleye.CleanReason(v)
		}
		s[cmd] = c
	}
	if err := checkChatSettings(s); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(s)
	if err := database.SaveStoredSetting(chatSettingsKey, string(b)); err != nil {
		return nil, err
	}
	chatState.Lock()
	chatState.settings, chatState.loaded = s, true
	chatState.Unlock()
	return s, nil
}

// fillTemplate replaces {placeholders} in a message from settings.
func fillTemplate(tpl string, vals map[string]string) string {
	return battleye.CleanReason(tokenRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := vals[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	}))
}

// chatSay sends `say <to> <text>` for a chat command (to -1 is everyone)
// and reports whether the server acknowledged it. A private message goes
// into that player's chat history. Tests replace it.
var chatSay = func(to battleye.Player, everyone bool, text string) bool {
	cmd := fmt.Sprintf("say %d %s", to.ID, text)
	if everyone {
		cmd = "say -1 " + text
	}
	_, acked, err := game.Run(cmd, 2*time.Second, false, "chat")
	if err == nil && acked && !everyone {
		recordSentMessage(to.GUID, cmd)
	}
	return err == nil && acked
}

// chatKick kicks a player for !kick. Tests replace it.
var chatKick = func(p battleye.Player, reason string) bool {
	_, acked, err := game.Run(fmt.Sprintf("kick %d %s", p.ID, reason), 2*time.Second, false, "chat")
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
func onlineNow() []battleye.Player {
	players, _ := game.OnlinePlayers()
	return players
}

// listOnlineStaff is every mod and admin online.
func listOnlineStaff() []battleye.Player {
	roles := map[string]int{}
	if database != nil {
		if r, err := database.AllPlayerRoles(); err == nil {
			roles = r
		}
	}
	var out []battleye.Player
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
func handleChatCommand(p battleye.Player, role int, text string) {
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
	all := CurrentChatSettings()
	set := all[cmd]
	if !set.On || def.Staff && role < roleMod {
		return
	}
	rest := strings.TrimSpace(text[len(fields[0]):])
	args := fields[1:]
	name := battleye.BareName(p.Name)
	if cmd != "vote" && coolingDown(cmd, set) {
		return
	}
	switch cmd {
	case "help":
		if len(args) == 0 {
			var names []string
			for _, d := range ChatCommandDefs {
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
		target, ok := game.PlayerByID(id)
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
				parts = append(parts, fillTemplate(set.Msgs["entry"], map[string]string{"target_name": battleye.BareName(o.Name), "target_id": strconv.Itoa(o.ID)}))
			}
			lines = append(lines, strings.Join(parts, " | "))
		}
		markUsed(cmd)
		go func() {
			for i, l := range lines {
				if i > 0 {
					time.Sleep(playersLinePause)
				}
				asker, ok := game.PlayerByGUID(p.GUID) // their number now; stop if they left
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

	case "warning":
		runWarningCommand(p, role, set, args)
	}
}

// chatVoteCommand handles !vote <question> (start a vote) and !vote y|n.
func chatVoteCommand(p battleye.Player, set chatSetting, rest string) {
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

	chatSay(p, true, fillTemplate(set.Msgs["start"], map[string]string{"player_name": battleye.BareName(p.Name), "topic": topic}))
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
		chatSay(battleye.Player{}, true, fillTemplate(set.Msgs["result"], map[string]string{
			"vote_result": result, "yes": strconv.Itoa(yes), "no": strconv.Itoa(no), "topic": v.topic}))
	}()
}

// findChatTarget finds the player a staff command names: a code (6 hex
// characters, the end of their GUID, from !code) or a player number (12 or
// #12, from !players). A number must be online; a code may also find a
// player who has left when offline is true (!ban). ok is false unless
// exactly one player matches.
func findChatTarget(arg string, offline bool) (t battleye.Player, online, ok bool) {
	if codeRe.MatchString(arg) {
		code := strings.ToLower(arg)
		var matches []battleye.Player
		for _, o := range onlineNow() {
			if codeOf(o.GUID) == code {
				matches = append(matches, o)
			}
		}
		online = len(matches) > 0
		if !online && offline && database != nil {
			known, err := database.PlayersByCode(code)
			if err != nil {
				log.Printf("chat: lookup of code %s: %v", code, err)
				return battleye.Player{}, false, false
			}
			matches = known
		}
		if len(matches) != 1 {
			return battleye.Player{}, false, false // nobody, or more than one player, has that code
		}
		return matches[0], online, true
	}
	id, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || id < 0 {
		return battleye.Player{}, false, false
	}
	t, online = game.PlayerByID(id)
	return t, online, online
}

// chatTargetAllowed reports whether p (of role) may punish t: nobody can
// punish themselves, or anyone of their own rank or higher, from chat.
func chatTargetAllowed(p battleye.Player, role int, t battleye.Player) bool {
	return !strings.EqualFold(t.GUID, p.GUID) && lookupPlayerRole(t.GUID) < role
}

// runPunishCommand handles !kick and !ban <code or player number> [reason].
// Mods can kick and ban Players; Admins can also kick and ban Mods. Nobody
// can kick or ban themselves, or anyone of their own rank or higher, from
// chat. !ban is always permanent and, by code, also finds players who have
// left.
func runPunishCommand(cmd string, p battleye.Player, role int, set chatSetting, args []string) {
	if len(args) == 0 {
		return
	}
	reason := battleye.CleanReason(strings.Join(args[1:], " "))
	if reason == "" {
		reason = "No reason given"
	}
	t, online, ok := findChatTarget(args[0], cmd == "ban")
	if !ok || !chatTargetAllowed(p, role, t) {
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
			log.Printf("chat: !ban of %s failed: %s", battleye.BareName(t.Name), res.Error)
			return
		}
	}
	markUsed(cmd)
	vals := map[string]string{"target_name": battleye.BareName(t.Name), "player_name": battleye.BareName(p.Name), "reason": reason}
	for _, s := range listOnlineStaff() {
		if !strings.EqualFold(s.GUID, t.GUID) { // a kicked Mod is on their way out
			chatSay(s, false, fillTemplate(set.Msgs["staff"], vals))
		}
	}
	if set.Announce {
		chatSay(battleye.Player{}, true, fillTemplate(set.Msgs["announce"], vals))
	}
	log.Printf("chat: %s %s %s (%s)", battleye.BareName(p.Name), done, battleye.BareName(t.Name), reason)
}

// warningActionDelay is the pause between the warning that reaches the
// threshold and the kick or ban, so the player can read why. Tests shorten it.
var warningActionDelay = 5 * time.Second

// runWarningCommand handles !warning <code or player number> [reason]: it
// adds one to the player's warnings for this visit, sends them the warning
// and tells the staff (and everyone, with Announce). The warning that reaches
// the threshold is followed, warningActionDelay later, by a kick or a
// permanent ban with that warning's reason, whichever the settings say at
// that moment. Who may warn whom is as for !kick.
func runWarningCommand(p battleye.Player, role int, set chatSetting, args []string) {
	if len(args) == 0 {
		return
	}
	t, _, ok := findChatTarget(args[0], false)
	if !ok || !chatTargetAllowed(p, role, t) {
		return
	}
	reason := battleye.CleanReason(strings.Join(args[1:], " "))
	if reason == "" {
		reason = "No reason given"
	}
	count, reached, ok := game.WarnPlayer(t.GUID, set.Threshold)
	if !ok {
		return
	}
	markUsed("warning")
	vals := map[string]string{"target_name": battleye.BareName(t.Name), "player_name": battleye.BareName(p.Name), "reason": reason,
		"warnings": strconv.Itoa(count), "warn_count": strconv.Itoa(set.Threshold)}
	chatSay(t, false, fillTemplate(set.Msgs["player"], vals))
	for _, s := range listOnlineStaff() {
		if !strings.EqualFold(s.GUID, t.GUID) { // a warned Mod has their own message
			chatSay(s, false, fillTemplate(set.Msgs["staff"], vals))
		}
	}
	if set.Announce {
		chatSay(battleye.Player{}, true, fillTemplate(set.Msgs["announce"], vals))
	}
	log.Printf("chat: %s warned %s (%d/%d): %s", battleye.BareName(p.Name), battleye.BareName(t.Name), count, set.Threshold, reason)
	if !reached {
		return
	}
	action := set.Action
	go func() {
		time.Sleep(warningActionDelay)
		cur, online := game.PlayerByGUID(t.GUID) // their number now
		done := "kicked"
		if action == "kick" {
			if !online {
				log.Printf("chat: %s left before the warning kick", battleye.BareName(t.Name))
				return
			}
			if !chatKick(cur, reason) {
				return
			}
		} else {
			done = "banned"
			if !online {
				cur = t
			}
			if res := banStep(cur, online, 0, reason, "chat"); !res.OK {
				log.Printf("chat: warning ban of %s failed: %s", battleye.BareName(t.Name), res.Error)
				return
			}
		}
		vals["action"] = done
		for _, s := range listOnlineStaff() {
			if !strings.EqualFold(s.GUID, t.GUID) {
				chatSay(s, false, fillTemplate(set.Msgs["limit_staff"], vals))
			}
		}
		if set.Announce {
			chatSay(battleye.Player{}, true, fillTemplate(set.Msgs["limit_announce"], vals))
		}
		log.Printf("chat: %s %s after %d warnings (%s)", battleye.BareName(t.Name), done, count, reason)
	}()
}
