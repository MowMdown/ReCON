# Vehicle chat commands (`!car`, `!truck`, `!boat`, `!vehicle`)

## Why

A fun tool for staff: a mod or admin types `!car olga black` in game chat
and a whole vehicle appears in front of them. Which vehicles exist differs
per map and per server mod list, so the admin decides exactly which classes
each short name means, in a file, instead of ReCON guessing at spawn time.

## Goals

- Four chat commands, mods and admins only: `!car`, `!truck`, `!boat`,
  `!vehicle`, each followed by a short name and an optional color:
  `!<category> <name> [color]`.
- The vehicle spawns whole (parts and fluids), 8 m in front of the player
  who typed it, facing their way: the existing `spawn` action
  (`livemap.Queue`, as the Live Map's Spawn item uses).
- One file per map, `maps/<world>/vehicles.json` in ReCON's data folder
  (next to that map's picture and calibration), that the admin edits.
- ReCON writes a first version of that file itself when it doesn't exist.

## Non-goals

- Giving a vehicle to another player (only the person who typed it gets one).
- Any change to recon-mod (approach A, below).
- A web editor for the file (the admin edits the JSON).

## Anti-goals

- ReCON overwriting or "fixing" an admin's edited file.
- Players (role below mod) being able to spawn anything.

## Approaches

- **A. All in ReCON, categories from class names (picked).** No mod
  update; works on the current server. A modded car lands in `vehicle`
  until the admin moves it, which matches the "catch-all" rule.
- B. The mod also reports each vehicle's kind (car, boat, helicopter) from
  DayZ's script classes. Catches oddly named modded boats and helicopters,
  but needs recon-mod 0.0.1m and still can't tell a truck from a car.
  Not picked; can be added later.

## The file

`<data>/maps/<world>/vehicles.json` (`<world>` lowercase, as the mod
reports it, e.g. `deerisle`):

```json
{
  "car": {
    "olga": { "default": "CivilianSedan", "black": "CivilianSedan_Black", "wine": "CivilianSedan_Wine" }
  },
  "truck": { "m3s": { "default": "Truck_01_Covered", "blue": "Truck_01_Covered_Blue", "orange": "Truck_01_Covered_Orange" } },
  "boat":  { "rubber": { "black": "Boat_01_Black", "blue": "Boat_01_Blue", "camo": "Boat_01_Camo", "orange": "Boat_01_Orange" } },
  "vehicle": { "mosquito": { "default": "RFMosquito" }, "jana": { "blue": "Motorbike_01_Blue", "red": "Motorbike_01_Red" } }
}
```

- Top level: the four categories (= the command names). A missing
  category means that command has nothing to spawn.
- Then short name -> color -> class name. Case never matters, anywhere:
  `!CAR Olga BLACK` works, and so does a file that says `"Car"`, `"Olga"`
  or `"civiliansedan_black"`. The command (already lowercased by the chat
  handler), category, name and color are compared ignoring case, and the
  class is looked up in the mod's item list ignoring case and sent with
  the item list's exact spelling (`livemap.Queue`'s check is
  case-sensitive today).
- `!car olga` (no color): `default` if there is one, otherwise a random
  color of that vehicle.
- The file is read when a command runs (cheap; edits take effect without
  a restart). A file that doesn't parse: the command replies that
  `vehicles.json` has an error (and where), and the error is logged.

## The first file

Written once, when all of these are true: the mod is connected, its item
list is complete, the world is known, and `maps/<world>/vehicles.json`
does not exist. If it exists ReCON never writes it again.

- **Grouping colors:** catalog items of kind `vehicle` that share a
  display name are one vehicle. Its base is their longest common class-name
  prefix (trimmed back to an `_` boundary); each class's color is what
  follows the base (`CivilianSedan_Black` -> `black`), and the class equal
  to the base is `default`. A display name starting with `$` (untranslated)
  falls back to grouping by class.
- **Short names:** the display name's first word, lowercased, letters and
  digits only (`Olga 24` -> `olga`, `M3S Covered` -> `m3s`). If two
  vehicles in one category would get the same name, both take the next
  word too (`stag-inflatable`... builder's call on the exact rule, as long
  as it is unique and typeable).
- **Categories (only when sure, else `vehicle`):**
  - class contains `boat` (any case) -> `boat`
  - class contains `truck` -> `truck`
  - base class is one of DayZ's own cars (`CivilianSedan`, `Sedan_02`,
    `Hatchback_02`, `OffroadHatchback`, `Offroad_02`) -> `car`
  - anything else (motorbikes, ATVs, helicopters, modded cars) -> `vehicle`
- Written pretty-printed (2-space indent), sorted, so it is easy to edit.
- Logged: `vehicles.json for deerisle written: 13 vehicles`.

On deerisle today: car = Olga, Sarka, Gunter, Ada, M1025; truck = M3S;
boat = Rubber Boat, STAG Inflatable Boat; vehicle = Jana 50, Bitrak 682,
ATV, STAG ATV, Mosquito.

## Vehicles added later

ReCON never changes the file. The chat-command settings dialog (the
vehicle commands' section) lists the vehicle classes on the server that
appear nowhere in this map's `vehicles.json`, so the admin can add them.

## The commands

In `internal/flows/commands.go`, as four `ChatCommandDefs` entries with
`Role: roleMod`, each with the usual on/off, cooldown (default `0`) and
editable messages. Help text, e.g. `!car <name> [color] spawns that car
in front of you. !car alone lists the names.`

- `!car` alone: replies with the names in that category (and their
  colors), e.g. `Cars: olga (black, wine), sarka (gray, red)...`, split
  over several lines like `!players` if long.
- `!car olga black`: queues `spawn` (`Text`: the class, `Value`: 1,
  ground) for the typing player's GUID; replies `Spawning Olga 24 (black)`.
- Unknown name or color: replies with what that name/category has.
- Replies go to the typing player only (`chatSay(p, false, ...)`).

**Servers without the mod.** Some admins run ReCON without recon-mod on
purpose. For them these commands must cost nothing: no `vehicles.json`
(or `maps/<world>/` folder) is created, nothing is logged about vehicles,
and the settings dialog marks the four commands "needs recon-mod" (still
editable, so they're ready if the mod is added later). Everything else in
ReCON works exactly as today.

**When the mod isn't connected (builder: the proposed behavior, user can
correct):** no file is written; `!help` doesn't list the four commands;
typing one replies `recon-mod isn't running on this server` instead of the
usual silent drop. Other `livemap.Queue` errors (player not seen by the
mod yet, class not in the item list) are passed on as the reply the same
way. This differs from the other commands' "silent unless it works" rule
on purpose: only staff can use these, and a silent failure would just
look broken.

## Constraints

- `flows` may import `livemap` (livemap imports nothing from flows).
- The class must be in the mod's item list; `livemap.Queue` already
  checks this.
- Follow the repo's style (plain-language comments, tests beside the code).

## Left to the builder

- Where the file-writing check runs (on sync once the catalog completes,
  or lazily on first command plus on catalog completion).
- Exact message wording and the clash rule for short names.
- How the settings dialog shows the "not in vehicles.json" list.
- Tests: generation from the deerisle catalog above, parsing, color
  defaults/random, role gating, mod-offline replies.
