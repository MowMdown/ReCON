# Vehicle Chat Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mods and admins type `!car|!truck|!boat|!vehicle <name> [color]` in game chat and a whole vehicle from the map's `vehicles.json` spawns in front of them.

**Architecture:** `internal/livemap` owns the per-map file (it has the data folder, the world, and the mod's item list): reading it, writing the first one from the item list, resolving classes. `internal/flows` adds the four chat commands and calls livemap through replaceable package vars, so its tests don't need a live mod. The chat-commands settings dialog marks the commands "needs recon-mod" and lists vehicles not in the file.

**Tech Stack:** Go (stdlib only), vanilla JS in `internal/web/templates/flows.html`.

**Spec:** `docs/superpowers/specs/2026-11-21-vehicle-chat-commands-design.md`

## Global Constraints

- Branch: commit to `claude-mod` (the user's standing rule for this repo; overrides CLAUDE.md's `claude`). Push only when the user asks.
- File: `<DataDir>/maps/<world lowercase>/vehicles.json`; shape `category -> name -> color -> class`; categories `car`, `truck`, `boat`, `vehicle`.
- Case never matters: command, category, name, color, and class (sent with the item list's exact spelling).
- No color: `default` if present, else a random color.
- Role: `roleMod` and up. Cooldown default `"0"`.
- Without the mod: no file, no `maps/<world>/` folder, no vehicle log lines; `!help` hides the four; typing one replies exactly `recon-mod isn't running on this server`.
- ReCON never rewrites an existing `vehicles.json`.
- Generated file: 2-space indent, sorted keys; log `recon-mod: vehicles.json for <world> written: <n> vehicles`.
- Vanilla cars (category `car`): `CivilianSedan`, `Sedan_02`, `Hatchback_02`, `OffroadHatchback`, `Offroad_02`.
- Plain-language comments in the repo's style; README updated.

## Review Focus

- A `vehicles.json` with a syntax error: the command replies `vehicles.json for <world> has an error at line <n>: <msg>` (not silence, not a crash). Test in Task 1.
- A class in the file that the server doesn't have (typo or unloaded mod): reply passes on `<class> isn't an item the server can spawn`. Test in Task 3.
- A file whose keys are mixed case (`"Car"`, `"Olga"`, `"Black"`): still matches. Test in Task 1.
- A vehicle with only colored classes and no `default`, typed without a color: a random one spawns (never an error). Test in Task 3.
- The mod reconnects on a different map: a file is written for the new map too, and the old map's file is untouched. Test in Task 2.

---

### Task 1: The vehicle file: read, generate, resolve

**Files:**
- Create: `internal/livemap/vehiclefile.go`
- Test: `internal/livemap/vehiclefile_test.go`

**Interfaces:**
- Produces:
  - `type VehicleFile map[string]map[string]map[string]string` (category -> name -> color -> class; every key lowercased on load)
  - `func LoadVehicleFile(path, world string) (VehicleFile, error)`: `os.ErrNotExist` passes through (wrapped, `errors.Is` works); a JSON error becomes `vehicles.json for <world> has an error at line <n>: <msg>` (line computed from the `json.SyntaxError`/`json.UnmarshalTypeError` offset).
  - `func GenerateVehicleFile(items []CatalogItem) VehicleFile`: only `Kind == "vehicle"`.
  - `func (f VehicleFile) Count() int`: number of vehicles (names) across categories.
  - `func CatalogClass(class string, now time.Time) (CatalogItem, bool)`: the item-list entry matching `class` ignoring case (needs the mod connected and the list complete).

Generation rules (the algorithm the tests don't fully determine):
- Group by display name; a name that is empty or starts with `$` groups alone by class.
- Base = longest common prefix of the group's classes; if it isn't itself one of the classes, cut it back to before its last `_` (and drop a trailing `_`).
- Color = lowercased remainder after `base_`; the class equal to base is `default`; a group of one is `default`.
- Name = display name's first word, lowercased, `[a-z0-9]` only (fallback: lowercased base). Clash within a category: every clashing vehicle appends its next word (no separator) until unique; if words run out append `2`, `3`...
- Category by lowercased base: contains `boat` -> `boat`; contains `truck` -> `truck`; base in the vanilla-car list -> `car`; else `vehicle`.

- [ ] **Step 1: Write the failing tests**

```go
// The deerisle item list (from the live server, 2026-11-21), plus a non-vehicle.
var deerisle = []CatalogItem{
	{"Boat_01_Black", "Rubber Boat", "vehicle"}, {"Boat_01_Blue", "Rubber Boat", "vehicle"}, {"Boat_01_Camo", "Rubber Boat", "vehicle"}, {"Boat_01_Orange", "Rubber Boat", "vehicle"},
	{"CivilianSedan", "Olga 24", "vehicle"}, {"CivilianSedan_Black", "Olga 24", "vehicle"}, {"CivilianSedan_Wine", "Olga 24", "vehicle"},
	{"Hatchback_02", "Gunter 2", "vehicle"}, {"Hatchback_02_Black", "Gunter 2", "vehicle"}, {"Hatchback_02_Blue", "Gunter 2", "vehicle"},
	{"Motorbike_01_Blue", "Jana 50", "vehicle"}, {"Motorbike_01_Red", "Jana 50", "vehicle"}, {"Motorbike_01_Yellow", "Jana 50", "vehicle"},
	{"Motorbike_02_Blue", "Bitrak 682", "vehicle"}, {"Motorbike_02_Green", "Bitrak 682", "vehicle"}, {"Motorbike_02_Red", "Bitrak 682", "vehicle"}, {"Motorbike_02_Yellow", "Bitrak 682", "vehicle"},
	{"OffroadHatchback", "Ada 4x4", "vehicle"}, {"OffroadHatchback_Blue", "Ada 4x4", "vehicle"}, {"OffroadHatchback_White", "Ada 4x4", "vehicle"},
	{"Offroad_02", "M1025", "vehicle"}, {"Offroad_02_Beige", "M1025", "vehicle"},
	{"RFMosquito", "Mosquito MK III Helicopter", "vehicle"},
	{"Sedan_02", "Sarka 120", "vehicle"}, {"Sedan_02_Grey", "Sarka 120", "vehicle"}, {"Sedan_02_Red", "Sarka 120", "vehicle"},
	{"Truck_01_Covered", "M3S Covered", "vehicle"}, {"Truck_01_Covered_Blue", "M3S Covered", "vehicle"}, {"Truck_01_Covered_Orange", "M3S Covered", "vehicle"},
	{"jmc_Boat_STAG_Black", "STAG Inflatable Boat", "vehicle"}, {"jmc_Boat_STAG_Red", "STAG Inflatable Boat", "vehicle"}, {"jmc_Boat_STAG_White", "STAG Inflatable Boat", "vehicle"},
	{"jmc_atv_Aqua", "ATV", "vehicle"}, {"jmc_atv_Blue", "ATV", "vehicle"}, {"jmc_atv_Green", "ATV", "vehicle"}, {"jmc_atv_Grey", "ATV", "vehicle"}, {"jmc_atv_Pink", "ATV", "vehicle"}, {"jmc_atv_Purple", "ATV", "vehicle"}, {"jmc_atv_Red", "ATV", "vehicle"},
	{"jmc_atv_STAG_Black", "STAG ATV", "vehicle"}, {"jmc_atv_STAG_Red", "STAG ATV", "vehicle"}, {"jmc_atv_STAG_White", "STAG ATV", "vehicle"},
	{"AKM", "AKM", "weapon"},
}

func TestGenerateVehicleFileDeerisle(t *testing.T) {
	f := GenerateVehicleFile(deerisle)
	// categories and names
	want := map[string][]string{
		"car": {"ada", "gunter", "m1025", "olga", "sarka"}, "truck": {"m3s"},
		"boat": {"rubber", "stag"}, "vehicle": {"atv", "bitrak", "jana", "mosquito", "stag"},
	}
	// assert sorted keys of each category == want[cat], and no other categories
	// spot checks:
	// f["car"]["olga"] == {"default":"CivilianSedan","black":"CivilianSedan_Black","wine":"CivilianSedan_Wine"}
	// f["boat"]["rubber"] == {"black":"Boat_01_Black","blue":"Boat_01_Blue","camo":"Boat_01_Camo","orange":"Boat_01_Orange"}
	// f["vehicle"]["stag"] == {"black":"jmc_atv_STAG_Black","red":"jmc_atv_STAG_Red","white":"jmc_atv_STAG_White"}
	// f["vehicle"]["atv"]["pink"] == "jmc_atv_Pink"; len(f["vehicle"]["atv"]) == 7
	// f["vehicle"]["mosquito"] == {"default":"RFMosquito"}
	// f["truck"]["m3s"]["orange"] == "Truck_01_Covered_Orange"
	// f.Count() == 13
}

func TestGenerateVehicleFileNameClash(t *testing.T) {
	// Two vehicles in "vehicle" both starting "STAG": {"jmc_atv_STAG_Red","STAG ATV"} and {"jmc_quad_STAG","STAG Quad"}
	// -> names "stagatv" and "stagquad"
}

func TestLoadVehicleFileIgnoresCase(t *testing.T) {
	// file: {"Car": {"Olga": {"Black": "CivilianSedan_Black"}}}
	// LoadVehicleFile -> f["car"]["olga"]["black"] == "CivilianSedan_Black"
}

func TestLoadVehicleFileErrors(t *testing.T) {
	// missing file -> errors.Is(err, os.ErrNotExist)
	// "{\n  \"car\": {\n    \"olga\": \n  }\n}" -> err.Error() starts with "vehicles.json for deerisle has an error at line 4:"
}

func TestCatalogClassIgnoresCase(t *testing.T) {
	// setup(t); sync a complete catalog containing {"CivilianSedan_Black","Olga 24","vehicle"}
	// CatalogClass("civiliansedan_black", now) -> Type "CivilianSedan_Black", true
	// CatalogClass("Nope", now) -> false
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/livemap/ -run 'VehicleFile|CatalogClass'`
Expected: FAIL (undefined: GenerateVehicleFile, LoadVehicleFile, CatalogClass)

- [ ] **Step 3: Implement the Interfaces above in `internal/livemap/vehiclefile.go`**

`CatalogClass` reads `state.catalogTypes` under the lock with an `strings.EqualFold` scan (the list is ~3,000 items; a scan per command is fine).

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/livemap/`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/livemap/vehiclefile.go internal/livemap/vehiclefile_test.go
git commit -m "Live Map: read and generate a map's vehicles.json"
```

### Task 2: Write the first file once the item list is in

**Files:**
- Modify: `internal/livemap/vehiclefile.go`, `internal/livemap/livemap.go` (`Start`: keep `h.DataDir`), `internal/livemap/handlers.go` (`handleModSync`: call after a successful `Sync`), `internal/livemap/modlink.go` (state field + reset on reconnect and in `reset()`)
- Test: `internal/livemap/vehiclefile_test.go`

**Interfaces:**
- Consumes: `GenerateVehicleFile`, `Catalog(now)`, `Current(now)` (for the world)
- Produces:
  - `var dataDir string` (package, set in `Start`)
  - `func vehicleFilePath(world string) string` = `filepath.Join(dataDir, "maps", strings.ToLower(world), "vehicles.json")`
  - `func maybeWriteVehicleFile(now time.Time)`: no-op unless the mod is connected, the item list is complete, the world is known and `state.vehFileFor != world`; then sets `state.vehFileFor = world` and, if the file doesn't exist, creates the folder, writes it (temp file + rename), logs the line from Global Constraints. Write errors are logged, not returned. `state.vehFileFor` is cleared on reconnect.
  - `func VehicleChoices(now time.Time) (VehicleFile, string, error)`: the file for the current world and the world's name. Errors (user-facing, exact): mod not connected -> `recon-mod isn't running on this server`; item list incomplete -> `the item list hasn't come in from the mod yet`; file missing -> `there's no vehicles.json for <world> yet`; parse errors from `LoadVehicleFile`.
  - `func UnlistedVehicles(now time.Time) []CatalogItem`: vehicles in the item list whose class (ignoring case) appears nowhere in the current file; empty when there's no file or no mod.

- [ ] **Step 1: Write the failing tests**

```go
func TestWritesVehicleFileOnce(t *testing.T) {
	// setup(t); dataDir = t.TempDir(); sync world "DeerIsle" with the deerisle catalog complete
	// maybeWriteVehicleFile(now) -> file <dir>/maps/deerisle/vehicles.json exists, LoadVehicleFile(...).Count() == 13,
	//   contents use 2-space indent
	// overwrite the file with `{"car":{"x":{"default":"CivilianSedan"}}}`; call again -> unchanged
	// reconnect (sync at now+1h) on world "chernarusplus" -> maps/chernarusplus/vehicles.json written, deerisle one unchanged
}

func TestNoVehicleFileWithoutMod(t *testing.T) {
	// setup(t); dataDir = t.TempDir(); no sync
	// maybeWriteVehicleFile(now) -> dataDir has no "maps" entry
	// VehicleChoices(now) -> err.Error() == "recon-mod isn't running on this server"
}

func TestVehicleChoicesAndUnlisted(t *testing.T) {
	// after a write: VehicleChoices -> world "deerisle", f["car"]["olga"] present, nil error
	// edit file to only {"car":{"olga":{"default":"CivilianSedan"}}} -> UnlistedVehicles has 41 items, none "CivilianSedan"
	// catalog incomplete -> VehicleChoices err "the item list hasn't come in from the mod yet"
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/livemap/ -run 'VehicleFile|VehicleChoices|Unlisted'`
Expected: FAIL (undefined: maybeWriteVehicleFile, VehicleChoices, UnlistedVehicles)

- [ ] **Step 3: Implement the Interfaces above**

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/livemap/`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/livemap
git commit -m "Live Map: write a map's first vehicles.json from the item list"
```

### Task 3: The `!car`, `!truck`, `!boat`, `!vehicle` commands

**Files:**
- Create: `internal/flows/vehicles.go`
- Modify: `internal/flows/commands.go` (4 defs; `NeedsMod` field; `!help` hiding; dispatch)
- Test: `internal/flows/vehicles_test.go`

**Interfaces:**
- Consumes: `livemap.VehicleChoices`, `livemap.CatalogClass`, `livemap.Queue`, `livemap.Current`
- Produces:
  - `chatCommandDef.NeedsMod bool` with JSON tag `needsMod`
  - Package vars (tests replace them):
    - `var modConnected = func() bool { s, _ := livemap.Current(time.Now()); return s.Connected }`
    - `var vehicleChoices = func() (livemap.VehicleFile, string, error) { return livemap.VehicleChoices(time.Now()) }`
    - `var spawnVehicle = func(guid, class string) (displayName string, err error)`: resolves with `livemap.CatalogClass` (not found -> `errors.New(class + " isn't an item the server can spawn")`), then `livemap.Queue(guid, livemap.Command{Action: "spawn", Text: item.Type, Value: 1}, time.Now())`; returns the item's display name.
    - `var pickColor = func(n int) int { return rand.IntN(n) }` (`math/rand/v2`)
  - `func runVehicleCommand(cmd string, p battleye.Player, set chatSetting, args []string)`

Defs (in this order, after `warning`), each `Role: roleMod, Cool: "0", NeedsMod: true`, fields:
- `spawned` "Spawned (to the player)" default `Spawning {vehicle} ({color})`, ph `vehicle`, `color`
- `list` "List (to the player, for !<cmd> alone or a name it doesn't have)" default `{category}: {names}`, ph `category`, `names`

Help, e.g. car: `!car <name> [color] spawns that car in front of you. !car alone lists the names.` (truck/boat likewise; vehicle: `...spawns that vehicle (bikes, ATVs, helicopters and the rest)...`). Category labels for `{category}`: `Cars`, `Trucks`, `Boats`, `Vehicles`. `{names}`: `olga (black, wine), sarka (gray, red)`; colors sorted, `default` left out; names sorted; sent in lines of at most 5 names, `playersLinePause` apart, like `!players`.

Behavior of `runVehicleCommand` (replies always to `p` only):
- `vehicleChoices` error -> reply the error text; return.
- No args -> list. Category empty -> list reply with `{names}` = `none yet: add some to vehicles.json`.
- Name not found, or color not found -> list reply (for a bad color, `{names}` is just that vehicle's entry).
- No color: `default` if present, else sorted colors `[pickColor(n)]`.
- `spawnVehicle` error -> reply its text; success -> `spawned` with `{vehicle}` = display name, `{color}` = color key; `markUsed(cmd)`.
- In `handleChatCommand`: a `NeedsMod` command typed while `!modConnected()` replies `recon-mod isn't running on this server` (if role allows and it's on), before cooldown; `!help` and `!help !car` skip `NeedsMod` defs while `!modConnected()`.

- [ ] **Step 1: Write the failing tests** (stub the four vars; `chatSay` records `said`; reset as `TestHelpListsWhatTheRoleMayUse` does)

```go
func TestVehicleCommandSpawns(t *testing.T) {
	// file {"car":{"olga":{"default":"CivilianSedan","black":"CivilianSedan_Black"}}}, spawnVehicle records (guid,class), returns "Olga 24"
	// roleMod "!CAR Olga BLACK" -> spawned class "CivilianSedan_Black" for p.GUID; said == ["Spawning Olga 24 (black)"]
	// roleMod "!car olga" -> class "CivilianSedan"; said "Spawning Olga 24 (default)"
}
func TestVehicleCommandRandomColor(t *testing.T) {
	// {"boat":{"rubber":{"blue":"Boat_01_Blue","black":"Boat_01_Black"}}}, pickColor returns 1
	// "!boat rubber" -> class "Boat_01_Blue" (sorted [black, blue][1])
}
func TestVehicleCommandLists(t *testing.T) {
	// {"car":{"olga":{"default":"CivilianSedan","black":"CivilianSedan_Black","wine":"CivilianSedan_Wine"},"sarka":{"red":"Sedan_02_Red"}}}
	// "!car" -> said == ["Cars: olga (black, wine), sarka (red)"]; nothing spawned
	// "!car lada" -> same list; "!car olga pink" -> ["Cars: olga (black, wine)"]
	// "!truck" with no truck category -> ["Trucks: none yet: add some to vehicles.json"]
}
func TestVehicleCommandErrors(t *testing.T) {
	// spawnVehicle returns error "CivilianSedan_Typo isn't an item the server can spawn" -> said is that text
	// vehicleChoices returns error "vehicles.json for deerisle has an error at line 4: ..." -> said is that text
}
func TestVehicleCommandRolesAndMod(t *testing.T) {
	// RolePlayer "!car olga" -> nothing said, nothing spawned
	// modConnected false: roleMod "!car olga" -> said == ["recon-mod isn't running on this server"];
	//   roleMod "!help" -> list has no !car/!truck/!boat/!vehicle; "!help !car" -> nothing
	// modConnected true: roleMod "!help" ends "..., !kick, !ban, !warning, !car, !truck, !boat, !vehicle. Type !help !<command> for details."
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/flows/ -run 'VehicleCommand'`
Expected: FAIL (undefined: runVehicleCommand / vars)

- [ ] **Step 3: Implement as specified above**

Also: `TestHelpListsWhatTheRoleMayUse` must keep passing; it runs with the real `modConnected` (no mod in tests -> hidden), so its expected lists don't change.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/flows/ ./internal/web/`
Expected: `ok` for both

- [ ] **Step 5: Commit**

```bash
git add internal/flows
git commit -m "Chat commands: !car, !truck, !boat and !vehicle"
```

### Task 4: Settings dialog and docs

**Files:**
- Modify: `internal/web/flows.go` (`handleChatCommandsGet`), `internal/web/templates/flows.html` (`renderCmds`, intro/help text), `README.md`
- Test: `internal/web/web_test.go`

**Interfaces:**
- Consumes: `chatCommandDef.NeedsMod` (JSON `needsMod`), `livemap.UnlistedVehicles`, `livemap.Current`
- Produces: `GET /api/chat-commands` adds `"modConnected": bool`, `"vehicleWorld": string` (from `Current`), `"unlistedVehicles": [{type,name}]`

UI (the user's requests, 2026-11-21):
- **Tabs by role:** the dialog (`#cmdModal`) gets two tabs above `#cmdList`, `All Players` (commands with `role == 0`) and `Mods & Admins` (`role >= 1`, admin-only ones included, keeping their `ADMINS` tag). Use the page's existing tab look (`.dtabs`/`.tabs` buttons, as the Help dialog's `data-tab` buttons). `renderCmds` renders only the selected tab's commands; the draft keeps every command, so switching tabs never loses unsaved edits and Save/Reset still cover all of them. Opens on `All Players`.
- **No intro paragraph:** delete `<p class="intro">` from `#cmdModal`.
- **Its content moves to Help** (the page's Help button, `data-pane="chat"`): the paragraph there must also say that the cooldown is server-wide (after anyone uses it, it's ignored for everyone until the time is up; 0 = none), that times are seconds or h m s such as `1m30s`, that the wording and order of what ReCON sends can be changed, and that words in braces such as `{player_name}` are filled in when the message is sent. Keep its existing Players page / Permissions sentence. Add rows for `!car`, `!truck`, `!boat`, `!vehicle <name> [color]` (Mods and Admins, needs recon-mod) to its table.
- A `NeedsMod` command gets a `needs recon-mod` tag in its head (same style as `who`), shown always (it's a requirement, not a state).
- Under `!vehicle`'s section only, when `unlistedVehicles` is non-empty: `On the server but not in maps/<world>/vehicles.json:` then `Name (Class)` items, comma-separated.

- [ ] **Step 1: Write the failing test**

```go
func TestChatCommandsGetHasVehicleFields(t *testing.T) {
	// GET /api/chat-commands -> JSON has keys "modConnected" (false), "unlistedVehicles" ([]), and
	// the "car" command has "needsMod": true
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/web/ -run ChatCommandsGetHasVehicleFields`
Expected: FAIL (missing keys)

- [ ] **Step 3: Implement the API fields and the dialog changes; add the four commands to README's chat-commands section (syntax, roles, the file's path and shape, first-file rules, case-insensitivity, behavior without the mod)**

- [ ] **Step 4: Run all tests and build**

Run: `go vet ./... && go test ./...`
Expected: all `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/web README.md
git commit -m "Chat commands dialog: tabs by role, help moved to Help, vehicle commands"
```
