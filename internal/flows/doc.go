// Package flows runs flows: lists of steps built on the Flows page (send a
// message, wait, lock the server, kick the player, an If with Else ifs and
// an Else, a For each, and so on). Steps run top to bottom; only Wait takes
// time.
//
// Every flow has one trigger, each in its own file:
//
//	schedule  a cron schedule, while Enabled                       schedule.go
//	join      once for each player who finishes loading in         join.go
//	chat      once for each chat message a player sends            message.go
//	manual    only when Run is pressed                             manual.go
//
// and an optional delay before step 1. "The player" is whoever triggered a
// join or chat run; {player_name}, {player_id}, {player_ping}, {player_ip},
// {player_guid}, {player_role} and {message} are filled in from them.
// Schedule and Manual flows have no player, so they message everyone only.
//
// The package also has the built-in chat commands (!help, !shout, !whisper,
// !players, !vote, !code, !kick, !ban, !warning), in chatcommands.go.
package flows
