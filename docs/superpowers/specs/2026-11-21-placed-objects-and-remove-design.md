# Placed objects on the Live Map, and !remove (ReCON v3.0.1, recon-mod v1.0.1)

## Why

- Admins want to see what players have built or put down (bases, tents,
  barrels...), check its condition and who placed it, look inside, and
  remove it when needed: from the Live Map, not in game.
- A player with a chat car should be able to get rid of it without
  spawning another (`!remove`).

## Goals

1. **`!remove`** (Vehicles role and up): deletes the player's own tracked
   chat car; nothing else.
2. **Placed objects layer** (Overlays, drawn on the same level as vehicles
   and predators): every placed object on the map.
   - **Popup** on click: what it is, its condition, who placed it, grid;
     **Info**.
   - **Info dialog**: what it is (class), where, condition, placed by and
     when, its contents (cargo, attachments, built parts), **Remove**.
3. Ships with the two fixes already on `claude-mod`: the map's picture
   loads without the mod; no more `:mod` image tags.

## Non-goals

- Ownership rules, raid alerts, base limits, or acting on objects in bulk.
- Changing an object (repairing, unlocking) from ReCON.
- Reading the game server's `.ADM` log.

## Anti-goals

- Removing anything other than the one object the admin picked (or, for
  `!remove`, the player's own chat car).
- A Live Map that slows down on a busy server because of thousands of
  objects.

## What counts as a placed object

- **Always** (DayZ's own placed kinds, also ones placed before this
  update): base building (`BaseBuildingBase`: fences and gates,
  watchtowers, territory flags, shelter sites) and tents and improvised
  shelters (`TentBase`).
- **Anything a player places from then on**: any item DayZ calls
  `OnPlacementComplete(player, ...)` on: barrels, crates, chests, protector
  cases, traps, garden plots, fireplaces, and modded placeables. A barrel
  placed before the update looks like any loot on the ground, so it isn't
  listed.

## Who placed it

DayZ doesn't keep the placer on the object (checked in the game's own
scripts; only the server's `.ADM` log writes a "placed" line). recon-mod
records it itself at `OnPlacementComplete`: the player's name and
SteamID64, and the time. It is saved with the object through the Community
Framework's mod storage (CF, already a dependency), so it survives
restarts, and removing the mod later can't corrupt the server's saved
objects. Objects placed before the update: "placed by: unknown".

## recon-mod v1.0.1

- **Tracking:** a list of placed objects, kept as DayZ makes and deletes
  them (the same way as vehicles): the always-kinds by class; others once
  placed (and, after a restart, those with a saved placer).
- **Sync:** every 30 s, `placed`: for each, `id` (this run's), `pid`
  (persistent ID), `type`, `kind` (`base`: BaseBuildingBase; `tent`:
  TentBase; `storage`: Barrel_ColorBase, WoodenCrate, SeaChest,
  ProtectorCase family and other cargo containers; `trap`: TrapBase;
  else `other`), `name` (display name), `x`, `z`, `health`
  (0..1), `placer`, `placerId` (SteamID64), `placedAt` (unix; 0 unknown);
  with `placedSent`.
- **Contents on request:** like `watchVehicles`: `watchPlaced` (ids) in
  the reply; the next sync's `placedDetails` holds that object's damage
  zones and its items (cargo and attachments, as an inventory).
- **`removeEntity`** action (`text`: persistent ID, `stat`: class): deletes
  that one object only if its ID and class match; a vehicle's crew is put
  out first (as replace does); not found = "it's already gone". Used by
  `!remove` and by the dialog's Remove.
- Version `1.0.1` (config.cpp and what it reports).

## ReCON v3.0.1

- **`!remove`:** a chat command (Vehicles role and up, needs recon-mod,
  on the Vehicles tab of the chat commands dialog, cooldown 0, editable
  messages). Looks up the player's chat car; none: "You don't have a
  spawned vehicle". Otherwise queues `removeEntity` (pid, class) and says
  "Removing your <vehicle>". When the result comes: ok or "already gone"
  -> the tracked car is forgotten. A mod before 1.0.1 answers "unknown
  action": ReCON says "!remove needs recon-mod 1.0.1".
- **Placed objects store/API:** kept from the syncs like vehicles;
  `GET /api/mod/placed` (all), `GET /api/mod/placed/{id}` (one, with its
  details: asks the mod, like a vehicle's Info), `POST
  /api/mod/world/removeEntity` (pid, class).
- **Live Map:**
  - Overlays gets **Placed objects** (off by default, remembered), with
    a count; drawn in the vehicles pane (under players, over towns).
  - **Kinds filter** (the owner's request): with Placed objects ticked, the
    menu shows its kinds indented below, each ticked by default,
    remembered, with a count: Base building, Tents & shelters, Storage,
    Traps, Other. Placed objects' count is what's ticked.
  - Markers: a small square per object, coloured by condition (pristine
    green to ruined red). To stay fast with thousands, markers are drawn
    only for what's in view, and only at mid zoom and closer; zoomed out
    the button's tooltip says to zoom in.
  - Popup: name, condition, "placed by <name>" (or unknown), grid, **Info**
    (the vehicle popup's look).
  - Info dialog (like the vehicle one): facts (class, where, condition,
    placed by, when), then built parts / attachments, damage zones and
    cargo side by side, each with its condition; **Remove** (asks to
    confirm: "Remove this Barrel and everything in it?").
- **Bundled mod:** `recon-mod.version` -> `v1.0.1`.
- README: the layer, the dialog, `!remove`, and that "placed by" starts
  with this mod version.

## Approaches

- **A. The mod records the placer itself (picked).** Reliable, kept with
  the object, works with no server config.
- B. ReCON reads the `.ADM` log: ReCON has no access to the game server's
  files (only RCON and the mod). Not possible.
- C. Show no placer: loses what the owner most wants to know. Not picked.

## Constraints

- Builder must check in game: CF mod storage saves and loads the placer
  (needs the mod's `CfgMods` storage setup); `OnPlacementComplete` runs on
  the server for every placeable; a just-placed object already has its
  persistent ID.
- Tests (ReCON): `!remove` with/without a car, old-mod reply, result
  forgets the car; placed list kept from syncs; details asked and
  returned; removeEntity validated (pid and class required).

## Release

`claude-mod` -> PR into `main` -> a `v3.0.1` branch from `main`; mod `main`
-> `v1.0.1` branch; mod released first (tag `v1.0.1`), then ReCON
(`v3.0.1-release`).

---

# Saved inventories and restore (same release)

## Why

Restore a player's gear after a hacker or a griefer killed or robbed
them, choosing exactly which items to give back.

## Saving

- **At death:** the mod sends the victim's full inventory with the kill
  event (read as they die, before the body can be looted). Always saved.
- **Every X minutes (default 5):** ReCON adds a few players per sync (5)
  to the inventories it asks the mod for (the existing `watch` list, so
  no mod change), going round everyone online; a copy is saved only if
  it differs from that player's last saved one (a fingerprint of the
  items).
- **Kept 7 days** (default), then removed (checked hourly).
- Settings: environment variables `INVENTORY_SAVE_MINUTES` (default 5,
  0 = only at death) and `INVENTORY_KEEP_DAYS` (default 7).
- Table `inventory_saves`: id, guid, at, reason (`death` | `timed`),
  kill id (for a death), fingerprint, items (compressed JSON, the
  inventory tree as the mod sends it), restored (JSON: item ids already
  given back, with when and who).

## Restoring

- **Where:** the player dialog's Kills & deaths tab: each of their deaths
  with a saved copy gets **Restore…**; the Inventory tab gets **Saved
  inventories…** (a list: when, death or timed, how many items) to open
  any copy.
- **The restore dialog:** the copy as a tree (slots, then attachments and
  contents), **nothing ticked** at first; **Select all** / **Deselect
  all**; "N of M items selected". Ticking a container or weapon ticks
  what's in and on it (each can be unticked); a ticked item without its
  container is restored loose. Items already restored from this copy are
  marked (and not ticked by Select all). **Restore selected** confirms
  ("Restore 12 items to MowMdown?"), then sends only those.
- **In game (mod `restore` action):** the selected tree (JSON in `text`):
  clothing into its empty slot, else inventory, else ground; attachments
  onto their parent; cargo into their container; condition, quantity,
  ammo (magazines), wetness kept. What can't go anywhere goes on the
  ground at their feet; a class the server doesn't have is skipped and
  reported. The player must be online and alive.
- **Logged:** each restore (to whom, from which copy, which items), and
  the copy's restored list updated so the same items aren't given twice
  by accident.

## Constraints

- Items land in the right containers, not necessarily the same grid
  spot. Restoring doesn't take back what was looted from the body.
- Builder checks in game: attachments onto slots (weapons, clothing),
  magazines with ammo, liquids, nested containers.
