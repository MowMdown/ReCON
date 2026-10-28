package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
)

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
	l, err := game.Dial("cli", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var ok bool
	if args[0] == "kickall" {
		rep, err := battleye.KickAll(l, strings.Join(args[1:], " "))
		ok = printKickReport(rep, err)
	} else {
		out, acked, err := l.Run(strings.Join(args, " "), 2*time.Second)
		ok = printCommandResult(out, acked, err)
	}
	l.Close()
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
	var l *battleye.Session
	defer func() {
		if l != nil {
			l.Close()
		}
	}()

	// connect (re)establishes the session, closing any previous one first.
	// Returns false (after printing the error) if login fails.
	connect := func() bool {
		if l != nil {
			l.Close()
			l = nil
		}
		nl, err := game.Dial("cli", true)
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
		if l == nil || l.IsClosed() {
			fmt.Println("(not connected, logging in...)")
			if !connect() {
				continue
			}
		}
		var acked bool
		var err error
		if line == "kickall" || strings.HasPrefix(line, "kickall ") {
			var rep *battleye.KickReport
			rep, err = battleye.KickAll(l, strings.TrimPrefix(line, "kickall"))
			printKickReport(rep, err)
			acked = err == nil
		} else {
			var out string
			out, acked, err = l.Run(line, 2*time.Second)
			printCommandResult(out, acked, err)
		}
		// A failed or unacknowledged command may mean the session silently
		// died (UDP has no disconnect signal). Probe it and re-login if needed.
		if (err != nil || !acked) && !l.IsResponsive() {
			fmt.Println("(session lost, logging in again...)")
			connect()
		}
	}
}

// printKickReport prints a human-readable kickall summary. It returns false
// (so the caller can exit non-zero) only when there was an error.
func printKickReport(rep *battleye.KickReport, err error) bool {
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
