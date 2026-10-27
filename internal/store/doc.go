// Package store is the SQLite player database. Every SQL statement in ReCON
// lives here:
//
//   - schema setup and upgrades (OpenPlayerDatabase, EnsureFlowsTable)
//   - players, sessions and IP history (OpenPlayerSession, ClosePlayerSession, ...)
//   - player details and admin notes (GetPlayerDetails, SavePlayerNote)
//   - chat history (AddChatMessage, GetChatHistory)
//   - SteamIDs (SteamIDByGUID, PlayersBySteamIDs, ListPendingSteamGUIDs, SetSteamID)
//   - roles for the chat commands (GetPlayerRole, SetPlayerRole, ...)
//   - settings, such as the chat commands' (GetStoredSetting, SaveStoredSetting)
//   - bans: permanent ones in the bans table, timed ones in memory
//     (RecordPlayerBan, SyncBansFromServer, ExpireTimedBans, ListAllBans, ...)
//   - flows (ListStoredFlows, GetStoredFlow, SaveStoredFlow, ...), as
//     FlowRecords with the steps as JSON
package store
