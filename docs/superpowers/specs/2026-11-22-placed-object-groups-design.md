# Placed objects grouped on the Live Map

Mockup: https://claude.ai/artifact/3c7gZVQrzBuXfFQBzWyrFV (1: group marker
and popup, 2: group Info dialog, 3: zoomed in, the group split).

## Why

A base is many placed objects within a few meters (crates, tents, barrels,
fences, traps). On the map they are a pile of 10 px squares on top of each
other: hard to count, hard to click one. Admins want to see "there's a base
here with 12 things", what they are, and what is in them, without zooming
in and hunting.

## Goals

1. **Groups.** Placed objects are grouped by a fixed grid of 50 m squares
   (meters on the map, lined up with its 0,0). A square holding two or more
   objects is one marker: a square, colored as its worst-condition object,
   with `×N` beside it. A square with one object shows that object's own
   marker, as today.
2. **Split when zoomed in.** Once a square is about 60 px wide on screen, its
   objects show one by one, and the square shows as a dashed outline (no
   label). Zooming out merges them again.
3. **Group popup** (click the `×N` marker): the square's grid reference, then
   each object (condition color, name, how many items are in and on it:
   `2 items`, `empty`), and who placed them (`Alice (3), Bob (1),
   unknown (1)`). Buttons: **Zoom in** (centers on the square, about 300 m
   across, so it splits) and **Info**. Clicking an object in the list opens
   that object's own popup.
4. **Item counts on demand.** When the popup opens, ReCON asks the mod for
   the contents of every object in the group (as Info does for one today);
   each count shows `Loading…` until it comes, usually a second or two.
5. **Group Info dialog.** Header: `N placed objects`, the grid reference,
   items inside in all; Close top right. Left: a tree like the player
   inventory, each object expandable into its attachments and cargo (and
   theirs). Right: the selected object: kind, where, condition, placed by
   and when, damage zones, contents count; **Show on map**, **Refresh**,
   **Remove**.

## Non-goals

- Acting on a whole group at once (remove all, etc.).
- Changing recon-mod: it already sends every placed object every 30 s, and
  sends an object's contents when asked.
- Moving the other zoom limits to meters (placed objects from zoom -2, the
  town names); that is separate and comes later.

## Anti-goals

- A slower Live Map on a busy server: grouping must cut markers, not add
  work per frame; asking for contents only happens for the group opened.
- Removing more than the one object the admin picked.

## Constraints

- One map unit is one meter (`CRS.Simple`); groups and the split threshold
  are worked out in meters and screen pixels, never by rescaling the map.
- Anything waiting on data says `Loading…`.
- Placed objects still only show from zoom -2 (unchanged here).

## Approaches

1. **Fixed 50 m grid (picked).** Stable: the same objects always group the
   same way, and the group has a grid reference. A base across a square's
   edge splits into two markers; accepted.
2. Distance clustering (objects within N m of each other, e.g. Leaflet's
   markercluster). Follows bases better, but groups shift as you pan and
   zoom, and it adds a library. Not picked.

Item counts: asked from the mod when the popup opens (picked; no mod
change, a second or two of `Loading…`), rather than a recon-mod change
sending a count with every object every 30 s.

## Decided

- 50 m squares; split at about 60 px per square on screen.
- Group marker: square in the worst condition's color plus `×N`; no "50 m"
  wording anywhere, the square's outline shows when split.
- Popup rows show only a count (`2 items`, `empty`), not item names.
- Zoom in centers on the square at about 300 m across.

## Left to the builder

- How the page asks for several objects' contents at once (one request per
  object with `?ask=1`, or one new endpoint taking a list).
- The exact pixel threshold and marker styling, within the mockup.
- What "items" counts: every item in and on the object, all levels (as the
  vehicle and placed Info dialogs count them).
