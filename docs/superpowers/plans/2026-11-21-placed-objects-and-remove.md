# Placed Objects and !remove Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `!remove` for a player's chat car, a Live Map layer of placed objects (popup, Info dialog with contents, Remove), and saved inventories with selective restore, shipped as ReCON v3.0.1 + recon-mod v1.0.1.

**Architecture:** recon-mod tracks placed objects (base building and tents always; anything else once placed, its placer saved with CF mod storage), sends them every 30 s, sends one object's contents on request, and gains `removeEntity` (persistent ID + class). ReCON stores them like vehicles, serves them, and draws them; `!remove` and the dialog's Remove use `removeEntity`.

**Tech Stack:** Go, vanilla JS (live.html), Enforce Script (ReCON-Mod).

**Spec:** `docs/superpowers/specs/2026-11-21-placed-objects-and-remove-design.md`

## Global Constraints

- Branches: ReCON `claude-mod`, mod `main`. Push/tag only when the user says so.
- `removeEntity`: `text` = persistent ID `b1:b2:b3:b4`, `stat` = class; delete only if both match; Transport → crew out first (existing `ForceDelete`); missing → error `it's already gone`.
- Placed sync every 30 s: `placedSent`, `placed[]` {id, pid, type, kind, name, x, z, health, placer, placerId, placedAt}; details via reply `watchPlaced` → sync `placedDetails[]` {id, zones[], items[]} (same shapes as vehicle detail).
- CF storage: `storageVersion = 1;` in `CfgMods ReCON_Mod`; key `"ReCON_Mod"`; writes placer name, placerId, placedAt (int).
- Mod version 1.0.1; ReCON `recon-mod.version` v1.0.1.
- `!remove` messages: `You don't have a spawned vehicle` (no row), `Removing your {vehicle}` (editable `removing`), `Your vehicle is already gone` (result "it's already gone"), `!remove needs recon-mod 1.0.1` (result "unknown action ..."). Row forgotten on ok or already-gone.
- Layer: Overlays "Placed objects" off by default (pref `placed`), vehicles pane, markers only for objects in view and at zoom ≥ -2 (the "mid" band and closer); condition colours = `CONDITION` levels by health (>0.7, >0.5, >0.3, >0, else).

## Review Focus

- A `!remove` while the old car is still being emptied (3 s): second removeEntity finds it gone → "already gone"; acceptable.
- Thousands of placed objects: marker drawing limited to view + zoom (test by reading; no JS runtime).
- An object removed in game while its dialog is open: the dialog says it isn't on the server any more (like vehicles).
- removeEntity called with an empty pid or class: 400, never sent. Test in Task 2.
- A mod before 1.0.1: no `placedSent` → layer button tooltip says it needs 1.0.1. Test in Task 2 (`sent` false).

---

### Task 1: `!remove` (ReCON)

**Files:** Modify `internal/livemap/chatcars.go`, `internal/livemap/modlink.go` (Actions: `removeEntity`; result handling for removes), `internal/flows/commands.go` (def), `internal/flows/vehicles.go` (handler); Test `internal/livemap/chatcars_test.go`, `internal/flows/vehicles_test.go`.

**Interfaces:**
- `func RemoveChatVehicle(guid string, now time.Time) (class string, err error)`: no row → `ErrNoChatVehicle` (`errors.New("You don't have a spawned vehicle")`); else `QueueWorld(Command{Action: "removeEntity", Text: pid, Stat: class, GUID? no})` — world action, with `CommandStatus.RemoveFor = guid`. Returns the class.
- Result handling: `RemoveFor != ""` and (ok, or error `it's already gone`) → delete the row (after the lock).
- flows: def `remove` (RoleVehicles, NeedsMod, field `removing` default `Removing your {vehicle}`, ph `vehicle`); help `!remove removes the vehicle your last !car, !truck, !boat or !vehicle spawned.`; handler uses package var `removeChatVehicle = func(guid string) (string, error)` then catalog display name via `livemap.CatalogClass`.
- Older mod: result error starting `unknown action` on a RemoveFor command → log once `!remove needs recon-mod 1.0.1`; the player isn't told later (chat is fire-and-forget); the immediate reply stays `Removing your ...`. (Ruling recorded: no async chat reply.)

- [ ] Tests: `TestRemoveChatVehicleQueues` (row → world command removeEntity with pid/class, RemoveFor set), `TestRemoveChatVehicleNone` (no row → ErrNoChatVehicle), `TestRemoveForgetsOnResult` (ok → row gone; "it's already gone" → row gone; other error → row kept), flows `TestRemoveCommand` (stub: reply `Removing your Olga 24`; none → `You don't have a spawned vehicle`; Player role → nothing).
- [ ] Run → FAIL; implement; `go test ./...` → ok; commit `Chat commands: !remove`.

### Task 2: Placed objects in ReCON (store and API)

**Files:** Create `internal/livemap/placed.go`, `internal/livemap/placed_test.go`; Modify `modlink.go` (sync fields, reply `WatchPlaced`, state), `handlers.go`, `livemap.go` (routes).

**Interfaces:**
- `type Placed struct {ID int; PID, Type, Kind, Name string; X, Z, Health float64; Placer, PlacerID string; PlacedAt int64}` (json: id, pid, type, kind, name, x, z, health, placer, placerId, placedAt).
- `type PlacedDetail = VehicleDetail` shape (id, zones, items) as `PlacedDetail` struct.
- `SyncRequest.PlacedSent Flag`, `.Placed []Placed`, `.PlacedDetails []PlacedDetail`; `SyncReply.WatchPlaced []int` json `watchPlaced`.
- `func PlacedObjects(now) (ps []Placed, sent bool)`, `func AskPlaced(id int, now)`, `func PlacedOf(id int, now) (Placed, PlacedDetail, haveDetail, ok bool)`.
- Routes: `GET /api/mod/placed`, `GET /api/mod/placed/{id}` (`?ask=1`), world action `removeEntity` accepted by `handleModWorld` (validation: non-empty `text` and `stat`).
- [ ] Tests: `TestPlacedKept` (placedSent → list; not sent → `sent` false), `TestPlacedDetailAsked` (AskPlaced → reply.WatchPlaced has id; sync with detail → PlacedOf haveDetail), `TestRemoveEntityValidated` (QueueWorld removeEntity with empty stat → error).
- [ ] Run → FAIL; implement; ok; commit `Live Map: placed objects from the mod, and removeEntity`.

### Task 3: Placed objects on the page

**Files:** Modify `internal/livemap/templates/live.html`, `README.md`.

- Overlays button `placedBtn` "Placed objects N" (pref `placed`, default off); load `/api/mod/placed` every 30 s while on; markers in pane `vehicles`, drawn only within map bounds and when `map.getZoom() >= -2`; button title when zoomed out: `Zoom in to see placed objects`; not sent: `recon-mod doesn't send placed objects: it needs 1.0.1`.
- Kinds filter: under `placedBtn`, rows `base` Base building, `tent` Tents & shelters, `storage` Storage, `trap` Traps, `other` Other (prefs `placed.<kind>`, default on), shown only while Placed objects is on, each with its count; Placed objects' count = ticked kinds; markers of unticked kinds not drawn. Info dialog shows the kind.
- Marker: 10 px square, fill = condition colour, dark stroke; class `omark`.
- Popup (`.vpop` look): name, `<grid>`, Condition (coloured), `Placed by <placer>` or `Placed by unknown`, `Info` button.
- Info dialog `oModal` modelled on `vModal`: facts (Class, Where, Condition, Placed by, Placed at), columns Attachments / Damage / Cargo (reuse `partRow`, `zoneName`), foot: Refresh, **Remove** (`confirm('Remove this ' + name + ' and everything in it?')` → POST `/api/mod/world/removeEntity` {text: pid, stat: type}), Close.
- README: the layer, dialog, `!remove`.
- [ ] `go test ./...` ok (templates parse); commit `Live Map: a Placed objects layer with Info and Remove`.

### Task 4: recon-mod 1.0.1

**Files (ReCON-Mod):** Create `ReCON/scripts/4_World/ReCON/ReCON_Placed.c`; Modify `ReCON_Protocol.c` (ReCON_Placed, ReCON_PlacedDetail, sync/reply fields, version), `ReCON_Module.c` (send every 30 s; watchPlaced), `ReCON_Actions.c` (`removeEntity`), `ReCON_Vehicles.c` (generalise Replace into `ReCON_Vehicles.RemoveByID(pid, type) string`), `config.cpp` (`storageVersion = 1;`, version 1.0.1), `README.md`.

- `modded class ItemBase`: `string m_ReCON_Placer, m_ReCON_PlacerId; int m_ReCON_PlacedAt;` `OnPlacementComplete(Man player, vector position, vector orientation)` (server): set from `PlayerBase.Cast(player).GetIdentity()` (name, plain id), `g_Game.GetTime()`? no: wall clock `CF_Date.Now().GetTimestamp()` if available else 0; `ReCON_Placed.Add(this)`. `CF_OnStoreSave/Load` with key `"ReCON_Mod"` (write only when version ≥ 1; read guarded by `ctx` null). After load: if placer set → `ReCON_Placed.Add`.
- `modded class BaseBuildingBase` and `TentBase`: `EEInit` → Add; `EEDelete` → Remove (ItemBase EEDelete covers all).
- `ReCON_Placed.Kind(EntityAI)`: `base` (BaseBuildingBase), `tent` (TentBase), `storage` (Barrel_ColorBase, WoodenCrate, SeaChest, ProtectorCase family, or anything with cargo that isn't the above), `trap` (TrapBase), else `other`.
- `ReCON_Placed.Read(list)`, `.Detail(id)` (zones + `ReCON_Inventory.ReadEntity`), `.ByPID`.
- `removeEntity`: find by pid among placed and vehicles; type check; Transport → `ForceDelete`, else `ObjectDelete`; result error `it's already gone` / `it is a X, not the Y` / "".
- [ ] `go run ./cmd/pbo`; version 1.0.1 in pbo; commit `Mod 1.0.1: placed objects with their placer, removeEntity`.

### Task 5: Saved inventories (ReCON)

**Files:** Create `internal/livemap/invsaves.go`, `internal/livemap/invsaves_test.go`; Modify `modlink.go` (kill events carry `Inventory []Item`; timed watch rotation; save after the lock), `livemap.go` (table, cleanup ticker, routes), `handlers.go`.

**Interfaces:**
- Table `inventory_saves(id INTEGER PRIMARY KEY, guid TEXT, at INTEGER, reason TEXT, kill_id INTEGER, fingerprint TEXT, items BLOB, restored TEXT)`; items = gzip(JSON of `[]Item`); fingerprint = sha256 of the JSON.
- `var saveEvery = minutesEnv("INVENTORY_SAVE_MINUTES", 5)`, `keepFor = daysEnv("INVENTORY_KEEP_DAYS", 7)`.
- Timed: each sync, up to 5 online players whose last timed save is older than `saveEvery` are added to `reply.Watch`; when their inventory arrives, save if the fingerprint differs from their newest save.
- Death: an `Event` of type kill with `Inventory` → save `death` (always) with the kill's id.
- `func InventorySaves(guid string) []SaveInfo` (id, at, reason, killId, items count, restored count); `func InventorySave(id int64) (Save, error)` (items, restored ids).
- Routes: `GET /api/mod/players/{guid}/saves`, `GET /api/mod/saves/{id}`.
- Cleanup: hourly delete `at < now - keepFor`.
- [ ] Tests: `TestTimedSaveOnlyWhenChanged`, `TestDeathSaveAlways`, `TestTimedRotationLimit` (≤5 added per sync), `TestSavesCleanup`, `TestSaveRoundTrip` (gzip JSON back to items).
- [ ] Commit `Inventories: saved at death and every few minutes (if changed)`.

### Task 6: Restore (ReCON)

**Files:** Modify `invsaves.go` (restore), `modlink.go` (Actions `restore`), `handlers.go`, `templates/live.html` (Saved inventories list, restore dialog, Restore… on deaths), `README.md`.

**Interfaces:**
- `POST /api/mod/saves/{id}/restore {"items": [ids]}`: builds the pruned tree (a ticked item keeps its ticked children; a ticked child of an unticked parent becomes a root), queues `restore` for the save's player (`text` = JSON of the tree), records restored ids with time; errors: player offline, nothing selected.
- Dialog: tree with checkboxes, none ticked; Select all (skips already-restored) / Deselect all; "N of M items selected"; parent tick cascades; Restore selected → confirm → POST.
- [ ] Tests: `TestRestorePrunesTree` (parent+some children; orphan child becomes root), `TestRestoreRecords` (restored ids stored; second listing marks them), `TestRestoreNeedsSelection`.
- [ ] Commit `Inventories: restore selected items from a saved copy`.

### Task 7: recon-mod 1.0.1: inventory at death, restore

**Files (ReCON-Mod):** `ReCON_Protocol.c` (`ReCON_Event.inventory`), `ReCON_Module.c` (on a player's kill event, read their inventory into it), `ReCON_Actions.c` + new `ReCON_Restore.c` (`restore`: parse `text` with `JsonSerializer` into `array<ref ReCON_Item>`; create recursively: clothing → `GetInventory().CreateAttachmentEx(type, slotId)` if the slot is free, else `ReCON_Actions.InInventory`, else `OnGround`; attachments → parent `CreateAttachment`; cargo → parent `CreateEntityInCargo` or `CreateInInventory`; then `SetHealth("", "", health)`, quantity (`SetQuantity`), magazines `ServerSetAmmoCount`, wetness `SetWet`); result error lists skipped classes, "" when all made).
- [ ] pbo builds; commit `Mod 1.0.1: inventory at death, restore`.

### Task 8: Release (on the user's word)

Dev builds only; the user makes full releases. recon-mod: a pre-release from `main` tagged `v1.0.1-dev`, and ReCON's `recon-mod.version` set to that same tag. ReCON: `v3.0.N-dev` (next number) from `claude-mod`. No release branches, no PRs into `main`.
