// Package steam finds players' SteamIDs and looks up their Steam profiles
// for the Details dialog's Steam Profile tab (from the SteamID64 stored in
// players.steam_id).
//
//	GET  /api/players/{guid}/steam             the profile (cached for steamCacheFor)
//	GET  /api/players/{guid}/steam?refresh=1   fetch now (at most every steamMinRefresh)
//	POST /api/players/{guid}/steam/search      find the SteamID (players without one)
//
// Finding the SteamID (this replaces the separate guidsearch tool): BattlEye
// only gives us a GUID, md5("BE" + SteamID64 as 8 little-endian bytes), which
// can't be reversed. So the search hashes every Steam account ID (0 to 2^32)
// until the GUID matches, working up from the oldest accounts and down from
// the newest at once (see steamSearchSplit). Old and new accounts turn up
// soonest; a full pass takes several minutes. It runs in the background, one
// pass at a time, and a pass looks for every GUID in players and bans that
// has no SteamID yet, since many GUIDs cost the same as one. Each SteamID is
// written to players and bans the moment it is found. A search asked for
// while a pass is running waits for the next pass. STEAM_SEARCH_CORES and
// STEAM_SEARCH_PRIORITY set how much CPU it uses (see steamSearchConfig;
// default 1 core, medium).
//
// When a player with no SteamID joins (PlayerJoined), the steam2guid
// table, if there is one, is read for them at once, in either mode.
// STEAM_SEARCH_MODE only decides when hashing starts for the rest:
//
//	manual  (the default) when the button is pressed.
//	auto    by itself, as a batch: 60 seconds after the first join the
//	        table couldn't answer, each further join adds 30 seconds, and
//	        it starts at once when 3 minutes have passed or 15 players are
//	        waiting, so a busy restart costs one pass, not one per player
//	        (steamAutoBatch).
//
// steam2guid tables (STEAM_TABLE_DIR, default the folder players.db is in,
// so /data): if a table built
// by the steam2guid tool is in that folder, each pass looks every GUID up in
// it first (a disk read, well under a millisecond for 4- and 8-byte tables)
// and only brute-forces what the table can't answer: account IDs at or above
// the table's MaxAccount. A table covering all 2^32 accounts means no
// hashing at all; a GUID it doesn't have isn't a Steam account's. No table,
// or one that is damaged or incomplete, means the full search as before. The
// table is opened fresh for every pass, so adding, rebuilding or removing it
// needs no restart. Only reading is built in; tables are made with steam2guid.
//
// Lookups need STEAM_API_KEY (free at steamcommunity.com/dev/apikey). Without
// it the tab still shows the SteamID and a link to the profile. Each lookup
// is five Web API calls, made at the same time:
//
//	ISteamUser/GetPlayerSummaries      name, avatar, online status, public or
//	                                   private; account age, country and last
//	                                   online only for public profiles
//	ISteamUser/GetPlayerBans           VAC bans, game bans, days since the
//	                                   last one, community and trade bans
//	IPlayerService/GetSteamLevel       Steam level
//	IPlayerService/GetOwnedGames       games owned, DayZ hours (if the game
//	                                   details are public)
//	ISteamUser/GetFriendList           friend count, and which friends have
//	                                   played here (if the list is public)
//
// Only the first two must succeed; the rest are left empty ("Hidden") when
// the player keeps them private or the call fails. Nothing is written to the
// database: profiles are kept in memory, so a restart starts fresh.
package steam
