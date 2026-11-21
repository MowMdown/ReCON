# Chat Car Replace and Vehicles Role Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Vehicles role that unlocks the vehicle chat commands, one chat-spawned vehicle per player (the next one replaces it, crew put out first), and the container shipping the recon-mod it needs into `/data/@ReCON`.

**Architecture:** Roles stay stored as numbers (Vehicles = 3) and are compared through a rank. ReCON's livemap keeps `chat_vehicles` (GUID -> persistent ID) and sends `spawn` with `replace`; recon-mod 0.0.1m spawns, then deletes the replaced vehicle, and reports the new persistent ID in the result.

**Tech Stack:** Go (ReCON), vanilla JS templates, Enforce Script (ReCON-Mod, packed with `go run ./cmd/pbo`).

**Spec:** `docs/superpowers/specs/2026-11-21-chat-car-replace-design.md`

## Global Constraints

- Branches: ReCON `claude-mod`, ReCON-Mod `main` (user's standing rule). Push only when asked.
- Role numbers: Player 0, Mod 1, Admin 2, **Vehicles 3**. Rank: Player 0 < Vehicles 1 < Mod 2 < Admin 3. Valid role values 0..3.
- Vehicle commands' minimum role: Vehicles. Staff (gets `!code`, kick/ban/warning notices) = rank >= Mod.
- Only the four vehicle chat commands track/replace; Live Map spawns never touch `chat_vehicles`.
- Persistent ID text: `b1:b2:b3:b4` (four ints from `EntityAI.GetPersistentID`).
- Mod: new vehicle first; only if it was created, delete the replaced one (crew out first). Missing replaced vehicle is not an error.
- Mod version 0.0.1m (config.cpp `version`, `ReCON_Sync.mod`). Old mod: car spawns, nothing tracked, ReCON logs once `recon-mod: tracking chat vehicles needs recon-mod 0.0.1m`.
- UI role order everywhere: Player, Vehicles, Mod, Admin.
- Bundled mod: `recon-mod.version` (repo root) = `v0.0.1m`; image path `/opt/recon/@ReCON` (env `RECON_MOD_BUNDLE`); marker file `.version` inside `@ReCON` (hidden); target `<DataDir>/@ReCON`.
- Tagging/pushing the mod release and ReCON builds happen only when the user says so.

## Review Focus

- A flow saved before this change with "{player_role} is at least Mod" (value "1"): a Vehicles player (3) must not match. Test in Task 1.
- A Vehicles player typing `!kick`: ignored silently (not allowed), as for a Player. Test in Task 1.
- Two quick chat spawns by one player before the first result returns: the second must replace whatever is stored when its result arrives; the first car may survive (accepted: rare, staff only). Ledger it; no test.
- A spawn that fails (bad class): the stored row is unchanged. Test in Task 3.
- An old mod (no `entity` in results): no row written, one log line. Test in Task 3.

---

### Task 1: The Vehicles role (Go)

**Files:**
- Modify: `internal/flows/conditions.go` (constants, names, rank, role comparison), `internal/flows/commands.go` (`allowedFor`, staff check, `chatTargetAllowed`, vehicle defs' Role), `internal/flows/flows.go:552` (validation 0..3), `internal/web/players.go:311` (validation 0..3)
- Test: `internal/flows/roles_test.go`

**Interfaces:**
- Produces: `RoleVehicles = 3`; `func RoleRank(r int) int` (0→0, 3→1, 1→2, 2→3, else 0); `roleNames` indexed by number: `{"Player", "Mod", "Admin", "Vehicles"}`; `const RoleMax = 3`.
- `allowedFor(role)`: `RoleRank(role) >= RoleRank(d.Role)`. Staff: `RoleRank(r) >= RoleRank(roleMod)`. `chatTargetAllowed`: `RoleRank(target) < RoleRank(role)`.
- Role condition compare (conditions.go, kind role only): both sides through `RoleRank` before eq/ne/ge/le.

- [ ] **Step 1: Write failing tests**

```go
func TestRoleRank(t *testing.T) // RoleRank(0,3,1,2) == 0,1,2,3; RoleName(3)=="Vehicles"
func TestVehiclesRoleCommands(t *testing.T) // stub modConnected/vehicleChoices/spawnVehicle as vehicles_test.go does:
// role 3 "!car olga" spawns; role 3 "!kick 5 x" says nothing; role 3 "!help" lists ..., !car, !truck, !boat, !vehicle (no !kick)
func TestVehiclesRoleNotStaff(t *testing.T) // listOnlineStaff skips a role-3 player (stub roles via the db helper used by existing staff tests)
func TestModCanPunishVehicles(t *testing.T) // chatTargetAllowed(mod, roleMod, target with role 3) == true; (vehicles, 3, player 0) used by !kick is still blocked by allowedFor
func TestRoleConditionUsesRank(t *testing.T) // branch {player_role} ge "1" (Mod): role 3 -> false, role 1 -> true, role 2 -> true; le "3" (Vehicles): role 0 -> true, role 1 -> false
```

- [ ] **Step 2:** `go test ./internal/flows/ -run 'Role|Vehicles'` → FAIL (undefined RoleRank/RoleVehicles)
- [ ] **Step 3:** Implement the Interfaces; flows validation message `pick Player, Vehicles, Mod or Admin`; players.go rejects < 0 or > `flows.RoleMax`.
- [ ] **Step 4:** `go test ./internal/flows/ ./internal/web/` → ok (existing role tests keep passing)
- [ ] **Step 5:** Commit `Roles: a Vehicles role for the vehicle chat commands`

### Task 2: The Vehicles role (pages, README)

**Files:**
- Modify: `internal/web/templates/players.html` (`ROLE_NAMES`, `ROLE_INFO`, picker order, badge), `internal/web/templates/flows.html` (`ROLES`, help rows, chat dialog tabs), `README.md`
- Test: `internal/web/web_test.go`

**Interfaces:**
- Consumes: role numbers/rank from Task 1.
- players.html: `ROLE_NAMES = ['Player', 'Mod', 'Admin', 'Vehicles']` (by number); picker iterates `ROLE_ORDER = [0, 3, 1, 2]`; `ROLE_INFO` keyed by number, Vehicles: tag `vehicle spawner`, can: everything a Player can, plus `!car`, `!truck`, `!boat`, `!vehicle` (needs recon-mod). Badge for role > 0 unchanged logic (shows Vehicles).
- flows.html: `ROLES = [['0','Player'],['3','Vehicles'],['1','Mod'],['2','Admin']]`; help text `Player < Vehicles < Mod < Admin`; validation message `Pick Player, Vehicles, Mod or Admin`. Chat dialog tabs: `All Players` (role 0), `Vehicles` (role 3), `Mods & Admins` (roles 1, 2); row tab from `c.role`.

- [ ] **Step 1: Failing test** `TestSetRoleVehicles`: POST role 3 to the player role route → 200 and stored 3; role 4 → 400.
- [ ] **Step 2:** `go test ./internal/web/ -run SetRoleVehicles` → FAIL only if Task 1 validation is missing (else it passes: then it's a guard; ledger it).
- [ ] **Step 3:** Page and README edits as above (README: roles table gets Vehicles; "Mods can kick, ban and warn Players and Vehicles players").
- [ ] **Step 4:** `go vet ./... && go test ./...` → ok
- [ ] **Step 5:** Commit `Roles: Vehicles on the Players page, flows and the chat commands dialog`

### Task 3: ReCON tracks each player's chat car

**Files:**
- Create: `internal/livemap/chatcars.go`, `internal/livemap/chatcars_test.go`
- Modify: `internal/livemap/modlink.go` (`Command.Replace`, `Result.Entity`, `CommandStatus.ChatCar`, result handling), `internal/livemap/livemap.go` (`openChatCars` in `Start`), `internal/flows/vehicles.go` (`spawnVehicle` uses `livemap.SpawnChatVehicle`)

**Interfaces:**
- `Command.Replace string` json `replace`; `Result.Entity string` json `entity`; `CommandStatus.ChatCar bool` json `chatCar,omitempty`.
- Table: `CREATE TABLE IF NOT EXISTS chat_vehicles (guid TEXT PRIMARY KEY, entity TEXT NOT NULL, type TEXT NOT NULL, spawned_at INTEGER NOT NULL)` (guid lowercase).
- `func openChatCars(d *sql.DB) error`; package var `chatDB *sql.DB`.
- `func ChatVehicleOf(guid string) (entity string, ok bool)`
- `func SpawnChatVehicle(guid, class string, now time.Time) (CommandStatus, error)`: `Queue(guid, Command{Action: "spawn", Text: class, Value: 1, Replace: <stored entity or "">}, now)`, then marks that queued command `ChatCar = true` (under the state lock, by its ID).
- Result handling in `Sync`: for a `ChatCar` command finishing ok with `Entity != ""`, collect `{guid, entity, type}`; write them after the state lock is released (upsert). Ok with empty `Entity`: log the Global Constraints line once per process. Failed: nothing.

- [ ] **Step 1: Failing tests** (`setup(t)`, an in-memory sqlite as kills tests do, complete catalog with `Hatchback_02`, player A online):
  - `TestChatCarReplaceSent`: no row → queued command has `Replace == ""`; store row `1:2:3:4` → next `SpawnChatVehicle` sends `Replace == "1:2:3:4"`.
  - `TestChatCarStoredOnResult`: sync the command as sent, then a result `{ID, OK: true, Entity: "5:6:7:8"}` → `ChatVehicleOf(guidA) == "5:6:7:8"`.
  - `TestChatCarKeptOnFailure`: row `1:2:3:4`, result `{OK: false, Error: "x"}` → row still `1:2:3:4`.
  - `TestChatCarOldMod`: result ok, `Entity == ""` → no row.
  - `TestLiveMapSpawnNotTracked`: `Queue(... spawn ...)` (not via SpawnChatVehicle) + ok result with Entity → no row.
- [ ] **Step 2:** `go test ./internal/livemap/ -run ChatCar` → FAIL (undefined)
- [ ] **Step 3:** Implement; `flows.spawnVehicle` calls `livemap.SpawnChatVehicle(guid, it.Type, now)` instead of `livemap.Queue`.
- [ ] **Step 4:** `go test ./...` → ok
- [ ] **Step 5:** Commit `Chat commands: one tracked vehicle per player, replaced by the next`

### Task 4: recon-mod 0.0.1m: report and replace (ReCON-Mod repo)

**Files (ReCON-Mod):**
- Modify: `ReCON/scripts/3_Game/ReCON/ReCON_Protocol.c` (`ReCON_Command.replace`, `ReCON_Result.entity`, version), `ReCON/config.cpp` (version), `ReCON/scripts/4_World/ReCON/ReCON_Actions.c` (spawn path), `ReCON/scripts/4_World/ReCON/ReCON_Vehicles.c` (persistent ID helpers, forced delete), `README.md`

**Interfaces:**
- `static string ReCON_Vehicles.PersistentID(EntityAI v)` → `b1:b2:b3:b4`.
- `static EntityAI ReCON_Vehicles.ByPersistentID(string pid)` (scan `s_All`).
- `static void ReCON_Vehicles.ForceDelete(EntityAI v)`: for each crew seat with a member, `Transport.CrewGetOut(seat)`; any crew member still attached is set 2 m beside the car on the surface; then `g_Game.ObjectDelete(v)`.
- `SpawnVehicleAt` returns the created vehicle via an out param (or a static `s_LastSpawned`), so the `spawn` case can: on success, if `c.replace != ""` and `ByPersistentID(c.replace)` exists and isn't the new one, `ForceDelete` it; then `r.entity = PersistentID(new)`.

- [ ] **Step 1:** Implement (no Enforce test runner; correctness is checked in Task 5).
- [ ] **Step 2:** `go test ./... && go run ./cmd/pbo` in ReCON-Mod → ok, `@ReCON/addons/ReCON.pbo` built; `grep -a 0.0.1m` finds the version in it.
- [ ] **Step 3:** README: `spawn` gains `replace`, results gain `entity`.
- [ ] **Step 4:** Commit `Mod 0.0.1m: report a spawned vehicle's persistent ID; spawn can replace one`

### Task 5: The container ships recon-mod

**Files (ReCON):**
- Create: `recon-mod.version` (`v0.0.1m`), `internal/modbundle/modbundle.go`, `internal/modbundle/modbundle_test.go`
- Modify: `Dockerfile` (a stage that downloads and unzips the pinned release, copies to `/opt/recon/@ReCON`, writes the marker), `cmd/recon` startup (call it once with the data folder), `README.md`

**Interfaces:**
- `func modbundle.Install(bundleDir, dataDir string) (written bool, version string, err error)`: no bundle dir → `(false, "", nil)`; reads `bundleDir/.version`; same as `dataDir/@ReCON/.version` → `(false, v, nil)`; else copies the bundle to `dataDir/@ReCON.new`, removes `dataDir/@ReCON`, renames `.new` into place → `(true, v, nil)`.
- `func modbundle.Bundled() string`: the bundled tag (`""` outside the image), read once.


- [ ] **Step 1: Failing tests** `TestInstallFresh` (no `@ReCON` → copied, marker matches, `written`), `TestInstallSame` (same marker → untouched, a sentinel file in it survives), `TestInstallNewer` (older marker + stray file → replaced, stray gone), `TestInstallNoBundle` (missing bundle dir → nothing written).
- [ ] **Step 2:** `go test ./internal/modbundle/` → FAIL (undefined)
- [ ] **Step 3:** Implement; startup logs `wrote recon-mod <v> to <dataDir>/@ReCON: copy it to the game server`; errors logged, never fatal. Dockerfile stage: `FROM alpine:3.20 AS mod`, `ARG` read from `recon-mod.version` via `COPY` + shell, `wget` the zip, `unzip`, `echo $tag > @ReCON/.version`; runtime `COPY --from=mod /out/@ReCON /opt/recon/@ReCON`.
- [ ] **Step 4:** `go test ./...` → ok; `docker build .` if docker is available here (else ledger: verified by the CI image build after the user approves tagging v0.0.1m).
- [ ] **Step 5:** Commit `Container: ships the recon-mod it needs into /data/@ReCON`

### Task 6: In-game check with the user (live server)

Not code. With the user, on their server running the new ReCON build and mod 0.0.1m (they install `@ReCON`):
- [ ] They type `!car gunter` → `GET /api/mod/vehicles` shows the new car; ReCON's `chat_vehicles` row has an entity (check via the player's API or a log line).
- [ ] They type `!car ada` → Gunter gone, Ada present, row updated.
- [ ] They sit in the Ada, another `!car olga` → they end up standing beside where the Ada was, Ada gone, Olga present.
- [ ] Restart the game server; `!car sarka` → the Olga (spawned before the restart) is deleted.
- [ ] After the new container starts: `/mnt/user/appdata/recon/deerisle/@ReCON` exists with a hidden `.version` = `v0.0.1m`.
- [ ] Any failure: back to systematic debugging; no deletion of anything else.
