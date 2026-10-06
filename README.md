# DayZ RCON Bridge — API Guide

A small Go web server that turns HTTP requests into BattlEye RCON commands for a DayZ server. It keeps no state: every request logs into RCON, runs its command, and disconnects.

Base URL in this setup: `http://<server-ip>:8080`.

---

## Configuration

Set through environment variables on the container.

| Variable | Default | Description |
| --- | --- | --- |
| `RCON_HOST` | `127.0.0.1` | Host or IP of the DayZ server |
| `RCON_PORT` | `2305` | BattlEye RCON UDP port (`RConPort` in `BEServer_x64.cfg`) |
| `RCON_PASSWORD` | required | BattlEye RCON password |
| `API_TOKEN` | required (server mode) | Token callers must send |
| `LISTEN_ADDR` | `:8080` | Address the HTTP server listens on |
| `ALLOWED_PREFIXES` | empty (allow all) | Comma-separated command prefixes the HTTP API may run, e.g. `say,players,#lock,#unlock,kick,#shutdown` |

`ALLOWED_PREFIXES` is case-insensitive and matches the start of the command. It applies to the HTTP API only, not to the console commands.

---

## Authentication

Every endpoint except `/health` needs the token, in either header:

```
Authorization: Bearer <token>
X-API-Token: <token>
```

The `Bearer ` prefix is optional, so a bare token in `Authorization` also works. A missing or wrong token returns `401`.

---

## Endpoints

### `GET /health`

No auth. Returns `{"ok": true}`. Use it for uptime checks.

### `GET /rcon?cmd=<command>`

Runs one RCON command. URL-encode the command.

```bash
curl "http://<host>:8080/rcon?cmd=players" -H "X-API-Token: $TOKEN"
```

### `POST /rcon`

Runs one RCON command.

- With `Content-Type: application/json`, the body is `{"command": "<command>"}`.
- With any other content type, the whole body is the command.

```bash
curl -X POST http://<host>:8080/rcon \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"command":"say -1 Restart in 5 minutes"}'
```

Response:

```json
{"ok": true, "command": "players", "acked": true, "output": "Players on server:\n..."}
```

| Field | Meaning |
| --- | --- |
| `ok` | The request reached RCON and finished without error |
| `command` | The command that was sent |
| `acked` | The server acknowledged the command packet. If `false`, the reply may have been lost, so the result is uncertain |
| `output` | Text the server returned. Empty for commands with no output, such as `say` |

### `POST /kickall`

Lists the connected players, then kicks them one by one from the **highest player number down to 0**, over a single RCON session. This is not a BattlEye command; it is a routine built from `players` and `kick`. POST only.

Body (optional JSON):

| Field | Default | Description |
| --- | --- | --- |
| `reason` | `Server restarting` | Kick reason sent with every `kick` |
| `lock` | `false` | Send `#lock` first so nobody can rejoin during the kick |

```bash
curl -X POST http://<host>:8080/kickall \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"Server is going down for a restart NOW!!!","lock":true}'
```

Response:

```json
{"ok": true, "result": {"order": [3,2,1,0], "unacked": [], "remaining": []}}
```

| Field | Meaning |
| --- | --- |
| `order` | Player numbers, in the order the kicks were sent |
| `unacked` | Kicks the server did not acknowledge |
| `remaining` | Players still connected after a final `players` check. Should be empty; if not, run it again |
| `raw` | Only present when no players were parsed: the raw `players` output, so you can see why |

With `ALLOWED_PREFIXES` set, `/kickall` needs `players` and `kick` allowed, plus `#lock` when `lock` is true.

`lock` does not unlock afterwards. Send `{"command":"#unlock"}` yourself, or restart the server.

---

## Errors

All errors are JSON: `{"ok": false, "error": "<message>"}`.

| Status | Meaning |
| --- | --- |
| 400 | No command, bad JSON, or unreadable body |
| 401 | Missing or wrong token |
| 403 | Command not allowed by `ALLOWED_PREFIXES` |
| 405 | Wrong HTTP method (`/kickall` is POST only) |
| 502 | RCON problem: login rejected, no reply from the server, or a socket error |

Unknown paths return the standard plain-text `404 page not found`.

---

## Useful commands

| Command | Effect |
| --- | --- |
| `players` | List connected players |
| `say -1 <message>` | Broadcast a message to everyone |
| `kick <player#> <reason>` | Kick one player |
| `#lock` / `#unlock` | Block or allow new connections |
| `#shutdown` | Stop the server (only restarts if something supervises the process) |

`players` rows look like this:

```
0   10.0.1.51:48796       69   7a6sd5f876asdf567asd8765fd897sdf(?)  Player_Name
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

- Quote a leading `#` (`rcon "#lock"`) or the shell treats it as a comment. Use single quotes around messages containing `!`.
- The console uses the container's own `RCON_*` variables, needs no API token, and ignores `ALLOWED_PREFIXES`.
- `rcon kickall` with no reason sends the kicks with no reason text. The HTTP endpoint defaults to `Server restarting`.

---

## Scheduler task form

| Field | Value |
| --- | --- |
| URI | `http://<server-ip>:8080/kickall` |
| Payload | `{"reason":"Server is going down for a restart NOW!!!","lock":true}` |
| Headers | blank |
| Authorization Header | the `API_TOKEN` value (bare, or `Bearer <token>` if the tool doesn't add the prefix) |
| Content Type | `application/json` |

Each task sends one request. For a restart sequence, schedule separate tasks, for example a `say -1` warning, then `/kickall` a few minutes later.

---

## Behaviour notes

- **Sessions:** each HTTP request opens a fresh UDP session. `/kickall` uses one session for all its commands.
- **Retries:** login is retried up to 3 times (1 second each) because UDP packets can be lost. Commands are never retried, so a lost reply can't make `kick` or `say` run twice.
- **Timing:** commands wait up to 2 seconds for a reply. Login can take up to about 3 seconds when packets are lost. `/kickall` can take longer with many players; the HTTP server allows up to 120 seconds per request.
- **Server messages:** BattlEye pushes server console messages to connected RCON clients; the bridge acknowledges them while a command is waiting.
- **Security:** the token is sent in plain text over HTTP. Keep the bridge on your LAN, or put it behind HTTPS before exposing it. Anyone holding the token can run any allowed RCON command.

---

## Rebuilding after code changes

Compose does not rebuild an existing image on its own. From the project folder:

```bash
docker compose up -d --build rcon-bridge
```
