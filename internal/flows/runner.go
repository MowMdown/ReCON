package flows

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// flowPlayer is the player a join or chat run is for, as they were when it
// started. Their number is looked up again (by GUID) whenever it's used,
// because numbers are reused when players leave.
type flowPlayer struct {
	battleye.Player
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
	if p, ok := players.ByGUID(x.p.GUID); ok {
		return p.ID, true
	}
	return 0, false
}

// placeholderValue is what a placeholder or If subject stands for in this run.
func (x *job) placeholderValue(name string) (string, bool) {
	switch name {
	case "players_online":
		return strconv.Itoa(len(onlineNow())), true
	case "threshold":
		return strconv.Itoa(x.run.counterThreshold), true
	}
	if x.p == nil {
		return "", false
	}
	switch name {
	case "player_name":
		return battleye.BareName(x.p.Name), true
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
		return RoleName(x.p.role), true
	case "message":
		return x.p.message, true
	case "counter":
		return strconv.Itoa(x.run.counterOf(x.p.GUID)), true
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
	return battleye.CleanReason(tokenRe.ReplaceAllStringFunc(text, func(m string) string {
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
		x.run.logf("%s has left; message not sent", battleye.BareName(x.p.Name))
		return
	}
	cmd := fmt.Sprintf("say %d %s", id, text)
	sendCommand(x.run, cmd)
	recordSentMessage(x.p.GUID, cmd)
}

// recordSentMessage adds a private message ReCON sent to a player's chat history.
func recordSentMessage(guid, cmd string) {
	if database == nil || guid == "" {
		return
	}
	if err := database.AddChatMessage(time.Now(), guid, store.ChatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// listOnlineNames is the latest player list as "Online (N): a, b, c".
func listOnlineNames() string {
	online := onlineNow()
	names := make([]string, 0, len(online))
	for _, p := range online {
		names = append(names, battleye.BareName(p.Name))
	}
	if len(names) == 0 {
		return "Online (0): nobody"
	}
	return fmt.Sprintf("Online (%d): %s", len(names), strings.Join(names, ", "))
}

// banStep bans the player. Tests replace it.
var banStep = func(p battleye.Player, online bool, minutes int, reason, logAs string) bans.Result {
	return bans.BanGUID(p, online, minutes, reason, logAs)
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
				x.run.logf("%s has left; kick skipped", battleye.BareName(x.p.Name))
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
				x.run.logf("ban of %s failed: %s", battleye.BareName(p.Name), res.Error)
				x.run.markRunFailed()
			} else {
				x.run.logf("banned %s (%s)", battleye.BareName(p.Name), banLength(minutes))
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
				cmp := b
				if strings.TrimSpace(cmp.Value) == counterThresholdToken {
					cmp.Value = strconv.Itoa(x.run.counterThreshold)
				}
				ok := evaluateIfCondition(cmp, v)
				word := "If "
				if k > 0 {
					word = "Else if "
				}
				if !x.inLoop {
					x.run.logf("%s%s: %s", word, describeIfCondition(b), map[bool]string{true: "yes", false: "no"}[ok])
				} else if ok {
					x.run.logf("%s (#%d): %s%s", battleye.BareName(x.p.Name), x.p.ID, word, describeIfCondition(b))
				}
				if ok {
					x.changeCounter(b.Counter)
					if !x.execFlowSteps(b.Steps) {
						return false
					}
					ran = true
					break
				}
			}
			if !ran && s.Else != nil {
				if x.inLoop {
					x.run.logf("%s (#%d): Else", battleye.BareName(x.p.Name), x.p.ID)
				} else {
					x.run.logf("Else")
				}
				x.changeCounter(s.ElseCounter)
				if !x.execFlowSteps(*s.Else) {
					return false
				}
			}
		}
	}
	return true
}

// counterOf is a player's counter in this run (0 until a branch changes it).
func (r *flowRun) counterOf(guid string) int {
	r.counterMu.Lock()
	defer r.counterMu.Unlock()
	return r.counters[strings.ToLower(guid)]
}

// changeCounter applies an If branch's or Else's counter change (see
// Branch.Counter) to the player, and logs the new value when it changes.
func (x *job) changeCounter(op string) {
	if op == "" || x.p == nil {
		return
	}
	r, g := x.run, strings.ToLower(x.p.GUID)
	r.counterMu.Lock()
	if r.counters == nil {
		r.counters = map[string]int{}
	}
	old := r.counters[g]
	n := old
	switch op {
	case "inc":
		n++
	case "dec":
		n = max(n-1, 0)
	case "reset":
		n = 0
	}
	r.counters[g] = n
	r.counterMu.Unlock()
	if n != old {
		r.logf("%s (#%d): counter %s, now %d/%d", battleye.BareName(x.p.Name), x.p.ID, counterOps[op], n, r.counterThreshold)
	}
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
		r.chatWindow.to = rcon.Events.LastSeq()
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
		p   battleye.Player
		msg string
	}
	var items []item
	from, to := x.run.chatWindowOf()
	x.run.polled.Do(func() { players.PollNow() })
	if of == forEachMessage {
		senders := map[string]bool{}
		for _, l := range players.ChatsAfter(from) {
			if l.Chat.Seq > to {
				continue // sent after this run started looking
			}
			p, ok := players.ByGUID(l.GUID)
			if !ok {
				continue // left since the line was sent
			}
			items = append(items, item{p, l.Chat.Text})
			senders[l.GUID] = true
		}
		x.run.logf("For each message: %d new from %d player(s)", len(items), len(senders))
	} else {
		for _, p := range onlineNow() { // by player number
			if p.GUID == "" {
				continue
			}
			msg := "" // only what they wrote since the flow's last run
			if c, ok := players.LatestChatOf(p.GUID); ok && c.Seq > from && c.Seq <= to {
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
	// lists the players and kicks each (battleye.KickAll).
	if bare := strings.TrimPrefix(lower, "#"); bare == "kickall" || strings.HasPrefix(bare, "kickall ") {
		reason := strings.TrimSpace(strings.TrimPrefix(cmd, "#")[len("kickall"):])
		rep, err := rcon.KickAll(reason, rcon.FlowSource(run.name))
		if err != nil {
			run.logf("kickall failed: %v", err)
			run.markRunFailed()
			return
		}
		run.addToRunLog(rep.Summary(reason))
		return
	}

	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := rcon.Run(cmd, 2*time.Second, retry, rcon.FlowSource(run.name))
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
