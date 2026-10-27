// Package bans keeps ReCON's ban list in step with BattlEye's: it reads the
// server's list (SyncWithServer) after every login and every SyncEvery,
// puts back permanent bans the server has lost, lets timed bans run out
// (ExpireDue), and places bans (BanGUID). Everything that changes or mirrors
// the ban list holds ListLock, so a sync never acts on a stale list.
package bans
