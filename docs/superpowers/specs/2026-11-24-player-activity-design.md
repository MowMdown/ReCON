# Player activity: trails, an Activity tab and session replay

## Why

Admins want to know what a player did, not only where they are now: where
they went, when they got in a car and drove it, who they killed, how they
died, when they joined and left. Today the Live Map shows only the present,
and kills and deaths are a list with no path around them.

## Scope

Everything here comes from what ReCON already receives from recon-mod 1.0.3
(the 1 s sync: position, heading, state, vehicle and seat; kill events) and
what ReCON keeps itself (sessions, the commands it sends). **No mod changes.**
Shots, hits, infected and animal kills, looting and base building need the
mod to send more and are left for later.

## What is recorded

ReCON records on every sync from the mod, on the server, whether or not
anyone has the Live Map open and at any zoom. Nothing is recorded while the
mod isn't connected (a trail shows a gap there).

### Movement points

For each online player, a point (time, x, z, heading) is saved when:

- they have moved **10 m** or more since the last saved point, or
- **5 s** have passed since the last saved point,
- and always at an event (so every event sits on the trail).

While they stand still (under 10 m moved and the same heading), a point
is saved every 30 s instead of every 5 s, so a player idling costs little.

The mod sends positions once a second (`intervalMs`, 1000 by default), so
points can't be closer than that; a slower mod interval spaces them further.

### Events

Worked out by comparing each sync with the one before, plus ReCON's own
records.

| Event | Shown as | Detected from |
|---|---|---|
| Joined | 🟢 Joined | session start (`session_history` / `active_sessions`) |
| Left | 🔴 Left | session end |
| Vehicle trip | 🚗 Got in Olga 24 · driver / 🚗 Got out of Olga 24 · drove 2.4 km | `vehicleId`, `seat`, `driver` |
| Bleeding | 🩸 Started bleeding (2 cuts) / Stopped bleeding | `bleeding` |
| Broken leg | 🦴 Broke a leg / Leg mended | `brokenLegs` |
| Sickness | 🤢 Got sick: cholera / Cured: cholera | `agents` bits |
| Knocked out | 💤 Knocked out / Woke up | `unconscious` |
| Restrained | ⛓ Restrained / Freed | `restrained` |
| Killed someone | ⚔ Killed Mossy · M4A1 · 120 m | `mod_kills` (killer) |
| Died | ☠ Killed by Wren · SVD · 340 m / ☠ Died: bled out | `mod_kills` (victim, cause) |
| Respawned | 🔁 Respawned | alive again after dying |
| Admin action | 🛠 Admin: healed / teleported 3.1 km / spawned M4A1 … | the commands ReCON sends for them |
| Staff teleport | 🛠 Teleported 420 m · Mod | the same jump by a player with the Mod or Admin role (they may teleport in game) |
| Possible teleport | ⚠ Possible teleport (420 m in 1 s) | a jump of more than ~100 m in one sync, on foot, with no admin teleport behind it, by a player below Mod |
| Possible speed hack | ⚠ Possible speed hack | on-foot speed over a limit no player reaches, held for several syncs, by a player below Mod |

Not recorded: chat, "now driving" (a seat change to driver mid-trip), and
the item in their hands.

**Vehicle trips.** Getting in and out is recorded only when the vehicle
moved more than 10 m while they were in it. A trip is two events: "Got in"
(with the time they actually got in) and "Got out" with the distance. They
count as the driver if they drove any part of it ("drove 2.4 km"),
otherwise as a passenger ("rode 2.4 km"). Because ReCON only knows the car
moved after the fact, "Got in" is written once the car has gone 10 m.

**Admin actions** are recorded without who sent them: ReCON's commands
don't record the admin today.

**Cheat flags** only show as red events in Activity and on the trail; they
raise no alert and take no action. Players with the **Mod or Admin** role
(Players page) are never flagged: they may teleport and move fast in game.
Their jumps are still recorded, as a purple "Teleported" event with their
role, like a teleport sent from ReCON. The role is the one they have when
it happens.

**Teleports on the trail.** A teleport isn't drawn as a walked path: a thin
dotted line joins where they left to where they arrived, with an icon at
each end. Purple 🛠 for a teleport sent from ReCON or by staff, red ⚠ for a
possible teleport. Example: https://claude.ai/artifact/N8qUgPuyAWMX1prxG7wVUw

### Storage

Two new tables in ReCON's database (next to `mod_kills`):

- `activity_points`: player id, time, x, z, heading. Indexed by player and time.
- `activity_events`: player id, time, kind, x, z, and the details as a small
  JSON object (vehicle name and distance, weapon and range, other player,
  cause…). Indexed by player and time.

Kills and deaths stay in `mod_kills`; the Activity view reads them from
there and doesn't copy them.

**Retention:** 3 days by default, changeable in a setting. Older points and
events are deleted once a day. At 10 m / 5 s a busy server (~60 players)
comes to roughly 0.1–0.2 GB for 3 days.

## Trail between points

Between two points the trail bends with the player's heading when the
heading at each end is within about 60° of the direction from one point to
the other (a smooth curve whose ends point the way they faced). Otherwise
(strafing, walking backwards, a car reversing) it is a straight line. The
same curve is used for the trail and for the replay ghost.

## On the map: the Trails layer

- A new **Trails** layer in Layers, on or off for everyone (all online
  players' trails, or none). Remembered in this browser like the others.
- Each trail is the player's **current session**, in their color, faded,
  drawn under every other layer.
- A small **green flag** where they joined: hover or tap for "Joined ·
  10 min ago".
- Someone who **left in the last 30 minutes** keeps their trail, ending in a
  small **red flag** ("Left · 5 min ago"). After 30 minutes it goes.
- Event icons along the trail (kills, deaths, vehicle trips, knocked out,
  cheat flags); hover or tap one for its details.
- Shown on the big map and the player dialog's mini-map.

## The Activity tab

A new tab in the Live Map's player dialog, for online and offline players
(offline ones open from the Offline list, as today). Not on the Players page.

- Sessions, newest first, within the kept days: date, joined → left, length,
  and a one-line summary (2 kills, 1 death, drove 5.1 km).
- Opening a session lists its events in order with their times. Filters by
  kind: vehicles, combat, health, admin, cheat flags.
- Clicking an event shows that spot on the mini-map.
- **Replay** on each session.

## Replay

Replay plays one session from start to end.

- It opens in a larger map view, about **1 km across**, that follows the
  ghost (like Follow); dragging it stops following, and a Recenter button
  brings it back.
- **The ghost** is the player's arrow, see-through, in their color, moving
  along the trail's curve, its heading turning smoothly, with a short fading
  tail. In a vehicle it becomes the car icon.
- **Pop-ups**: a small bubble beside the ghost for each event, fading after
  a few seconds. On by default: kills, deaths, vehicle trips, knocked out,
  restrained, cheat flags, joined and left. Off by default, switchable on the
  replay bar: health (bleeding, legs, sickness) and admin actions.
- **Controls**: play/pause; speed 1×, 5×, 20×, 60×; a timeline to drag, with
  ticks for events (red for kills and deaths, blue for vehicle trips);
  "Skip quiet stretches" (fast through time with no movement or events).
- Gaps: a stretch with no data (the mod was disconnected) is skipped, with
  the ghost fading out and back in.

## Not in this version

- Mod-only events: shots, hits, infected and animal kills, looting, base
  building, doors.
- Replaying a kill (killer and victim together) or an area.
- Activity on the Players page (a link to the Live Map's dialog at most).
- Alerts or Flows triggers for cheat flags.
- Recording who (which admin) sent a command.

## Open questions for the plan

- The speed limit for "Possible speed hack" (a sprint is about 6–7 m/s;
  something like 12 m/s held for 3 syncs, on foot).
- Where the retention setting lives (the Live Map's settings or the
  Developer page).
- Whether the replay view is a dialog over the map or replaces the player
  dialog's contents.
