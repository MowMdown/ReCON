// HTTP -> BattlEye RCON bridge (stdlib only).
//
//	POST /rcon  {"command":"say -1 Restart in 5 minutes"}  (or text/plain body)
//	GET  /rcon?cmd=players
//	POST /kickall {"reason":"Server restarting","lock":false,"shutdown":false}  (kicks highest player # down to 0)
//	GET  /health                                           (no auth)
//
// Auth: "Authorization: Bearer <token>" (or the raw token), or "X-API-Token: <token>".
//
// Env: RCON_HOST (127.0.0.1) RCON_PORT (2306) RCON_PASSWORD (required)
//
//	API_TOKEN (required) LISTEN_ADDR (:8080)
//	ALLOWED_PREFIXES optional, comma list e.g. "say,players,#lock,#unlock,#shutdown"
package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	rconAddr     string
	rconPassword string
	apiToken     string
	allowed      []string
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func build(ptype byte, payload []byte) []byte {
	body := append([]byte{0xFF, ptype}, payload...)
	pkt := []byte{'B', 'E', 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(pkt[2:6], crc32.ChecksumIEEE(body))
	return append(pkt, body...)
}

func parse(data []byte) (ptype byte, payload []byte, ok bool) {
	if len(data) < 8 || data[0] != 'B' || data[1] != 'E' {
		return 0, nil, false
	}
	body := data[6:]
	if binary.LittleEndian.Uint32(data[2:6]) != crc32.ChecksumIEEE(body) || body[0] != 0xFF {
		return 0, nil, false
	}
	return body[1], body[2:], true
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

var errLoginRejected = errors.New("RCON login rejected (wrong password or IP not allowed)")

func login(conn net.Conn, buf []byte) error {
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(build(0, []byte(rconPassword))); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if isTimeout(err) {
					break
				}
				return err
			}
			t, p, ok := parse(buf[:n])
			if !ok || t != 0 {
				continue
			}
			if len(p) < 1 || p[0] != 1 {
				return errLoginRejected
			}
			return nil
		}
	}
	return errors.New("no reply to RCON login after 3 tries: RCon not enabled (no RConPassword), wrong port, or UDP blocked")
}

type session struct {
	conn net.Conn
	buf  []byte
	seq  byte
}

func dial() (*session, error) {
	conn, err := net.Dial("udp", rconAddr)
	if err != nil {
		return nil, err
	}
	s := &session{conn: conn, buf: make([]byte, 65535)}
	if err := login(conn, s.buf); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *session) close() { s.conn.Close() }

func (s *session) run(command string, wait time.Duration) (string, bool, error) {
	seq := s.seq
	s.seq++
	if _, err := s.conn.Write(build(1, append([]byte{seq}, command...))); err != nil {
		return "", false, err
	}
	s.conn.SetReadDeadline(time.Now().Add(wait))
	parts := map[byte][]byte{}
	acked := false
	for {
		n, err := s.conn.Read(s.buf)
		if err != nil {
			if isTimeout(err) {
				break
			}
			return "", false, err
		}
		t, p, ok := parse(s.buf[:n])
		if !ok {
			continue
		}
		if t == 2 && len(p) > 0 { // server push message: must be acked
			s.conn.Write(build(2, p[:1]))
			continue
		}
		if t == 1 && len(p) > 0 && p[0] == seq { // reply to this command
			acked = true
			body := p[1:]
			if len(body) >= 3 && body[0] == 0 { // multipart: 0x00 | total | index
				parts[body[2]] = append([]byte(nil), body[3:]...)
				if len(parts) == int(body[1]) {
					break
				}
			} else {
				parts[0] = append([]byte(nil), body...)
				break
			}
		}
	}
	idx := make([]int, 0, len(parts))
	for k := range parts {
		idx = append(idx, int(k))
	}
	sort.Ints(idx)
	var sb strings.Builder
	for _, i := range idx {
		sb.Write(parts[byte(i)])
	}
	return sb.String(), acked, nil
}

func rcon(command string, wait time.Duration) (string, bool, error) {
	s, err := dial()
	if err != nil {
		return "", false, err
	}
	defer s.close()
	return s.run(command, wait)
}

var keepaliveEvery = 30 * time.Second

type live struct {
	conn    net.Conn
	mu      sync.Mutex // guards writing to conn and seq counter
	seq     byte
	replies chan []byte
	done    chan struct{}
	stop    chan struct{}
	once    sync.Once
	err     error
}

func (l *live) write(pkt []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.conn.Write(pkt)
	return err
}

func dialLive() (*live, error) {
	conn, err := net.Dial("udp", rconAddr)
	if err != nil {
		return nil, err
	}
	if err := login(conn, make([]byte, 65535)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	l := &live{
		conn:    conn,
		replies: make(chan []byte, 256), // Expanded capacity for large multipart responses
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	go l.reader()
	go l.keepalive()
	return l, nil
}

func (l *live) close() {
	l.once.Do(func() {
		close(l.stop)
		l.conn.Close()
	})
}

func (l *live) dead() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *live) reader() {
	buf := make([]byte, 65535)
	for {
		n, err := l.conn.Read(buf)
		if err != nil {
			l.err = err
			close(l.done)
			return
		}
		t, p, ok := parse(buf[:n])
		if !ok || len(p) == 0 {
			continue
		}
		switch t {
		case 2: // server push message: must be acknowledged safely
			l.write(build(2, p[:1]))
		case 1:
			select {
			case l.replies <- append([]byte(nil), p...):
			default:
			}
		}
	}
}

func (l *live) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.mu.Lock()
			seq := l.seq
			l.seq++
			l.mu.Unlock()
			l.write(build(1, []byte{seq}))
		case <-l.stop:
			return
		case <-l.done:
			return
		}
	}
}

func (l *live) alive() bool {
	_, acked, err := l.run("", 2*time.Second)
	return err == nil && acked
}

func (l *live) run(command string, wait time.Duration) (string, bool, error) {
	if l.dead() {
		return "", false, l.err
	}

	l.mu.Lock()
	for len(l.replies) > 0 { // drain stale keepalive/command ACKs
		<-l.replies
	}
	seq := l.seq
	l.seq++
	l.mu.Unlock()

	if err := l.write(build(1, append([]byte{seq}, command...))); err != nil {
		return "", false, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	parts := map[byte][]byte{}
	acked := false
loop:
	for {
		select {
		case p := <-l.replies:
			if p[0] != seq {
				continue
			}
			acked = true
			body := p[1:]
			if len(body) >= 3 && body[0] == 0 { // multipart: 0x00 | total | index
				parts[body[2]] = append([]byte(nil), body[3:]...)
				if len(parts) == int(body[1]) {
					break loop
				}
			} else {
				parts[0] = append([]byte(nil), body...)
				break loop
			}
		case <-timer.C:
			break loop
		case <-l.done:
			return "", false, l.err
		}
	}
	idx := make([]int, 0, len(parts))
	for k := range parts {
		idx = append(idx, int(k))
	}
	sort.Ints(idx)
	var sb strings.Builder
	for _, i := range idx {
		sb.Write(parts[byte(i)])
	}
	return sb.String(), acked, nil
}

var playerRow = regexp.MustCompile(`(?m)^\s*(\d+)\s+\S+:\d+\s`)

func playerIDs(out string) []int {
	ids := []int{}
	for _, m := range playerRow.FindAllStringSubmatch(out, -1) {
		if id, err := strconv.Atoi(m[1]); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	return ids
}

type kickReport struct {
	Order         []int  `json:"order"`
	Unacked       []int  `json:"unacked"`
	Remaining     []int  `json:"remaining"`
	Raw           string `json:"raw,omitempty"`
	ShutdownSent  bool   `json:"shutdown_sent"`
	ShutdownAcked bool   `json:"shutdown_acked"`
	ShutdownError string `json:"shutdown_error,omitempty"`
}

type runner interface {
	run(command string, wait time.Duration) (string, bool, error)
}

func kickAll(reason string, lock, shutdown bool) (*kickReport, error) {
	s, err := dial()
	if err != nil {
		return nil, err
	}
	defer s.close()
	return kickAllOn(s, reason, lock, shutdown)
}

func kickAllOn(s runner, reason string, lock, shutdown bool) (*kickReport, error) {
	reason = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(reason))
	if lock {
		if _, _, err := s.run("#lock", 2*time.Second); err != nil {
			return nil, err
		}
	}
	out, _, err := s.run("players", 3*time.Second)
	if err != nil {
		return nil, err
	}
	rep := &kickReport{Order: playerIDs(out), Unacked: []int{}, Remaining: []int{}}
	if len(rep.Order) == 0 {
		rep.Raw = out
	}
	for _, id := range rep.Order {
		cmd := fmt.Sprintf("kick %d %s", id, reason)
		if _, acked, err := s.run(strings.TrimSpace(cmd), 1000*time.Millisecond); err != nil {
			return nil, err
		} else if !acked {
			rep.Unacked = append(rep.Unacked, id)
		}
	}
	if len(rep.Order) > 0 {
		time.Sleep(500 * time.Millisecond)
		if out, _, err := s.run("players", 3*time.Second); err == nil {
			rep.Remaining = playerIDs(out)
		}
	}
	if shutdown {
		rep.ShutdownSent = true
		_, acked, err := s.run("#shutdown", 1500*time.Millisecond)
		rep.ShutdownAcked = acked
		if err != nil {
			rep.ShutdownError = err.Error()
		}
	}
	return rep, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func authed(r *http.Request) bool {
	tok := r.Header.Get("X-API-Token")
	if tok == "" {
		tok = r.Header.Get("Authorization")
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(apiToken)) == 1
}

func permitted(command string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, p := range allowed {
		if strings.HasPrefix(strings.ToLower(command), p) {
			return true
		}
	}
	return false
}

func runCommand(w http.ResponseWriter, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "no command"})
		return
	}
	if !permitted(command) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "command not allowed"})
		return
	}
	out, acked, err := rcon(command, 2*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "command": command, "acked": acked, "output": out})
}

func handleRcon(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		writeJSON(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		runCommand(w, r.URL.Query().Get("cmd"))
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "bad body"})
			return
		}
		cmd := string(raw)
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			var body struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSON(w, 400, map[string]any{"ok": false, "error": "bad JSON"})
				return
			}
			cmd = body.Command
		}
		runCommand(w, cmd)
	default:
		writeJSON(w, 405, map[string]any{"ok": false, "error": "method not allowed"})
	}
}

func handleKickAll(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		writeJSON(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"ok": false, "error": "use POST"})
		return
	}
	var body struct {
		Reason   string `json:"reason"`
		Lock     bool   `json:"lock"`
		Shutdown bool   `json:"shutdown"`
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "bad JSON"})
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "Server restarting"
	}
	if !permitted("players") || !permitted("kick") || (body.Lock && !permitted("#lock")) || (body.Shutdown && !permitted("#shutdown")) {
		writeJSON(w, 403, map[string]any{"ok": false, "error": "command not allowed"})
		return
	}
	rep, err := kickAll(body.Reason, body.Lock, body.Shutdown)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
}

func printKickReport(rep *kickReport, err error) bool {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	}
	if len(rep.Order) == 0 {
		fmt.Println("No players parsed from `players` output:")
		fmt.Println(strings.TrimRight(rep.Raw, "\n"))
		return true
	}
	fmt.Printf("Kicked (highest to lowest): %v\n", rep.Order)
	if len(rep.Unacked) > 0 {
		fmt.Printf("No acknowledgement for: %v\n", rep.Unacked)
	}
	fmt.Printf("Still connected: %v\n", rep.Remaining)
	return true
}

func printResult(out string, acked bool, err error) bool {
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	case out != "":
		fmt.Println(strings.TrimRight(out, "\n"))
	case acked:
		fmt.Println("(ok, no output)")
	default:
		fmt.Println("(no acknowledgement from server)")
	}
	return true
}

func cli(args []string) {
	if args[0] == "kickall" {
		if !printKickReport(kickAll(strings.Join(args[1:], " "), false, false)) {
			os.Exit(1)
		}
		return
	}
	if args[0] != "-i" {
		if !printResult(rcon(strings.Join(args, " "), 2*time.Second)) {
			os.Exit(1)
		}
		return
	}
	repl()
}

func repl() {
	fmt.Println("Interactive RCON console (logs in once and stays logged in). Type 'exit' to quit.")
	var l *live
	defer func() {
		if l != nil {
			l.close()
		}
	}()
	connect := func() bool {
		if l != nil {
			l.close()
			l = nil
		}
		nl, err := dialLive()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return false
		}
		l = nl
		return true
	}
	connect()
	sc := bufio.NewScanner(os.Stdin)
	for fmt.Print("rcon> "); sc.Scan(); fmt.Print("rcon> ") {
		line := strings.TrimSpace(sc.Text())
		if line == "exit" || line == "quit" {
			return
		}
		if line == "" {
			continue
		}
		if l == nil || l.dead() {
			fmt.Println("(not connected, logging in...)")
			if !connect() {
				continue
			}
		}
		var acked bool
		var err error
		if line == "kickall" || strings.HasPrefix(line, "kickall ") {
			var rep *kickReport
			rep, err = kickAllOn(l, strings.TrimPrefix(line, "kickall"), false, false)
			printKickReport(rep, err)
			acked = err == nil
		} else {
			var out string
			out, acked, err = l.run(line, 2*time.Second)
			printResult(out, acked, err)
		}
		if (err != nil || !acked) && !l.alive() {
			fmt.Println("(session lost, logging in again...)")
			connect()
		}
	}
}

func main() {
	rconPassword = os.Getenv("RCON_PASSWORD")
	if rconPassword == "" {
		log.Fatal("RCON_PASSWORD is required")
	}
	rconAddr = net.JoinHostPort(env("RCON_HOST", "127.0.0.1"), env("RCON_PORT", "2306"))
	if len(os.Args) > 1 {
		cli(os.Args[1:])
		return
	}

	apiToken = os.Getenv("API_TOKEN")
	if apiToken == "" {
		log.Fatal("API_TOKEN is required")
	}
	for _, p := range strings.Split(os.Getenv("ALLOWED_PREFIXES"), ",") {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			allowed = append(allowed, p)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/rcon", handleRcon)
	mux.HandleFunc("/kickall", handleKickAll)
	mux.HandleFunc("/players", handlePlayersPage)
	mux.HandleFunc("/api/players", handlePlayersJSON)
	mux.HandleFunc("/api/players/", handlePlayerAction)

	addr := env("LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      120 * time.Second,
	}
	log.Printf("listening on %s -> RCON %s", addr, rconAddr)
	log.Fatal(srv.ListenAndServe())
}