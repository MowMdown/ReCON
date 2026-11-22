# Live Map: event spots, predators, loadouts and fewer pop-ups

Agreed with the user in conversation on 2026-11-22. Mockup of the placed
object levels and the predator popup:
https://claude.ai/artifact/JJRqS8ag8fwm6cy7vcCvwt

Needs recon-mod 1.0.3 (items marked **mod**); ReCON keeps working with
1.0.2 (those features then stay empty).

## What gets built

1. **Vehicles (mod).** Every sync (1 s) the mod sends the vehicles with
   someone in them (`crewVehicles`), plus each vehicle once more on the sync
   after its last player got out. The full list (and the world events) every
   10 s instead of 5. ReCON updates its list by id between full lists.
2. **Event spots (mod + ReCON).** The mod sends ReCON the mission's
   `cfgeventspawns.xml`, `db/events.xml` and `cfgeventgroups.xml` as text,
   once, when asked. ReCON parses them: every `Static*` event that is
   `active` in events.xml (not `StaticPlant*`), each spawn position, the
   classes that make the event (its children, or its group's). Categories by
   name: Heli crashes (`HeliCrash`, `SantaCrash`), Convoys (`Convoy`), Police
   (`Police`), Medical (`Ambulance`, `Medic`), Gas zones
   (`ContaminatedArea`), Other (the rest). ReCON sends the mod the spots to
   check (id, x, z, radius, classes); the mod checks a slice of them each
   sync (`GetObjectsAtPosition`) and reports which are up. Gas: a dynamic
   spot is up when a gas world event is within 100 m; static zones (all
   `EffectArea`s not on a spot) always show, coloured. The page draws every
   spot, active in colour, inactive grey, with Active/Inactive toggles and
   one per category.
3. **Predator popup (mod).** The mod sends each predator's health (0-1),
   behaviour (`GetBehaviourSlot`), activity (`GetBehaviourAction`),
   attacking (`IsAttack`). New command `deleteAnimal` (text: its id). Popup:
   Health, Behavior, Activity, Attacking, Nearest player (only while
   Hunting/Enraged/attacking, the nearest online player and distance);
   Remove → Cancel / Remove.
4. **Restore condition (mod).** `restore` takes `condition`: `worn` (65%
   health), `pristine` (100%) or empty (as saved). Death restores send
   `worn`.
5. **Restore without confirm.** The Restore button runs at once.
6. **`!save` / `!equip` (ReCON + mod).** Mods and admins. `!save` stores
   their current inventory as their loadout (table `loadouts`, one per
   player, separate from death saves). `!equip` asks "type confirm"; the
   player's next message within 30 s being `confirm` sends `restore` with
   `clear` (the mod empties their inventory first) and `pristine`. Items
   that don't fit go on the ground.
7. **Overlays.** Events and Placed objects are headings that open and close
   their sub-menu; what shows is only what is ticked inside.
8. **Last known inventory.** ReCON asks for one or two online players'
   inventories per sync so each is at most 5 min old; the player dialog
   shows that copy at once ("as of …") until the live one comes.
9. **Occupied cars (mod).** Each player is sent with the vehicle id and
   seat; a car with players is drawn in the driver's colour (or the first
   seat's), their arrows hidden; the popup lists the crew.
10. **Left Shoulder / Right Shoulder** for the `Shoulder` / `Melee` slots.
11. **Refresh by scale bar.** Players, predators and occupied vehicles:
    bar 1 km+ 10 s, 500 m 5 s, 200 m 2 s, less 1 s.
12. **Parked vehicles** (the full list) every 10 s at any zoom; occupied
    ones at the rate above (`/api/mod/vehicles?occupied=1`).
13. **No browser pop-ups.** confirm() becomes the button turning into
    Cancel + the red action in place; alert() becomes a line in the
    dialog or popup. No question text.
14. **Placed object levels.** Bar ≥140 m: one block per 100 m square
    (click zooms to the 100 m step on it); 70-140 m: one per 50 m square
    with the popup (Zoom in left, Info right; Zoom in goes to the 50 m
    step); less: each object. Hidden above 200 m (done).

## Decided

- No guessing an animal's target: "Nearest player" is labelled as such.
- No question text in confirms.
- Loadouts come back at 100%; death restores at Worn.
- The mod never reads mission files on its own: only when ReCON asks.

## Left to the builder

- Spot check radius: 30 m around the spawn position.
- Spot checks per sync: 40.
