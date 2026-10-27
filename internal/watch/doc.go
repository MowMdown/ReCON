// Package watch is the server watch: every 5 seconds it polls `players`
// while the server answers (who is online, pings), and every 30 seconds it
// sends the heartbeat, an empty command that keeps the web session alive.
// The heartbeat is the only thing that logs in, at most once per heartbeat:
// on a new socket only when the old one has died, otherwise again on the
// same socket, so BattlEye keeps the same admin slot. Three checks in a row
// without an answer mark the server down. After every login it reads the
// ban list. See docs/rcon-login.md.
package watch
