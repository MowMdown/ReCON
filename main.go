package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
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
//	db.go     SQLite storage and the poller that mirrors the server into it
//	flows.go  flow scripts, cron schedules, the runner and the scheduler
// ============================================================================

var (
	rconAddress    string    // "host:port" of the DayZ/BattlEye RCON endpoint, set in main()
	rconPassword   string    // RCON password, read from the RCON_PASSWORD env var
	globalDatabase *Database // player database, opened in main()
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// main wires everything together:
//  1. Reads config from the environment:
//     RCON_PASSWORD (required), RCON_HOST (default 127.0.0.1),
//     RCON_PORT (default 2306), DB_PATH (default /data/players.db),
//     LISTEN_ADDR (default :8080).
//  2. Opens the player database.
//  3. If command-line args were given, runs CLI mode and returns.
//  4. Otherwise runs as a server: clears stale sessions, prepares the flows
//     table, starts the background player sync and the flow scheduler, and
//     serves HTTP forever.
func main() {
	rconPassword = os.Getenv("RCON_PASSWORD")
	if rconPassword == "" {
		log.Fatal("RCON_PASSWORD is required")
	}
	rconAddress = net.JoinHostPort(environmentOrDefault("RCON_HOST", "127.0.0.1"), environmentOrDefault("RCON_PORT", "2306"))

	dbPath := environmentOrDefault("DB_PATH", "/data/players.db")
	var err error
	globalDatabase, err = OpenDatabase(dbPath)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}

	// CLI mode: `prog kickall ...`, `prog -i`, `prog players`, etc.
	if len(os.Args) > 1 {
		runCommandLine(os.Args[1:])
		return
	}

	// Drop sessions left open by a previous run so restart gaps aren't counted as playtime.
	if err := globalDatabase.ClearActiveSessions(); err != nil {
		log.Printf("failed to clear stale sessions: %v", err)
	}
	if err := initFlows(); err != nil {
		log.Fatalf("failed to initialize flows: %v", err)
	}

	startScheduler() // runs enabled flows on their cron schedule (flows.go)

	addr := environmentOrDefault("LISTEN_ADDR", ":8080")
	startBackgroundSync(5 * time.Second) // periodic player/ban polling (db.go)
	log.Printf("listening on %s -> RCON %s", addr, rconAddress)
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
		runConsole()
		return
	}
	l, err := dialPersistent("cli", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var ok bool
	if args[0] == "kickall" {
		rep, err := kickAll(l, strings.Join(args[1:], " "))
		ok = printKickReport(rep, err)
	} else {
		out, acked, err := l.run(strings.Join(args, " "), 2*time.Second)
		ok = printResult(out, acked, err)
	}
	l.close()
	if !ok {
		os.Exit(1)
	}
}

// runConsole is the interactive REPL. It logs in once and keeps the session
// alive with the keepalive goroutine, so repeated commands don't pay the
// login round trip. Features:
//   - "exit" / "quit" leaves; blank lines are ignored
//   - "kickall [reason]" works the same as the CLI/HTTP version
//   - automatic reconnect: if the session is dead before a command, or a
//     command fails/gets no ack and the session no longer answers a probe,
//     it logs in again
func runConsole() {
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
		nl, err := dialPersistent("console", true)
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
			rep, err = kickAll(l, strings.TrimPrefix(line, "kickall"))
			printKickReport(rep, err)
			acked = err == nil
		} else {
			var out string
			out, acked, err = l.run(line, 2*time.Second)
			printResult(out, acked, err)
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

// printResult prints the outcome of a single command. It distinguishes three
// non-error cases so you can tell them apart in the terminal:
//   - output text            -> printed as-is
//   - acked but empty output -> "(ok, no output)"
//   - nothing at all         -> "(no acknowledgement from server)"
//
// Returns false on error so the caller can exit non-zero.
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
