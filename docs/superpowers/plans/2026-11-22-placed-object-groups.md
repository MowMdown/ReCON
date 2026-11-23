# Placed Object Groups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Group placed objects on the Live Map by 50 m squares, with a group popup (counts on demand), Zoom in, and a group Info dialog; then ship a ReCON `-dev` pre-release.

**Architecture:** One new read endpoint lets the page ask the mod for several placed objects' contents at once and read back those that have come. Everything else is in `internal/livemap/templates/live.html`: grouping in `drawPlaced`, a group popup on the existing `plcPopup`, and a new `gModal` dialog reusing the inventory tree's `irow` rows.

**Tech Stack:** Go (net/http), Leaflet 1.x, plain JS in the template.

**Spec:** `docs/superpowers/specs/2026-11-22-placed-object-groups-design.md`

## Global Constraints

- One map unit = one metre; squares are `GROUP_M = 50` m, lined up with 0,0.
- A square splits when `GROUP_M` m is at least `SPLIT_PX = 60` px on screen.
- Zoom in: centre on the square, `ZOOM_IN_M = 300` m across.
- Anything waiting on data says `Loading…`.
- No "50 m" wording in the UI; the grid reference names a group.
- Placed objects still only show from `PLACED_MIN_ZOOM` (-2).
- No recon-mod change.

## Review Focus

- A group whose objects are removed or vanish while its popup/dialog is open: rows drop out; an empty group closes its popup and the dialog says "That isn't on the server any more."
- Mod without placed details (older than 1.0.1) or a detail that never comes: counts stop at `Loading…` for 15 s, then show `?`.
- Kinds ticked off in the Placed objects sub-menu: a group counts only the shown kinds.
- Objects exactly on a square's edge (x = 50k): they go in the square to the east/north (`Math.floor`).
- Large groups (50+ objects): one batch request, not one per object.

---

### Task 1: Batch details endpoint

**Files:**
- Modify: `internal/livemap/placed.go`, `internal/livemap/livemap.go`
- Test: `internal/livemap/placed_test.go`

**Interfaces:**
- Produces: `PlacedDetails(ids []int, ask bool, now time.Time) map[int]PlacedDetail` and `GET /api/mod/placed/details?ids=1,2,3[&ask=1]` → `{"ok":true,"details":{"<id>":PlacedDetail}}` (only ids with a detail; ask=1 calls `AskPlaced` for each id first; at most 200 ids, more → 400).

- [ ] Step 1: `TestPlacedDetailsBatch`: two placed objects 7 and 8; `PlacedDetails([]int{7,8}, true, now)` is empty and the next sync reply's `WatchPlaced` is `[7 8]`; after a sync bringing both details, `PlacedDetails([]int{7,8}, false, now)` has both with their items; asking again with `ask=true` empties them.
- [ ] Step 2: run `go test ./internal/livemap -run TestPlacedDetailsBatch` → FAIL (undefined).
- [ ] Step 3: implement `PlacedDetails` and `handleModPlacedDetails`; register the route before `/api/mod/placed/{id}`.
- [ ] Step 4: `go test ./internal/livemap` → PASS.
- [ ] Step 5: commit.

### Task 2: Grouping and the group marker

**Files:** Modify `internal/livemap/templates/live.html` (`drawPlaced` and new helpers next to it).

**Interfaces:**
- Produces: `placedGroups(list) -> [{key, cx, cz, x0, z0, objs:[Placed]}]` (key `"<i>,<j>"`, `i = floor(x/GROUP_M)`), `groupSplit() -> bool` (true when `GROUP_M` m ≥ `SPLIT_PX` px), `worstHealth(objs)`.

- [ ] Step 1: `drawPlaced` builds groups from the shown objects in view; a group of one, or any group while `groupSplit()`, draws its objects as today; otherwise one marker per group: the square in `vehCondition(worstHealth)` colour plus `×N` (class `gmark`), clicking opens `openGroupPopup(key)`.
- [ ] Step 2: while split, each group of 2+ draws a dashed `L.rectangle` of its square (non-interactive, class `gsquare`).
- [ ] Step 3: markers keyed by `'g'+key` / object id, so redraws update in place; `zoomend` already calls `drawPlaced`.
- [ ] Step 4: check in the browser against the fake mod (Task 5): grouped at whole-map-ish zoom, split close in.

### Task 3: Group popup with counts

**Files:** Modify `live.html`.

**Interfaces:**
- Consumes: Task 1 endpoint, Task 2 `placedGroups`.
- Produces: `openGroupPopup(key)`, `groupOpen` (key or null), `groupDetail` (`{id: PlacedDetail}`), `askGroup(ids)` (one `?ask=1` call, then polls without ask every 1 s up to 15 tries, while `groupOpen` or `groupInfo` is set), `objCount(detail) -> number` (every item in and on it, all levels).

- [ ] Step 1: popup on `plcPopup`: `<h4>N placed objects</h4>`, grid reference; one row per object: condition square, name, `Loading…` / `N items` / `1 item` / `empty` / `?` after 15 s; row click → `openPlacedPopup(id)`; `Placed by Alice (3), Bob (1), unknown (1)`; buttons **Zoom in** (`map.setView([cz,cx], log2(viewWidth/ZOOM_IN_M))`, closes popup) and **Info** (`openGroupInfo(key)`).
- [ ] Step 2: `renderPlacedPopup` re-renders the group popup when `groupOpen` is set (so it updates when details and the 30 s list come); a group that is gone or empty closes it.
- [ ] Step 3: browser check.

### Task 4: Group Info dialog

**Files:** Modify `live.html` (new `#gModal` markup next to `#oModal`, CSS, Esc/backdrop handling like the other dialogs).

**Interfaces:**
- Consumes: Task 3 `groupDetail`, `askGroup`; existing `irow`, `pathTo`, `countIn`, `removePlaced(id)`, `placedName`, `placedBy`.
- Produces: `openGroupInfo(key)`, `closeGroupInfo()`, `groupInfo` (key), `groupSel` (object id), `groupItemSel` (item id).

- [ ] Step 1: header `N placed objects`, grid badge, items inside in all, Close top right (`dlg-x`).
- [ ] Step 2: left: one row per object (expand ▸/▾ shows its attachments and cargo as `irow` rows, nested); right: the selected object's facts (kind, where, condition, placed by, placed), damage zones, contents count, buttons Show on map (closes, centres at `ZOOM_IN_M`), Refresh (`askGroup([id])`), Remove (`removePlaced(id)`); selecting an item inside shows that item like the inventory detail (name, class, condition, quantity).
- [ ] Step 3: all objects gone → "That isn't on the server any more."
- [ ] Step 4: browser check.

### Task 5: Verify, ship

- [ ] Step 1: fake mod (scratch `fakemod.py`) also sends `placedSent` + a cluster of 5 placed objects and answers `watchPlaced` with `placedDetails`; screenshot grouped, popup (Loading… then counts), split, Info dialog; desktop and phone width.
- [ ] Step 2: `go vet ./... && go test ./...`; rebuild `docs/preview` if templates in `internal/web` changed.
- [ ] Step 3: commit, push `claude-mod`; tag the next `v3.0.N-dev` and push it (the workflow builds the image and the pre-release); check the run passes and the release exists.
