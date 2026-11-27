# Economy Editor TODO

This file tracks the DayZ Central Economy (CE) editor work across **ReCON** and its **ReCON-Mod** mission-file transport bridge.

## Project boundaries

- [ ] Keep all CE parsing, discovery, validation, dependency analysis, XML generation, persistence, backups, history, and UI in **ReCON** (Go).
- [ ] Keep **ReCON-Mod** limited to authenticated mission-file transport and filesystem read/write operations.
- [ ] Do not move CE rules or economy-specific behavior into ReCON-Mod.
- [ ] Do not add Economy functionality to ReCON Manager.
- [ ] Preserve the existing ReCON UI design system and page conventions.
- [ ] Treat mission-file changes as saved to disk only; report that a DayZ restart is required. Never imply the running server has loaded the changes or restart it automatically.

## Current foundation

- [x] Create the `economy` development branch in ReCON.
- [x] Add the initial Economy page and navigation entry.
- [x] Add basic XML/ZIP import and ZIP export.
- [x] Add draft persistence and basic revision records in ReCON SQLite.
- [x] Add initial XML syntax validation.
- [x] Add source-hash conflict protection for draft saving.
- [ ] Review and complete the initial implementation before treating these foundations as production-ready.

## 1. Mission-file discovery and synchronization

### ReCON-Mod transport

- [x] Add protocol structures for mission-file write requests and per-file results.
- [x] Move mission-file read/write functionality out of `ReCON_EventSpots.c` into the dedicated `ReCON_MissionFiles.c`.
- [x] Add write dispatch to the Mod sync lifecycle.
- [ ] Compile the Mod and resolve all Enforce Script/compiler errors.
- [ ] Review path validation and harden traversal/symlink/path handling as supported by the DayZ filesystem API.
- [ ] Decide and implement whether writes may create new XML files; current implementation only writes existing files.
- [ ] Confirm write acknowledgements are retained and retried correctly when a sync response is lost.
- [ ] Test nested mission paths and multiple files in one sync.
- [ ] Verify per-file success and failure reporting against a real mission directory.
- [ ] Confirm compatibility behavior with older ReCON/Mod protocol versions.

### ReCON discovery and read path

- [ ] Replace the current limited/hardcoded mission-file read list with a request mechanism for arbitrary relative mission paths.
- [ ] Read `cfgeconomycore.xml` from the mission.
- [ ] Parse its registered CE folders/files and recursively discover referenced configuration files.
- [ ] Preserve registration order, include order, and override semantics.
- [ ] Support nested relative paths and custom/modded XML registrations.
- [ ] Record missing files and read failures without silently dropping registrations.
- [ ] Return file contents and hashes to ReCON.
- [ ] Add a refresh/rescan operation and show discovery status in the UI.

## 2. CE data model and XML engine

- [ ] Define typed Go models for known CE structures.
- [ ] Implement parsing for:
  - [ ] `cfgeconomycore.xml`
  - [ ] `types.xml`
  - [ ] `globals.xml`
  - [ ] `economy.xml`
  - [ ] `events.xml`
  - [ ] `cfgspawnabletypes.xml`
  - [ ] `cfgrandompresets.xml`
  - [ ] `cfglimitsdefinition.xml`
  - [ ] `cfgeventspawns.xml`
  - [ ] `cfgeventgroups.xml`
  - [ ] Map-specific group/proto files and other discovered CE XML.
- [ ] Preserve unknown elements, attributes, comments, and custom modded content during edits and round trips.
- [ ] Preserve meaningful source ordering and file boundaries.
- [ ] Preserve multiple definitions and load-order/override behavior rather than flattening files into one global collection.
- [ ] Add safe XML generation that changes only intended values.
- [ ] Add round-trip tests using vanilla and modded mission fixtures.
- [ ] Add malformed XML and unknown-element regression tests.

## 3. Economy workspace and visual editors

- [ ] Replace the basic XML-only workspace with the full Economy workspace while retaining the existing ReCON visual style.
- [ ] Build a configuration tree grouped by discovered CE file/category.
- [ ] Add a Types editor with search, sorting, filtering, and pagination for large item lists.
- [ ] Edit classname, nominal, min, lifetime, restock, quantmin, quantmax, cost, flags, usage, and value fields.
- [ ] Add multi-select and bulk editing for selected item definitions.
- [ ] Add item duplication, copy/paste, and bulk import/export.
- [ ] Show where an item is defined and which related configurations reference it.
- [ ] Add a Globals editor.
- [ ] Add an Economy settings editor.
- [ ] Add an Events editor.
- [ ] Add event spawn and event group editors.
- [ ] Add a Spawnable Types editor for attachments, cargo, variants, and damage settings.
- [ ] Add Random Presets and Limits Definition editors.
- [ ] Add a raw XML editor for advanced/custom configuration.
- [ ] Keep visual and raw XML editing synchronized without discarding unknown XML.
- [ ] Make all editors usable on narrower screens and with large configurations.

## 4. Dependency analysis and validation

- [ ] Build a dependency/reference graph across discovered files.
- [ ] Validate referenced files, presets, event groups, and other known references.
- [ ] Detect duplicate definitions and report their source files and load order.
- [ ] Detect invalid values and structurally inconsistent definitions.
- [ ] Detect missing or unresolved references.
- [ ] Detect potentially conflicting overrides.
- [ ] Treat unknown classnames as warnings by default so modded content is not incorrectly rejected.
- [ ] Do not automatically flag intentional values such as `nominal=0` as errors.
- [ ] Account for CE flags and their effect on quantity/count behavior; avoid simplistic validation assumptions.
- [ ] Categorize findings as errors, warnings, and informational notices.
- [ ] Link each finding to the affected file and editor row/field.
- [ ] Allow validation of one file and the complete discovered configuration.
- [ ] Add automated tests for validation rules and known edge cases.

## 5. Drafts, change review, and publishing

### ReCON publish workflow

- [ ] Add a publish UI showing changed files and their status.
- [ ] Generate a preview/diff of every file before publishing.
- [ ] Require validation to complete before publish; define which warnings may be overridden.
- [ ] Back up the current mission files before writing.
- [ ] Persist publish operations and their per-file statuses in SQLite.
- [ ] Make publish operation IDs stable and retries idempotent.
- [ ] Track queued, sent, acknowledged, failed, and verified states.
- [ ] Retry failed or unacknowledged transfers safely.
- [ ] Verify written content using hashes returned/read back from the Mod.
- [ ] Detect external mission-file changes between initial read and publish; never overwrite them silently.
- [ ] Report partial success accurately and allow retrying only failed files.
- [ ] Show the final status as **Saved to disk — Restart required** after successful writes.
- [ ] Do not automatically restart the DayZ server.

### Draft and conflict handling

- [ ] Distinguish imported source, working draft, generated output, and published mission state.
- [ ] Track per-file source and working-copy hashes.
- [ ] Detect external changes before save and publish.
- [ ] Provide a conflict comparison and explicit resolution workflow.
- [ ] Ensure failed publish operations do not mark files as published.

## 6. Backups, revisions, and rollback

- [ ] Back up original mission files before each publish operation.
- [ ] Store revision metadata and changed-file contents in SQLite.
- [ ] Distinguish saved drafts from successfully published revisions.
- [ ] Show revision history with timestamps and operation status.
- [ ] Compare any two revisions.
- [ ] Restore a previous revision as a new working draft.
- [ ] Support rollback to mission files through the normal publish/verification flow.
- [ ] Support named snapshots.
- [ ] Export and import complete snapshots as ZIP archives.

## 7. Presets and administration tools

- [ ] Save and apply reusable configuration presets.
- [ ] Add configurable bulk nominal/quantity adjustments.
- [ ] Add classname search and replace across discovered files.
- [ ] Add configuration comparison between imported snapshots.
- [ ] Identify definitions with no known active references (informational only; do not assume they are safe to delete).
- [ ] Identify missing referenced definitions/files.
- [ ] Add economy balance summaries and category comparisons.
- [ ] Support modded/custom CE XML without requiring vanilla-only schemas.

## 8. Map visualization (later phase)

- [ ] Evaluate reuse of existing ReCON map assets and map components.
- [ ] Visualize event spawn positions and related map configuration.
- [ ] Link map markers to the corresponding event/spawn XML entries.
- [ ] Keep map visualization optional and separate from core CE parsing/editing.

## Suggested delivery milestones

1. **Reliable import and discovery:** Read a real mission, discover every registered CE file, and display the complete configuration tree.
2. **Lossless editing:** Edit a known value, regenerate XML without losing unknown content, and export the result.
3. **Core visual editors:** Deliver Types and Globals first, followed by Events and Spawnable Types.
4. **Validation:** Add dependency-aware validation and actionable diagnostics.
5. **Safe publishing:** Back up, publish, acknowledge, verify, and report restart-required status.
6. **History and tools:** Add rollback, snapshots, presets, bulk editing, and comparison.
7. **Map integration:** Add optional event/spawn visualization.

## Definition of done

The first end-to-end milestone is complete when ReCON can import/read a real DayZ mission, discover all CE files registered through its economy configuration, display them in a configuration tree, edit a value, regenerate the affected XML without losing unknown content or ordering, validate the result, and export it. Publishing is a separate milestone and must include backup, per-file acknowledgement, verification, and conflict protection.
