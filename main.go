package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// main.go overview
// ----------------------------------------------------------------------------
// Entry point. Reads configuration, opens the database, then either:
//
//   - runs a one-off CLI command or the interactive console (any arguments), or
//   - starts the server: background player/ban sync, the flow scheduler and
//     the web server (no arguments).
//
// Where everything else lives:
//
//	rcon.go   talking to the game server (BattlEye protocol, sessions, parsing)
//	web.go    HTTP routes, JSON API and the HTML pages
//	db.go     SQLite storage and the ban sync and chat recorder that feed it
//	players.go who is online and their sessions, from the console log
//	flows.go  flows (steps, conditions, the runner) and the built-in chat
//	          commands; one file per trigger: flows_schedule.go,
//	          flows_join.go, flows_message.go, flows_manual.go
// ============================================================================

var globalDatabase *Database // player database, opened in main()

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// RCON settings
// ---------------------------------------------------------------------------

// rconSettings is where ReCON finds the game server's RCON. The defaults
// come from the environment (docker-compose.yaml): RCON_HOST (default
// 127.0.0.1), RCON_PORT (default 2306) and RCON_PASSWORD. RCON Settings on
// the Players page can override any of them until ReCON restarts: the
// overrides are kept in memory only (rconOverrides), and a field left empty
// uses the default. The CLI uses the environment only.
type rconSettings struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Password string `json:"password"`
}

// rconOverrides is what RCON Settings last connected with (all empty until
// then).
var rconOverrides struct {
	sync.Mutex
	settings rconSettings
}

// rconDefaultSettings are the settings from the environment.
func rconDefaultSettings() rconSettings {
	return rconSettings{
		Host:     environmentOrDefault("RCON_HOST", "127.0.0.1"),
		Port:     environmentOrDefault("RCON_PORT", "2306"),
		Password: os.Getenv("RCON_PASSWORD"),
	}
}

// clean trims the fields and checks them. Empty fields are fine (they use
// the defaults). Errors are user-facing.
func (s rconSettings) clean() (rconSettings, error) {
	s.Host, s.Port = strings.TrimSpace(s.Host), strings.TrimSpace(s.Port)
	if strings.ContainsAny(s.Host, " \t/") {
		return s, errors.New("Server IP: enter an IP address or host name, such as 10.0.0.5")
	}
	if s.Port != "" {
		if n, err := strconv.Atoi(s.Port); err != nil || n < 1 || n > 65535 {
			return s, errors.New("Port: enter a number from 1 to 65535")
		}
	}
	return s, nil
}

// effective fills the empty fields from the defaults and returns the RCON
// endpoint ("host:port") and password to use.
func (s rconSettings) effective() (address, password string) {
	d := rconDefaultSettings()
	if s.Host == "" {
		s.Host = d.Host
	}
	if s.Port == "" {
		s.Port = d.Port
	}
	if s.Password == "" {
		s.Password = d.Password
	}
	return net.JoinHostPort(s.Host, s.Port), s.Password
}

// currentRconOverrides is what RCON Settings last connected with.
func currentRconOverrides() rconSettings {
	rconOverrides.Lock()
	defer rconOverrides.Unlock()
	return rconOverrides.settings
}

// keepRconOverrides remembers the settings RCON Settings connected with,
// until ReCON restarts.
func keepRconOverrides(s rconSettings) {
	rconOverrides.Lock()
	rconOverrides.settings = s
	rconOverrides.Unlock()
}

// main wires everything together:
//  1. Opens the player database (DB_PATH, default /data/players.db).
//  2. Works out where the game server's RCON is: the environment's
//     RCON_HOST, RCON_PORT and RCON_PASSWORD (rconSettings).
//  3. If command-line args were given, runs CLI mode and returns.
//  4. Otherwise runs as a server: clears stale sessions, prepares the flows
//     table, starts the background player sync and the flow scheduler, and
//     serves HTTP (LISTEN_ADDR, default :8080) forever.
func main() {
	dbPath := environmentOrDefault("DB_PATH", "/data/players.db")
	var err error
	globalDatabase, err = OpenPlayerDatabase(dbPath)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	address, password := rconSettings{}.effective()
	setRconTarget(address, password)

	// CLI mode: `prog kickall ...`, `prog -i`, `prog players`, etc.
	if len(os.Args) > 1 {
		if password == "" {
			log.Fatal(errNoPassword)
		}
		runCommandLine(os.Args[1:])
		return
	}
	// The web pages still start without a password, so one can be entered
	// in RCON Settings; until then every RCON call fails with errNoPassword.
	if password == "" {
		log.Printf("rcon: %v", errNoPassword)
	}

	// Drop sessions left open by a previous run so restart gaps aren't counted as playtime.
	if err := globalDatabase.ClearActiveSessions(); err != nil {
		log.Printf("failed to clear stale sessions: %v", err)
	}
	if err := initStoredFlows(); err != nil {
		log.Fatalf("failed to initialize flows: %v", err)
	}

	startFlowScheduler() // runs enabled flows on their cron schedule (flows_schedule.go)
	startChatWatcher()   // chat commands and Player message flows (flows_message.go)

	addr := environmentOrDefault("LISTEN_ADDR", ":8080")
	startDatabaseSync() // sessions and chat history from the console log (db.go)
	startServerWatch()  // keeps RCON logged in, polls players and bans (rcon.go)
	log.Printf("listening on %s -> RCON %s", addr, address)
	log.Fatal(runWebServer(addr)) // web.go
}

// ---------------------------------------------------------------------------
// CLI modes
// ---------------------------------------------------------------------------

// runCommandLine handles command-line arguments (anything after the program name):
//
//	kickall [reason...]   kick everyone, print a summary
//	-i                    interactive console
//	<anything else>       joined into one RCON command, run once, printed
//
// Exits with status 1 on failure.
func runCommandLine(args []string) {
	if args[0] == "-i" {
		runInteractiveConsole()
		return
	}
	l, err := dialPersistentConnection("cli", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var ok bool
	if args[0] == "kickall" {
		rep, err := kickAllPlayers(l, strings.Join(args[1:], " "))
		ok = printKickReport(rep, err)
	} else {
		out, acked, err := l.run(strings.Join(args, " "), 2*time.Second)
		ok = printCommandResult(out, acked, err)
	}
	l.close()
	if !ok {
		os.Exit(1)
	}
}

// runInteractiveConsole is the interactive REPL. It logs in once and keeps the session
// alive with the keepalive goroutine, so repeated commands don't pay the
// login round trip. Features:
//   - "exit" / "quit" leaves; blank lines are ignored
//   - "kickall [reason]" works the same as the CLI/HTTP version
//   - automatic reconnect: if the session is dead before a command, or a
//     command fails/gets no ack and the session no longer answers a probe,
//     it logs in again
func runInteractiveConsole() {
	fmt.Println("Interactive RCON console (logs in once and stays logged in). Type 'exit' to quit.")
	var l *persistentConnection
	defer func() {
		if l != nil {
			l.close()
		}
	}()

	// connect (re)establishes the session, closing any previous one first.
	// Returns false (after printing the error) if login fails.
	connect := func() bool {
		if l != nil {
			l.close()
			l = nil
		}
		nl, err := dialPersistentConnection("cli", true)
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
		if l == nil || l.isClosed() {
			fmt.Println("(not connected, logging in...)")
			if !connect() {
				continue
			}
		}
		var acked bool
		var err error
		if line == "kickall" || strings.HasPrefix(line, "kickall ") {
			var rep *kickReport
			rep, err = kickAllPlayers(l, strings.TrimPrefix(line, "kickall"))
			printKickReport(rep, err)
			acked = err == nil
		} else {
			var out string
			out, acked, err = l.run(line, 2*time.Second)
			printCommandResult(out, acked, err)
		}
		// A failed or unacknowledged command may mean the session silently
		// died (UDP has no disconnect signal). Probe it and re-login if needed.
		if (err != nil || !acked) && !l.isResponsive() {
			fmt.Println("(session lost, logging in again...)")
			connect()
		}
	}
}

// printKickReport prints a human-readable kickall summary. It returns false
// (so the caller can exit non-zero) only when there was an error.
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

// printCommandResult prints the outcome of a single command. It distinguishes three
// non-error cases so you can tell them apart in the terminal:
//   - output text            -> printed as-is
//   - acked but empty output -> "(ok, no output)"
//   - nothing at all         -> "(no acknowledgement from server)"
//
// Returns false on error so the caller can exit non-zero.
func printCommandResult(out string, acked bool, err error) bool {
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
