package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// console.go overview
// ----------------------------------------------------------------------------
// The Players page's Console and Message dialogs:
//
//   - the RCON console: GET /api/console?after=N returns the server messages
//     and commands since N (serverEvents, rcon.go), POST /api/console runs any
//     RCON command and puts its reply in the same log.
//   - per-player chat history: every chat line a player writes and every
//     private message ReCON sends them, kept in SQLite under their GUID (GUIDs
//     never change; player numbers are reused). GET /api/players/{guid}/chat
//     reads it, POST /api/players/{guid}/chat sends `say <id> <message>`.
//   - the chat recorder: a background subscriber to serverEvents that files
//     each incoming chat line under the GUID of the player who wrote it.
//
// The chat_messages SQL lives here with the rest of the feature rather than
// in db.go.
// ============================================================================

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// chatHistoryLimit is how many lines GET /api/players/{guid}/chat returns:
// the newest ones, oldest first. Everything stays in the table.
const chatHistoryLimit = 300

// Chat directions.
const (
	chatIn  = "in"  // the player wrote it
	chatOut = "out" // ReCON sent it to them
)

// chatLine is one row of a player's history, as /api/players/{guid}/chat
// returns it. Line is ready to show: "(Global) Name: text" for chat in,
// "> say 3 text" for messages out.
type chatLine struct {
	Time      time.Time `json:"time"`
	Direction string    `json:"direction"`
	Line      string    `json:"line"`
}

// EnsureChatTable creates the chat_messages table (once; safe to repeat).
func (db *Database) EnsureChatTable() error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS chat_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at DATETIME NOT NULL,
		guid TEXT NOT NULL,
		direction TEXT NOT NULL,
		line TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_guid
		ON chat_messages(guid, id);`)
	return err
}

// AddChatMessage stores one line of a player's history.
func (db *Database) AddChatMessage(at time.Time, guid, direction, line string) error {
	_, err := db.Exec("INSERT INTO chat_messages (at, guid, direction, line) VALUES (?, ?, ?, ?)",
		at.UTC(), strings.ToLower(guid), direction, line)
	return err
}

// ChatHistory returns a player's newest `limit` lines, oldest first.
func (db *Database) ChatHistory(guid string, limit int) ([]chatLine, error) {
	rows, err := db.Query(`
		SELECT at, direction, line FROM (
			SELECT id, at, direction, line FROM chat_messages
			WHERE guid = ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, strings.ToLower(guid), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []chatLine{}
	for rows.Next() {
		var l chatLine
		if err := rows.Scan(&l.Time, &l.Direction, &l.Line); err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// GUIDByName returns the GUID of the most recently seen player with this
// exact name, or "" if there is none.
func (db *Database) GUIDByName(name string) (string, error) {
	var guid string
	err := db.QueryRow("SELECT guid FROM players WHERE username = ? ORDER BY last_seen DESC LIMIT 1", name).Scan(&guid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return guid, err
}

// ---------------------------------------------------------------------------
// Matching chat to players
// ---------------------------------------------------------------------------

// bareName drops the "(Lobby)" tag BattlEye adds while a player is loading.
func bareName(name string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), lobbySuffix))
}

// onlineByName and onlineByID look a player up in the poller's latest
// `players` list (flows.go), so they never send a command of their own.
func onlineByName(name string) (Player, bool) {
	latest.Lock()
	defer latest.Unlock()
	for _, p := range latest.players {
		if bareName(p.Name) == name {
			return p, true
		}
	}
	return Player{}, false
}

func onlineByID(id int) (Player, bool) {
	latest.Lock()
	defer latest.Unlock()
	for _, p := range latest.players {
		if p.ID == id {
			return p, true
		}
	}
	return Player{}, false
}

// guidForChat finds who wrote a chat line: an online player with that name,
// else the last player seen with it (someone who chatted and left before the
// next poll). "" when nobody matches.
func guidForChat(name string) string {
	if p, ok := onlineByName(name); ok && p.GUID != "" {
		return p.GUID
	}
	guid, err := globalDatabase.GUIDByName(name)
	if err != nil {
		log.Printf("chat: looking up %q: %v", name, err)
	}
	return guid
}

// startChatRecorder creates the chat table and files every incoming chat
// line under its writer's GUID for as long as ReCON runs. A line from a name
// no player has had is logged and left out of the history (it still shows in
// the console).
func startChatRecorder() {
	if err := globalDatabase.EnsureChatTable(); err != nil {
		log.Printf("chat: can't create chat_messages, chat history is off: %v", err)
		return
	}
	events, _ := serverEvents.subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind != eventChat {
				continue
			}
			guid := guidForChat(ev.Name)
			if guid == "" {
				log.Printf("chat: no player named %q, line not kept in a history: %s", ev.Name, ev.Text)
				continue
			}
			if err := globalDatabase.AddChatMessage(ev.Time, guid, chatIn, ev.Text); err != nil {
				log.Printf("chat: store: %v", err)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Console API
// ---------------------------------------------------------------------------

// handleConsoleEvents serves GET /api/console?after=N: every kept event
// newer than N, plus "last" to send as after= next time. The page polls it
// while the console is open; after=0 (or none) returns the whole buffer.
func handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, last := serverEvents.afterWithLast(after)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events, "last": last})
}

// consoleSayRe matches a private message typed in the console (say 3 hi),
// so it can be added to that player's history. say -1 (everyone) is not.
var consoleSayRe = regexp.MustCompile(`(?i)^say\s+(\d+)\s+(.+)$`)

// handleConsoleCommand serves POST /api/console {"command": "..."}: runs one
// command exactly as typed and writes the outcome into the console log.
// runShared already logged the command itself (as "console"). kickall is
// ReCON's own command, as in the CLI console; everything else goes to
// BattlEye.
func handleConsoleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	cmd := cleanReason(req.Command)
	if cmd == "" {
		fail(w, http.StatusBadRequest, "no command")
		return
	}
	note := func(kind, text string) {
		serverEvents.publish(ServerEvent{Time: time.Now(), Kind: kind, Text: text, PlayerID: -1})
	}

	if low := strings.ToLower(cmd); low == "kickall" || strings.HasPrefix(low, "kickall ") {
		reason := strings.TrimSpace(cmd[len("kickall"):])
		if reason == "" {
			reason = "Server restarting"
		}
		note(eventSent, "> "+cmd)
		rep, err := kickAll(sharedRunner{}, reason)
		if err != nil {
			note(eventError, "kickall failed: "+err.Error())
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("rcon[console] > %s", rep.summary(reason))
		note(eventReply, rep.summary(reason))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": true, "output": rep.summary(reason)})
		return
	}

	out, acked, err := runShared(cmd, 3*time.Second, false, "console")
	switch {
	case err != nil:
		note(eventError, "error: "+err.Error())
		fail(w, http.StatusBadGateway, err.Error())
		return
	case !acked:
		note(eventError, "(no acknowledgement from server)")
	case strings.TrimSpace(out) != "":
		note(eventReply, strings.TrimRight(out, "\r\n "))
	}
	if acked {
		recordConsoleSay(cmd)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked, "output": out})
}

// recordConsoleSay adds a `say <id> ...` typed in the console to that
// player's history, if they are online.
func recordConsoleSay(cmd string) {
	m := consoleSayRe.FindStringSubmatch(cmd)
	if m == nil {
		return
	}
	id, _ := strconv.Atoi(m[1])
	p, ok := onlineByID(id)
	if !ok || p.GUID == "" {
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), p.GUID, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Per-player chat API
// ---------------------------------------------------------------------------

// chatGUID returns the {guid} path value, lower-cased; false (with a 404
// written) when it isn't a GUID.
func chatGUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	guid := r.PathValue("guid")
	if !guidPattern.MatchString(guid) {
		fail(w, http.StatusNotFound, "invalid player GUID")
		return "", false
	}
	return strings.ToLower(guid), true
}

// handleChatHistory serves GET /api/players/{guid}/chat: the player's chat
// and the private messages sent to them, oldest first. Works while RCON is
// down, and for players who are offline.
func handleChatHistory(w http.ResponseWriter, r *http.Request) {
	guid, ok := chatGUID(w, r)
	if !ok {
		return
	}
	lines, err := globalDatabase.ChatHistory(guid, chatHistoryLimit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "messages": lines})
}

// handleChatSend serves POST /api/players/{guid}/chat {"message": "..."}:
// sends `say <id> <message>` to the player with that GUID and adds it to
// their history. It reads `players` first because the player's number can
// change between page refreshes (it's reused after someone leaves).
func handleChatSend(w http.ResponseWriter, r *http.Request) {
	guid, ok := chatGUID(w, r)
	if !ok {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	msg := cleanReason(req.Message)
	if msg == "" {
		fail(w, http.StatusBadRequest, "message is required")
		return
	}
	out, acked, err := runShared("players", 3*time.Second, true, "")
	if err != nil || !acked {
		rconUnavailable(w, "RCON unavailable")
		return
	}
	var p Player
	found := false
	for _, x := range parsePlayers(out) {
		if strings.EqualFold(x.GUID, guid) {
			p, found = x, true
			break
		}
	}
	if !found {
		fail(w, http.StatusConflict, "player is no longer connected")
		return
	}
	cmd := fmt.Sprintf("say %d %s", p.ID, msg)
	_, acked, err = runShared(cmd, 2*time.Second, false, "web")
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if !acked {
		fail(w, http.StatusBadGateway, "message was not acknowledged by the server")
		return
	}
	if err := globalDatabase.AddChatMessage(time.Now(), guid, chatOut, "> "+cmd); err != nil {
		log.Printf("chat: store: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
