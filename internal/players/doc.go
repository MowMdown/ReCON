// Package players tracks who is online and their sessions, from what
// BattlEye tells the web session (the console log, rcon.Events):
//
//	Player #3 Name (1.2.3.4:2304) connected        -> joining: number, name, IP, time
//	Player #3 Name - BE GUID: <guid>               -> GUID known: the session opens
//	Verified GUID (<guid>) of player #3 Name          (whichever line comes first)
//	`players` poll without "(Lobby)" in the name   -> finished loading
//	Player #3 Name (<guid>) has been kicked by ... -> the session will end as kicked
//	Player #3 Name disconnected                    -> the session closes
//	(Global) Name: text                            -> kept with the player for
//	                                                  this session (memory only)
//
// The database is only written when one of those happens, never for a player
// who simply stays online. Playtime counts from the connect line, so loading
// time is included.
//
// The tracker is ReCON's list of who is online (Online): flows, chat
// commands and the Players page all read it. A `players` poll (PollNow)
// fills in what the messages can't tell: every 5 seconds while the server
// answers (package watch), every 3 seconds while someone is still loading,
// and right before a For each runs. A poll also catches anything the console
// log missed.
//
// Server restarts: when the server stops answering, every session is closed
// at the time it last answered (ServerWentDown) but remembered, and so are
// the players who left in the restartLookback before it, unless banned. A
// player who comes back from the same IP within restartGrace of ReCON's
// next login keeps the same session, with the time they were away not
// counted. Back from another IP, they get a new session.
//
// The package also files chat lines into each player's chat history
// (StartChatRecorder) and hands joins and new sessions to hooks set at
// startup (SetHooks): the join flows and the SteamID lookup.
package players
