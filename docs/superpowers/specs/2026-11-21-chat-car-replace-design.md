# One chat car per player

## Why

A mod or admin can spawn themselves a vehicle with `!car`, `!truck`,
`!boat` or `!vehicle` as often as they like, so old ones pile up around the
map (seen live on deerisle, 2026-11-21: three cars beside one player). Each
player should have at most one chat-spawned vehicle: spawning another
deletes their previous one, no exceptions.

## Goals

- Track, per player (GUID), the one vehicle their last vehicle chat command
  spawned.
- The next vehicle chat command spawns the new one and deletes the old one,
  whoever is in it (the crew is put out on the ground first) and whatever
  state it's in (a wreck too).
- Tracking survives ReCON restarts and game server restarts.

## Non-goals

- Vehicles spawned from the Live Map (Spawn item, Spawn here) are never
  tracked or deleted by this.
- No limit across players, no expiry, no UI to list tracked cars.

## Anti-goals

- Deleting any vehicle that isn't the one this player's own chat command
  made (never by class, position or "nearest").
- The old car going away without the new one arriving.

## Approaches

- **A. ReCON tracks; one "replace" spawn (picked).** ReCON stores
  player -> persistent ID of their chat car. The vehicle chat command sends
  one `spawn` that also names the car to replace; the mod spawns the new car,
  and only if that worked, puts the old car's crew out and deletes it, then
  reports the new car's persistent ID, which ReCON stores.
- B. Two commands (delete, then spawn): can half-happen. Not picked.
- C. The mod tracks in its own file: state ReCON can't see; lost on a
  reinstall. Not picked.

## How (approach A)

**Identity:** DayZ's persistent ID (`EntityAI.GetPersistentID`, four ints),
written `b1:b2:b3:b4`. The Live Map's `GetID` changes every server restart;
the persistent ID doesn't. The mod finds a vehicle by persistent ID by
scanning `ReCON_Vehicles`' list.

**recon-mod 0.0.1m:**
- `spawn` takes an optional `replace` (a persistent ID). With it, after the
  new vehicle is created: if a vehicle with that persistent ID exists, every
  crew member is got out (`Transport.CrewGetOut` for each seat, then placed
  on the ground 2 m beside it if still attached), and it is deleted. A
  missing old car is not an error.
- A vehicle spawn's result reports the new vehicle's persistent ID
  (`ReCON_Result.entity`); other results leave it empty.
- If the new vehicle can't be created, nothing is deleted and the result is
  the error.

**ReCON:**
- A table `chat_vehicles` (guid primary key, entity, type, spawned_at).
- The vehicle chat command's spawn goes through livemap with the player's
  GUID: livemap reads their row, sends `spawn` with `replace` = that entity,
  and remembers that this command is a chat car for that GUID.
- When that command's result comes back ok with an `entity`, livemap writes
  the row (replacing the old one). A failed spawn leaves the row as it was.
- A mod before 0.0.1m ignores `replace` and reports no entity: the car
  spawns, nothing is tracked or deleted, and ReCON logs once that tracking
  needs recon-mod 0.0.1m.
- The chat reply stays `Spawning <vehicle> (<colour>)`; no new message.

## Constraints

- Who can use the vehicle commands is the Vehicles role and up (second
  section below); the one-car rule applies to every one of them alike.
- Builder must confirm in game that a just-created vehicle already has its
  persistent ID, and that `CrewGetOut` leaves the player standing safely; the
  2 m placement is the fallback.

## Left to the builder

- Exact Enforce calls for getting the crew out, verified in game.
- Where livemap keeps "this command is a chat car for GUID" (in the queued
  command's status is fine).
- Tests: replace sent with the stored ID; row written on an ok result with
  an entity; row kept on a failed spawn; Live Map spawns never touch the
  table; old-mod result (no entity) logged and not stored.

---

# A Vehicles role

## Why

Vehicle chat commands shouldn't need Mod: the owner wants to let chosen
players spawn vehicles without giving them kick/ban/warning.

## The roles (each includes everything below it)

| Role | Chat commands |
|---|---|
| Player | everyone's |
| Vehicles (new) | + `!car`, `!truck`, `!boat`, `!vehicle` |
| Mod | + `!kick`, `!ban`, `!warning` |
| Admin | everything |

## How

- **Storage, no renumbering:** `players.role` keeps Player 0, Mod 1,
  Admin 2; Vehicles is stored as **3**. Every comparison goes through a
  rank (Player 0 < Vehicles 1 < Mod 2 < Admin 3), so existing roles and
  saved flows (whose role checks store these numbers) keep their meaning.
- **Where roles are compared** (all switch to the rank): a command's
  minimum role (`allowedFor`); who counts as staff for `!code` and the
  kick/ban/warning notices (rank >= Mod: Vehicles players are not staff);
  who may punish whom (`chatTargetAllowed`: the target's rank below
  yours, so Mods can punish Players and Vehicles players); the flows'
  role condition (is / is not / is at least / is at most compare ranks;
  valid values 0 to 3).
- The four vehicle commands' minimum role becomes Vehicles.
- **UI:** the Players page's role picker (Details, Permissions) and the
  flows' role value picker gain Vehicles (in rank order: Player,
  Vehicles, Mod, Admin); the role badge shows "Vehicles". The chat
  commands dialog's tabs become All Players | Vehicles | Mods & Admins
  (a command on the tab of its minimum role; Admin-only ones under Mods &
  Admins, keeping their ADMINS tag).
- `!help` lists what the role may use, as now (so a Vehicles player sees
  the four vehicle commands once the mod is connected).
- README's chat commands and roles section updated.

## Left to the builder

- Tests: rank order; a Vehicles player can use `!car` but not `!kick`;
  a Mod can kick a Vehicles player; a Vehicles player isn't told about
  `!code`; a flow "is at least Mod" doesn't match a Vehicles player;
  role 3 accepted by the role API and flow validation, 4 rejected.

---

# The container ships the mod it needs

## Why

Admins shouldn't have to guess which recon-mod a ReCON build needs. Each
ReCON image carries that mod and puts it in the data folder, ready to copy
into the game server (e.g. Unraid: `/mnt/user/appdata/recon/deerisle/@ReCON`).

## Approach (A, picked)

- `recon-mod.version` at the ReCON repo root holds one release tag, e.g.
  `v0.0.1m`: the mod this ReCON needs.
- The Dockerfile downloads that release's `ReCON-Mod.zip`
  (`https://github.com/MowMdown/ReCON-Mod/releases/download/<tag>/ReCON-Mod.zip`)
  in a build stage, unzips `@ReCON` to `/opt/recon/@ReCON` in the image and
  writes the tag into `/opt/recon/@ReCON/.version`. A missing
  release fails the image build.
- On start, ReCON compares `/data/@ReCON/.version` with the
  bundled one. Missing or different: `/data/@ReCON` is replaced wholesale
  (written beside it, then swapped), and logged
  `wrote recon-mod v0.0.1m to /data/@ReCON: copy it to the game server`.
  Same: nothing. Outside the container (no `/opt/recon/@ReCON`): nothing.
  Env `RECON_MOD_BUNDLE` overrides the bundle path (tests).
- No update notices on the page (the owner's call): the start-up log line is the only pointer.
- B (build from source in the image) and C (commit the files) not picked.

## Constraints

- Replacing `/data/@ReCON` is safe: it holds only the mod's files (its
  settings live in the game server's profile).
- Every mod release ReCON pins must exist as a tag with `ReCON-Mod.zip`;
  0.0.1l was never tagged, so the first pin is v0.0.1m.
