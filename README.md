# DayZ RCON Bridge

A small Go web server for a DayZ server. It turns HTTP requests into BattlEye RCON commands, serves two web pages for managing players and bans, and records player activity (names, IPs, playtime, bans) in a local SQLite database.

Base URL in this setup: `http://<server-ip>:8080`.

> **No authentication.** Any client that can reach the port can run any RCON command. Run it on a trusted local network only and never expose it to the internet.

---

## What it does

* **Web pages:** `/players` (live players with Message / Kick / Ban buttons) and `/bans` (the BattlEye ban list with an Unban button).
* **HTTP API:** run any RCON command (`/rcon`), kick everyone (`/kickall`), and the JSON endpoints the pages use.
* **Background tracking:** polls the server every 5 seconds and records players, IPs and playtime in SQLite, whether or not any page is open.
* **Console:** the same binary works as a command-line RCON tool inside the container.
* **Logging:** every RCON command and every non-polling HTTP request is written to the container logs.

---

## Configuration

Set through environment variables on the container.

| Variable        | Default           | Description                                               |
| --------------- | ----------------- | --------------------------------------------------------- |
| `RCON_HOST`     | `127.0.0.1`       | Host or IP of the DayZ server                             |
| `RCON_PORT`     | `2306`            | BattlEye RCON UDP port (`RConPort` in `BEServer_x64.cfg`) |
| `RCON_PASSWORD` | required          | BattlEye RCON password                                    |
| `LISTEN_ADDR`   | `:8080`           | Address the HTTP server listens on                        |
| `DB_PATH`       | `/data/players.db`| SQLite database file                                      |

There is no command allowlist. Every RCON command is permitted.

Mount `/data` as a volume or the database is lost whenever the container is recreated. Example compose entry:

```yaml
    volumes:
      - ./data:/data
```

---

## Web pages

### `/players`: DayZ Players

Live table of connected players: ID, name, IP, GUID and ping, refreshed every 5 seconds.

| Button  | What it does                                                                                           |
| ------- | ------------------------------------------------------------------------------------------------------ |
| Message | `say <id> <message>` to that player only                                                               |
| Kick    | `kick <id> <reason>`                                                                                   |
| Ban     | `addBan <guid> <minutes> <reason>` (0 minutes = permanent), then kicks the player with the same reason, then records the ban in the database |

BattlEye bans are by GUID only. The player's IP is saved in the database for reference but is not banned. If the kick after a ban is not confirmed, the page tells you; the ban is already in place.

### `/bans`: Server Bans

The BattlEye GUID ban list (IP bans are ignored), refreshed every 10 seconds, with the player name, GUID, time left, reason and an **Unban** button.

Unban sends `removeBan <#>` using the ban's current number, confirms the GUID is gone, saves the ban list (`writeBans`), and then deletes the row from the database. BattlEye can list the same GUID more than once, and the numbers shift after each removal, so the bridge removes one entry at a time from a fresh list until none are left.

---

## Endpoints

### `GET /health`

Returns `{"ok": true}`. Use it for uptime checks.

### `GET /rcon?cmd=<command>`

Runs one RCON command. URL-encode the command.

```bash
curl "http://<host>:8080/rcon?cmd=players"
```

### `POST /rcon`

Runs one RCON command.

* With `Content-Type: application/json`, the body is `{"command": "<command>"}`.
* With any other content type, the whole body is the command.

```bash
curl -X POST http://<host>:8080/rcon \
  -H "Content-Type: application/json" \
  -d '{"command":"say -1 Restart in 5 minutes"}'
```

Response:

```json
{"ok": true, "command": "players", "acked": true, "output": "Players on server:\n..."}
```

| Field     | Meaning                                                                                                          |
| --------- | ---------------------------------------------------------------------------------------------------------------- |
| `ok`      | The request reached RCON and finished without error                                                              |
| `command` | The command that was sent                                                                                        |
| `acked`   | The server acknowledged the command packet. If `false`, the reply may have been lost, so the result is uncertain |
| `output`  | Text the server returned. Empty for commands with no output, such as `say`                                       |

### `POST /kickall`

Lists the connected players, then kicks them one by one from the **highest player number down to 0**, over a single RCON session. This is not a BattlEye command; it is a routine built from `players` and `kick`. POST only.

Body (optional JSON):

| Field      | Default             | Description                                             |
| ---------- | ------------------- | ------------------------------------------------------- |
| `reason`   | `Server restarting` | Kick reason sent with every `kick`                      |
| `lock`     | `false`             | Send `#lock` first so nobody can rejoin during the kick |
| `shutdown` | `false`             | Send `#shutdown` after the kicks                        |

```bash
curl -X POST http://<host>:8080/kickall \
  -H "Content-Type: application/json" \
  -d '{"reason":"Server is going down for a restart NOW!!!","lock":true}'
```

Response:

```json
{"ok": true, "result": {"order": [3,2,1,0], "unacked": [], "remaining": [], "shutdown_sent": false, "shutdown_acked": false}}
```

| Field            | Meaning                                                                                      |
| ---------------- | -------------------------------------------------------------------------------------------- |
| `order`          | Player numbers, in the order the kicks were sent                                             |
| `unacked`        | Kicks the server did not acknowledge                                                         |
| `remaining`      | Players still connected after a final `players` check. Should be empty; if not, run it again |
| `raw`            | Only present when no players were parsed: the raw `players` output, so you can see why       |
| `shutdown_sent`  | `#shutdown` was sent                                                                         |
| `shutdown_acked` | The server acknowledged `#shutdown`                                                          |
| `shutdown_error` | Only present if sending `#shutdown` failed                                                   |

`lock` does not unlock afterwards. Send `{"command":"#unlock"}` yourself, or restart the server.

### Player API (used by `/players`)

| Method | Path                             | Body                                           | Description                                                  |
| ------ | -------------------------------- | ---------------------------------------------- | ------------------------------------------------------------ |
| GET    | `/api/players`                   |                                                | Connected players: `{"ok", "connected", "players": [...], "count"}` |
| POST   | `/api/players/<id>/message`      | `{"message": "text"}`                          | Private `say` to one player                                  |
| POST   | `/api/players/<id>/kick`         | `{"reason": "text"}`                           | Kick one player                                              |
| POST   | `/api/players/<id>/ban`          | `{"reason": "text", "minutes": 0}`             | Ban by GUID, kick, and record in the database                |

Each player object is `{"id", "ip", "port", "ping", "guid", "name"}`. Kick and ban look the player up again first, so they fail with 404 if the player has already left. The ban response includes `guid_ban`, `db_updated`, `kicked` and `perm`.

### Ban API (used by `/bans`)

| Method | Path                         | Description                                                                 |
| ------ | ---------------------------- | --------------------------------------------------------------------------- |
| GET    | `/api/bans`                  | Live BattlEye GUID bans, synced into the database: `{"ok", "bans": [...], "count"}` |
| POST   | `/api/bans/<guid>/unban`     | Remove every entry for that GUID from BattlEye and the database             |

Each ban object is `{"index", "guid", "name", "minutes", "perm", "reason"}`. The unban response is `{"ok", "be_removed", "db_removed", "removed", "saved"}`. If the GUID is not in BattlEye any more, the database row is simply removed.

---

## Errors

All errors are JSON: `{"ok": false, "error": "<message>"}`.

| Status | Meaning                                                                          |
| ------ | -------------------------------------------------------------------------------- |
| 400    | No command, bad JSON, missing message, or unreadable body                        |
| 404    | Unknown path or action, or the player is no longer connected                     |
| 405    | Wrong HTTP method (for example `/kickall` is POST only)                          |
| 500    | The BattlEye action worked but the database update failed                        |
| 502    | RCON problem: login rejected, no reply from the server, or a socket error        |
| 503    | The web pages' shared RCON connection is unavailable; it reconnects by itself    |

Unknown paths outside the API return the standard plain-text `404 page not found`.

---

## Useful commands

| Command                              | Effect                                                              |
| ------------------------------------ | ------------------------------------------------------------------- |
| `players`                            | List connected players                                              |
| `say -1 <message>`                   | Broadcast a message to everyone                                     |
| `say <player#> <message>`            | Message one player                                                  |
| `kick <player#> <reason>`            | Kick one player                                                     |
| `bans`                               | List GUID and IP bans with their numbers                            |
| `addBan <guid> <minutes> <reason>`   | Ban a GUID (0 minutes = permanent)                                  |
| `removeBan <#>`                      | Remove a ban by its number (numbers shift after each removal)       |
| `writeBans`                          | Save the ban list to disk                                           |
| `#lock` / `#unlock`                  | Block or allow new connections                                      |
| `#shutdown`                          | Stop the server (only restarts if something supervises the process) |

`players` rows look like this:

```text
Players on server:

[#] [IP Address]:[Port] [Ping] [GUID] [Name]
0   10.0.1.51:48796       69   7a6sd5f876asdf567asd8765fd897sdf(?)  Player_Name
```

`bans` output looks like this (only the GUID section is used):

```text
GUID Bans:
[#] [GUID] [Minutes left] [Reason]
----------------------------------------
0  49aacf68461f67163d0a2ecb6a4cee94 perm testing

IP Bans:
[#] [IP Address] [Minutes left] [Reason]
----------------------------------------------
```

---

## Container console

The image includes a small shell, and the binary doubles as a console tool, available as `rcon` or `rcon-bridge`.

```sh
rcon players
rcon say -1 'Hello everyone'
rcon kickall 'Server is going down for a restart NOW!!!'
rcon "#lock"
rcon -i                  # interactive mode: type commands, 'exit' to quit
```

From the terminal:

```bash
docker exec dayz-rcon-bridge rcon players
```

Notes:

* Quote a leading `#` (`rcon "#lock"`) or the shell treats it as a comment. Use single quotes around messages containing `!`.
* The console uses the container's own `RCON_*` variables.
* `rcon kickall` with no reason sends the kicks with no reason text. The HTTP endpoint defaults to `Server restarting`.
* Interactive mode logs in once, sends a keepalive every 30 seconds, and logs in again if the session is lost.

---

## Scheduler task form

| Field                | Value                                                                |
| -------------------- | -------------------------------------------------------------------- |
| URI                  | `http://<server-ip>:8080/kickall`                                    |
| Payload              | `{"reason":"Server is going down for a restart NOW!!!","lock":true}` |
| Headers              | blank                                                                |
| Authorization Header | blank                                                                |
| Content Type         | `application/json`                                                   |

Each task sends one request. For a restart sequence, schedule separate tasks, for example a `say -1` warning, then `/kickall` a few minutes later.

---

## Database

SQLite in WAL mode at `DB_PATH` (default `/data/players.db`). It can be read while the bridge is running.

### How it is filled

* **Every 5 seconds:** a background poller sends `players` and updates the player tables, even with no page open. A player who joins and leaves between two polls is not recorded, so playtime is accurate to about 5 seconds.
* **Every 10th poll (about 50 seconds), and whenever `/bans` is open:** the ban list is read from BattlEye and mirrored into the `bans` table. Missing bans are added, reasons are refreshed, and rows for bans that no longer exist in BattlEye (expired or removed) are deleted.
* **On Ban and Unban:** the ban row is written or removed immediately.
* **On startup:** open sessions left over from a previous run are cleared so downtime is not counted as playtime.
* **If RCON stops answering:** after 3 failed polls in a row, open sessions are closed once so playtime stops accruing.

### Tables

| Table             | Contents                                                                      |
| ----------------- | ----------------------------------------------------------------------------- |
| `players`         | One row per GUID: latest name and IP, first and last seen, total playtime in seconds |
| `active_sessions` | Players online right now                                                      |
| `session_history` | One row per finished visit: IP, connect and disconnect time, duration         |
| `player_ips`      | Every IP a player has connected from, with first and last time seen           |
| `bans`            | Mirror of the BattlEye GUID ban list: name, IP at ban time, reason, permanent flag |

Total playtime only increases when a player disconnects, so a player who is online right now is not counted until they leave.

### Views with friendlier column names

Read-only views for browsing. The tables above are what the bridge writes to.

| View              | Replaces          | Columns                                                                                           |
| ----------------- | ----------------- | ------------------------------------------------------------------------------------------------- |
| `view_players`    | `players`         | player_name, player_guid, last_ip_address, first_joined, last_seen, playtime_seconds, playtime    |
| `view_online`     | `active_sessions` | player_name, player_guid, ip_address, connected_at                                                |
| `view_sessions`   | `session_history` | session_id, player_name, player_guid, ip_address, connected_at, disconnected_at, duration_seconds, duration |
| `view_player_ips` | `player_ips`      | player_name, player_guid, ip_address, first_used, last_used                                       |
| `view_bans`       | `bans`            | player_name, player_guid, ip_address_when_banned, ban_reason, ban_type                            |

`playtime` and `duration` are readable text such as `1h 30m`. `ban_type` is `Permanent` or `Timed`.

---

## Viewing the data with sqlite3

Open the database from the host (against the mounted volume) or from anywhere `sqlite3` is installed:

```bash
sqlite3 -header -column /data/players.db
```

Or run one query straight from the shell:

```bash
sqlite3 -header -column /data/players.db "SELECT * FROM view_players;"
```

Inside the `sqlite3` prompt, turn on headers and columns once:

```
.headers on
.mode column
```

If a column is cut off or the output is cramped, set widths in column order: `.width 14 34 22 24 10`.

If the container has no `sqlite3`, run it from the host against the mounted volume, or copy out `players.db`, `players.db-wal` and `players.db-shm` together and query the copy.

### Who is online and who has played

```sql
-- Online right now
SELECT * FROM view_online;

-- Everyone, most playtime first
SELECT player_name, playtime, last_seen FROM view_players ORDER BY playtime_seconds DESC;

-- Top 10 by hours played
SELECT player_name, ROUND(playtime_seconds / 3600.0, 1) AS hours
FROM view_players ORDER BY hours DESC LIMIT 10;

-- Find one player by name
SELECT * FROM view_players WHERE player_name LIKE '%somename%';

-- Most recently seen
SELECT player_name, last_seen FROM view_players ORDER BY last_seen DESC LIMIT 10;
```

### IP history

```sql
-- Every IP one player has used, newest first
SELECT * FROM view_player_ips
WHERE player_name LIKE '%somename%' ORDER BY last_used DESC;

-- IPs used by more than one player (possible shared connections or alt accounts)
SELECT ip_address,
       COUNT(DISTINCT player_guid) AS players,
       group_concat(DISTINCT player_name) AS names
FROM view_player_ips
GROUP BY ip_address
HAVING players > 1;
```

### Sessions

```sql
-- Latest 20 visits
SELECT * FROM view_sessions ORDER BY session_id DESC LIMIT 20;

-- One player's visits
SELECT * FROM view_sessions
WHERE player_name LIKE '%somename%' ORDER BY session_id DESC;

-- Visits and total time per player
SELECT player_name, COUNT(*) AS visits, SUM(duration_seconds) AS seconds
FROM view_sessions GROUP BY player_guid ORDER BY seconds DESC;
```

### Bans

```sql
-- All bans
SELECT * FROM view_bans;

-- Permanent bans only
SELECT * FROM view_bans WHERE ban_type = 'Permanent';

-- Find a ban by name
SELECT * FROM view_bans WHERE player_name LIKE '%somename%';
```

A row in the `bans` table is only a record. The ban itself lives in BattlEye, so deleting a row from the database does not unban anyone. Use the Unban button (or `removeBan`) instead.

### Is it writing?

```sql
-- last_seen should advance every few seconds while a player is online
SELECT player_name, last_seen FROM view_players ORDER BY last_seen DESC LIMIT 3;

-- Row counts per table
SELECT (SELECT COUNT(*) FROM players)         AS players,
       (SELECT COUNT(*) FROM player_ips)      AS ip_rows,
       (SELECT COUNT(*) FROM session_history) AS sessions,
       (SELECT COUNT(*) FROM bans)            AS bans;
```

From the host you can also watch the files change. Writes land in `players.db-wal` first, so check that one too:

```bash
watch -n1 'ls -l --time-style=full-iso /data/players.db*'
```

### Schema and housekeeping

```
.tables              -- list tables and views
.schema players      -- show a table's definition
.quit
```

Back up with `sqlite3 /data/players.db ".backup /data/players-backup.db"`. That is safe while the bridge is running.

---

## Logging

Everything goes to the container log (`docker logs -f dayz-rcon-bridge`).

RCON commands are logged once each after they finish, with the source and whether the server acknowledged them:

```text
rcon[web] > "kick 3 cheating" (acked=true err=<nil>)
rcon[web] > "addBan 49aacf68... 0 testing" (acked=true err=<nil>)
rcon[http-kickall] > "players" (acked=true err=<nil>)
rcon[cli] > "kickall" (acked=true err=<nil>)
rcon[console] > "say -1 hello" (acked=true err=<nil>)
```

HTTP requests are logged with the caller's address (and `X-Forwarded-For` if a proxy sets it) and user agent:

```text
http POST /kickall from 192.168.1.20:51234 ua="curl/8.5.0"
http POST /api/players/3/ban from 192.168.1.50:60112 ua="Mozilla/5.0 ..."
```

| Source          | Meaning                                                          |
| --------------- | ---------------------------------------------------------------- |
| `web`           | The web pages and the background poller                          |
| `http`          | A direct `/rcon` request                                         |
| `http-kickall`  | A `/kickall` request                                             |
| `console`       | The interactive `rcon -i` session                                |
| `cli`           | A one-off `rcon <command>` run                                   |

To keep the log readable, the poller's routine `players` and `bans` reads, empty keepalive probes, and the pages' background polling (`/api/players`, `/api/bans`, `/players`, `/bans`, `/health`) are not logged. If something kicks players unexpectedly, find the `http POST /kickall from ...` line and check the address. If there is no HTTP line, the trigger was the CLI or the container's command arguments.

---

## Behaviour notes

* **Two kinds of RCON connection:** `/rcon`, `/kickall` and the CLI open a fresh UDP session per request and close it afterwards (`/kickall` uses one session for all its commands). The web pages and the background poller share one long-lived session that is created on demand and replaced if it dies.
* **Retries:** login is retried up to 3 times (1 second each) because UDP packets can be lost. Commands that change things (`kick`, `say`, `addBan`, `removeBan`) are never retried, so a lost reply can't make them run twice. The shared connection only retries read-only `players` and `bans` lookups.
* **Timing:** commands wait up to 2 seconds for a reply (`bans` up to 4). Login can take up to about 3 seconds when packets are lost. `/kickall` can take longer with many players; the HTTP server allows up to 120 seconds per request.
* **Ban list safety:** banning, unbanning and the ban-list sync share a lock, so a sync can never delete a ban that was just added. If the `bans` output isn't recognised, the sync does nothing instead of treating it as an empty list.
* **Server messages:** BattlEye pushes server console messages to connected RCON clients; the bridge acknowledges them.

---

## Rebuilding after code changes

Compose does not rebuild an existing image on its own. From the project folder:

```bash
docker compose up -d --build rcon-bridge
```

If the build seems to use old code, force a clean build with `docker compose build --no-cache rcon-bridge`. After upgrading from an older version, the database picks up new tables and views on startup. If your existing `players` table still has a `last_name` column, rename it once with the bridge stopped:

```bash
sqlite3 /data/players.db "ALTER TABLE players RENAME COLUMN last_name TO username;"
```