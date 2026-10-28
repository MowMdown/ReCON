// Command recon is a web panel and command-line tool for a DayZ server's
// BattlEye RCON.
//
// With no arguments it serves the web GUI (LISTEN_ADDR, default :8080). With
// arguments it is a command-line RCON tool:
//
//	recon players           run one command and print the reply
//	recon kickall [reason]  kick everyone
//	recon -i                interactive console
//
// Settings come from the environment: RCON_HOST, RCON_PORT, RCON_PASSWORD,
// DB_PATH, LISTEN_ADDR, TZ and the STEAM_* variables (see the README).
package main

import (
	"log"
	"os"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/web"
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
//  1. Opens the player database (DB_PATH, default /data/players.db) and
//     hands it to every package that uses it.
//  2. Works out where the game server's RCON is: the environment's
//     RCON_HOST, RCON_PORT and RCON_PASSWORD (game.Settings).
//  3. If command-line args were given, runs CLI mode and returns.
//  4. Otherwise runs as a server: clears stale sessions, prepares the flows
//     table, starts the player tracker, the chat recorder, the flow
//     scheduler and the server watch, and serves HTTP forever.
func main() {
	db, err := store.OpenPlayerDatabase(environmentOrDefault("DB_PATH", "/data/players.db"))
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	game.SetDatabase(db)
	flows.SetDatabase(db)
	steam.SetDatabase(db)
	web.SetDatabase(db)

	address, password := game.Settings{}.Effective()
	game.SetTarget(address, password)

	// CLI mode: `prog kickall ...`, `prog -i`, `prog players`, etc.
	if len(os.Args) > 1 {
		if password == "" {
			log.Fatal(battleye.ErrNoPassword)
		}
		runCommandLine(os.Args[1:])
		return
	}
	// The web pages still start without a password, so one can be entered
	// in RCON Settings; until then every RCON call fails with ErrNoPassword.
	if password == "" {
		log.Printf("rcon: %v", battleye.ErrNoPassword)
	}

	// Drop sessions left open by a previous run so restart gaps aren't counted as playtime.
	if err := db.ClearActiveSessions(); err != nil {
		log.Printf("failed to clear stale sessions: %v", err)
	}
	if err := flows.Init(); err != nil {
		log.Fatalf("failed to initialize flows: %v", err)
	}
	flows.StartScheduler()   // runs enabled flows on their cron schedule
	flows.StartChatWatcher() // chat commands and Player message flows

	// The player tracker hands players who finished joining to the join
	// flows, and every new session to the SteamID lookup.
	game.SetPlayerHooks(flows.StartJoinFlows, steam.PlayerJoined)
	game.StartChatRecorder() // chat history from the console log
	game.StartTracker()      // sessions from the console log
	game.RefreshBanExpiry()  // when the next timed ban runs out
	game.StartWatch()        // keeps RCON logged in, polls players and bans

	addr := environmentOrDefault("LISTEN_ADDR", ":8080")
	log.Printf("listening on %s -> RCON %s", addr, address)
	log.Fatal(web.Serve(addr))
}
