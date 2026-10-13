# DayZ ReCON

A small web server that manages a DayZ server over BattlEye RCON. It gives you:

- a **web GUI** for players, bans and scheduled flows (such as a restart countdown),
- an **HTTP API** that any tool or task scheduler can call,
- a **console** for running RCON commands from inside the container.

> **No authentication.** Anyone who can reach the port can run any RCON command. Keep it on a trusted local network and never expose it to the internet.

---

## 1. Build, install and run

### Requirements

- Docker with Docker Compose
- A DayZ server with BattlEye RCON turned on: `RConPassword` and `RConPort` set in `BEServer_x64.cfg`

### Folder layout

```text
dayz-recon/
├── docker-compose.yaml
├── .env
└── rcon-bridge/
    ├── Dockerfile
    ├── go.mod
    ├── main.go
    ├── rcon.go
    ├── web.go
    ├── db.go
    └── flows.go
```

### Configuration

Each container is configured with environment variables:

| Variable        | Default            | Description                                                              |
| --------------- | ------------------ | ------------------------------------------------------------------------ |
| `RCON_PASSWORD` | required           | BattlEye RCON password                                                   |
| `RCON_HOST`     | `127.0.0.1`        | IP or hostname of the DayZ server                                        |
| `RCON_PORT`     | `2306`             | BattlEye RCON UDP port (`RConPort` in `BEServer_x64.cfg`)                |
| `LISTEN_ADDR`   | `:8080`            | Address the web server listens on inside the container                  |
| `DB_PATH`       | `/data/players.db` | Where player, ban and flow data is kept                                  |
| `TZ`            | `UTC`              | Time zone for flow schedules, for example `America/New_York`             |

Mount `/data` as a volume, or your players, bans and flows are lost whenever the container is recreated.

Put the password in `.env`, next to `docker-compose.yaml`:

```env
RCON_PASSWORD=your-rcon-password
```

### docker-compose.yaml

```yaml
services:
  dayz-recon:
    container_name: dayz-recon
    build: ./rcon-bridge
    restart: unless-stopped
    environment:
      RCON_HOST: 192.168.1.10
      RCON_PORT: "2306"
      RCON_PASSWORD: ${RCON_PASSWORD}
      TZ: America/New_York
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
```

To manage more than one DayZ server, copy the service once per server. Give each copy its own service and container name, RCON port, password, data folder and host port.

### Build and start

From the folder that holds `docker-compose.yaml`:

```bash
docker compose up -d --build
```

Check that it connected:

```bash
docker logs -f dayz-recon
# listening on :8080 -> RCON 192.168.1.10:2306
```

Then open `http://<host-ip>:8080/players` in a browser.

### Updating after code changes

The Go code is compiled when the image is built, so a plain restart keeps the old version. Rebuild instead:

```bash
docker compose up -d --build
```

If it still seems to run old code, force a clean build:

```bash
docker compose build --no-cache && docker compose up -d
```

---

## 2. Using it

The examples below use `http://<host>:8080`.

### Web GUI

Use the links in the top bar to move between the three pages.

#### Players: `/players`

Shows the players online right now (ID, name, IP, GUID, ping), refreshed every 5 seconds. Each row has three buttons:

| Button  | What it does                                                                                     |
| ------- | ------------------------------------------------------------------------------------------------ |
| Message | Sends a private message to that player                                                           |
| Kick    | Kicks the player with the reason you type                                                        |
| Ban     | Bans the player's GUID for the minutes you enter (**0 = permanent**), then kicks them             |

Below that, **Offline Players** lists everyone who has played before, with their last IP, total time on the server and when they were last seen.

#### Server Bans: `/bans`

Lists the server's GUID bans with the player name, time left and reason, refreshed every 10 seconds. **Unban** removes the ban.

**Ban Player** (top right) bans anyone who has ever played on the server, online or not:

1. Pick the player by name. Online players are marked `(online)`, and the first 8 characters of each GUID tell apart players with the same name. Players who are already banned aren't listed.
2. Enter a short reason. This is the message the player sees.
3. Leave the duration at **0 for a permanent ban**, or enter a number of minutes.

The ban is placed on the player's GUID. If they're online, they're kicked right away.

#### Flows: `/flows`

A flow is a saved list of RCON commands that runs on a schedule (such as a restart countdown) or each time a player joins (such as a welcome message). A default "Server restart" flow is created on first start, disabled.

| Button           | What it does                                                                     |
| ---------------- | -------------------------------------------------------------------------------- |
| Enable / Disable | Turns the flow on or off                                                         |
| Run / Stop       | Starts a flow right now, or stops a running one                                  |
| Edit             | Changes the name, steps or schedule                                              |
| Delete           | Removes the flow (stop it first if it's running)                                 |
| New flow         | Creates a flow                                                                   |
| Help             | Opens the full guide to writing flows                                            |

The **#Lock**, **#Unlock**, **#Shutdown** and **#Kick All** buttons at the top right send that command immediately. Shutdown and Kick All ask you to confirm first. You can drag rows to reorder the list, and each flow's run log appears under the table.

**Writing steps.** Put one step per line, as `offset | command`. Offsets are measured from the scheduled time:

```text
-60m | say -1 Server is going to restart in $minutes minutes!
-5m  | say -1 Server is going to restart in $minutes minutes!
-3m  | #lock
-1m  | #kickall Restarting now
0m   | #shutdown
+90  | #unlock
cancel | say -1 Server restart cancelled.
```

- `-60m` runs 60 minutes **before** the scheduled time, `0m` runs **on** it, and `+90` runs 90 seconds **after** it. The units are `s`, `m` and `h`; with no unit the number is in seconds.
- `$minutes` and `$seconds` are replaced with how long before the scheduled time that step runs.
- `cancel | ...` lines run only if you press Stop before the scheduled time.
- `kickall [reason]` (or `#kickall`) kicks every connected player.
- Lines starting with `//` are comments.

**Schedule.** A schedule is a standard 5-field cron expression (`minute hour day-of-month month day-of-week`) in the container's `TZ`. As you type, the dialog explains the expression in plain English and shows the next run times.

| Cron          | Runs                       |
| ------------- | -------------------------- |
| `0 6 * * *`   | Every day at 06:00         |
| `0 */6 * * *` | Every 6 hours              |
| `30 4 * * 1,4`| Mondays and Thursdays 04:30 |
| `@daily`      | Every day at midnight      |

**Player-join flows.** Leave the schedule blank (or enter `* * * * *`) and press **Enable**. The flow then runs once for every player who joins, instead of on the clock:

```text
Flow name: Welcome player!
Schedule:  * * * * *
Steps:     0s | say $player Welcome to our server!
```

- `$player` becomes the joining player's number, which is what `say` and `kick` need. `$name` becomes their name.
- Offsets count from when the join is seen (joins are checked every 5 seconds), so use `0s` or `+` offsets only.
- A player still loading in can miss a `0s` message. Use `+30` or so if messages go unseen.
- Players already online when the container starts aren't greeted. After a server restart, everyone who reconnects is.
- A flow that uses `$player` or `$name` can't be started with Run or given a clock schedule.
- A flow with a blank schedule that stays disabled can still be started by hand with Run.

### Console

The container includes a command-line RCON tool called `rcon`.

Run one command:

```bash
docker exec dayz-recon rcon players
docker exec dayz-recon rcon say -1 "Hello everyone"
docker exec dayz-recon rcon "#lock"
docker exec dayz-recon rcon kickall "Server restarting"
```

Interactive mode stays logged in until you type `exit`:

```bash
docker exec -it dayz-recon rcon -i
rcon> players
rcon> say -1 Restart in 5 minutes
rcon> kickall Restarting now
rcon> exit
```

Tips:

- Quote anything starting with `#` (`"#lock"`), or the shell treats it as a comment.
- Use single quotes around messages that contain `!`.
- `kickall` without a reason kicks players with no reason text.

### API

Every response is JSON. Failed requests return `{"ok": false, "error": "..."}`.

#### Run any RCON command: `/rcon`

```bash
# GET (URL-encode the command)
curl "http://<host>:8080/rcon?cmd=players"

# POST with JSON
curl -X POST http://<host>:8080/rcon \
  -H "Content-Type: application/json" \
  -d '{"command":"say -1 Restart in 5 minutes"}'

# POST with the command as plain text
curl -X POST http://<host>:8080/rcon -d '#lock'
```

Response:

```json
{"ok": true, "command": "players", "acked": true, "output": "Players on server: ..."}
```

`acked: false` means the server never confirmed it received the command, so the result is uncertain.

#### Kick everyone: `POST /kickall`

The JSON body is optional. Every field has a default:

| Field      | Default             | Description                                      |
| ---------- | ------------------- | ------------------------------------------------ |
| `reason`   | `Server restarting` | Kick reason                                      |
| `lock`     | `false`             | Send `#lock` first so nobody can rejoin          |
| `shutdown` | `false`             | Send `#shutdown` after the kicks                 |

```bash
curl -X POST http://<host>:8080/kickall \
  -H "Content-Type: application/json" \
  -d '{"reason":"Restarting now","lock":true}'
```

The response lists who was kicked (`order`) and who is still connected (`remaining`). `lock` does not unlock afterwards, so send `#unlock` yourself, or use a flow with a `+90 | #unlock` step.

#### Players

| Method | Path                         | Body                                   | Description                         |
| ------ | ---------------------------- | -------------------------------------- | ----------------------------------- |
| GET    | `/api/players`               |                                        | Online and offline players          |
| GET    | `/api/players/all`           |                                        | Every player ever seen, from the saved player list (works while RCON is down) |
| POST   | `/api/players/<id>/message`  | `{"message": "text"}`                  | Private message to one player       |
| POST   | `/api/players/<id>/kick`     | `{"reason": "text"}`                   | Kick one player                     |
| POST   | `/api/players/<id>/ban`      | `{"reason": "text", "minutes": 0}`     | Ban by GUID (0 = permanent) and kick |

`<id>` is the player number shown in `/players` or by the `players` command.

#### Bans

| Method | Path                       | Description                        |
| ------ | -------------------------- | ---------------------------------- |
| GET    | `/api/bans`                | List the GUID bans                 |
| POST   | `/api/bans/<guid>/ban`     | Ban a known player by GUID, online or not. Optional body `{"reason": "text", "minutes": 0}`; `minutes` defaults to 0 (permanent). Online players are kicked too |
| POST   | `/api/bans/<guid>/unban`   | Remove every ban for that GUID     |

#### Flows

| Method | Path                         | Body                                         | Description                                         |
| ------ | ---------------------------- | -------------------------------------------- | --------------------------------------------------- |
| GET    | `/api/flows`                 |                                              | All flows, with their next run and latest run log   |
| POST   | `/api/flows`                 | `{"id", "name", "script", "schedule", "scheduleEnabled"}` | Create (`id` 0) or update a flow       |
| POST   | `/api/flows/<id>/run`        | optional `{"minutes": N}`                    | Run now; with `minutes`, the scheduled time is N minutes from now and earlier steps are skipped |
| POST   | `/api/flows/<id>/stop`       |                                              | Stop a running flow                                 |
| POST   | `/api/flows/<id>/enable`     |                                              | Turn the schedule on                                |
| POST   | `/api/flows/<id>/disable`    |                                              | Turn the schedule off                               |
| POST   | `/api/flows/<id>/delete`     |                                              | Delete a flow                                       |
| POST   | `/api/flows/command`         | `{"command": "lock"}`                        | Quick command: `lock`, `unlock`, `shutdown` or `kickall` |

Starting a 10-minute restart countdown from a script or task scheduler:

```bash
curl -X POST http://<host>:8080/api/flows/1/run \
  -H "Content-Type: application/json" \
  -d '{"minutes":10}'
```

#### Health check

`GET /health` returns `{"ok": true}`.

#### Calling it from a task scheduler

Tools with a "POST to a URI" action can call any of the endpoints above. For example:

| Field        | Value                                            |
| ------------ | ------------------------------------------------ |
| URI          | `http://<host>:8080/kickall`                     |
| Payload      | `{"reason":"Restarting now","lock":true}`        |
| Content Type | `application/json`                               |

For a full restart sequence, a flow is usually simpler: schedule it on the Flows page, or trigger it with `POST /api/flows/<id>/run`.

### Common RCON commands

| Command                            | Effect                                  |
| ---------------------------------- | --------------------------------------- |
| `players`                          | List connected players                  |
| `say -1 <message>`                 | Message everyone                        |
| `say <player#> <message>`          | Message one player                      |
| `kick <player#> <reason>`          | Kick one player                         |
| `bans`                             | List bans                               |
| `addBan <guid> <minutes> <reason>` | Ban a GUID (0 = permanent)              |
| `removeBan <#>`                    | Remove a ban by its number              |
| `#lock` / `#unlock`                | Block or allow new connections          |
| `#shutdown`                        | Stop the server                         |
