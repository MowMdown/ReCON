// Package rcon is ReCON's own RCON session (the web session), shared by the
// pages, flows and polls, and the master console log.
//
//   - Settings: where the game server is (RCON_HOST, RCON_PORT,
//     RCON_PASSWORD), overridden in memory by RCON Settings.
//   - The web session: one logged-in battleye.Session. Login and Relogin are
//     called only by the server watch (package watch); Run sends a command
//     and logs it, and its reply, to the console log and the container log
//     as [web]. OnLogin hooks run after every login, which also sends
//     #unlock.
//   - The console log (Events): every server message and every command
//     ReCON sends with its reply, up to consoleLogLimit lines. Polls and
//     heartbeats aren't in it. The player tracker, chat history and flows
//     all work from it.
//   - Status: what the server watch last saw, for the status bar.
package rcon
