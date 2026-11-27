# Claude Code Model & Token Efficiency Policy (`CLAUDE.md`)

## 1. Model & Reasoning Selection

Use the fastest, least expensive Claude model capable of completing the task reliably. Never default to Opus for routine work.

Select models and thinking effort based on task complexity:

| Task | Model | Thinking / Effort |
|---|---|---|
| Simple questions, explanations, formatting | **Haiku 5.5** | Medium |
| Documentation, comments, simple edits | **Haiku 5.5** | Medium |
| Basic Go functions and SQL queries | **Sonnet 5.5** | Medium |
| Docker, Compose, CI/CD configuration | **Sonnet 5.5** | Medium |
| Intermediate to complex Go functions | **Opus 5.5** | Medium |
| Routine bug fixes | **Opus 5.5** | Medium |
| Multi-file code changes & complex coding | **Opus 5.5** | Medium |
| Complex debugging | **Opus 5.5** | Medium |
| Security analysis and vulnerability reviews | **Opus 5.5** | Medium |
| Architectural decisions | **Opus 5.5** | Medium (escalate if needed) |
| Difficult concurrency, networking, or data integrity problems | **Opus 5.5** | Medium (escalate if needed) |

### Claude Model Selection Rules

- Use exact model identifiers supported by the active Claude Code CLI / API configuration.
- Never invent model IDs or assume an unconfigured model is available.
- Default to **Haiku 5.5 (Medium)** for lightweight tasks and **Sonnet 5.5 (Medium)** for standard tasks.
- Use **Opus 5.5 (Medium)** for coding, multi-file refactors, security reviews, and architectural work.
- Do not switch models unnecessarily mid-task without permission.
- If automatic model/effort switching is unavailable in the current session, explicitly recommend the `/model` command and effort setting before proceeding.

## 2. Token & Context Efficiency

Minimize unnecessary token consumption and context window bloat:

- Never read the entire repository unless explicitly required.
- Inspect only files directly relevant to the requested task.
- Use targeted searches (`Grep`, `Glob`) with narrow paths instead of dumping full files.
- Avoid re-reading files already loaded in the current context window.
- Do not repeat explanations or context already established in the session.
- Keep responses concise, direct, and technically complete—skip introductory filler and recaps.
- Apply targeted edits (`Edit`) rather than rewriting entire files (`Write`) when making localized changes.
- Exclude build artifacts, dependencies (`vendor/`, `node_modules/`), binaries, and generated files unless required.
- Run targeted tests (e.g., `go test -run TestName ./pkg/...`) instead of the full test suite when appropriate.

## 3. Task Assessment

Before executing a task:

1. Identify the exact requested outcome.
2. Locate the involved files and components.
3. Estimate complexity and risk.
4. Select the appropriate model tier (**Haiku 5.5 Medium**, **Sonnet 5.5 Medium**, or **Opus 5.5 Medium**).
5. Determine whether investigation is needed before editing.

Do not overcomplicate simple tasks. For complex tasks, investigate root causes before modifying code.

## 4. Escalation Policy

Escalate from **Haiku 5.5 (Medium)** → **Sonnet 5.5 (Medium)** → **Opus 5.5 (Medium)** when:

- Multiple attempted solutions have failed.
- The root cause remains unclear after initial investigation.
- Changes span multiple tightly coupled systems.
- Potential security or data-integrity implications exist.
- Substantial architectural reasoning is required.
- Confidence in the proposed solution is low.

Do not escalate to Opus solely because a straightforward edit touches multiple files.

## 5. Development Practices

- Read and understand existing code before modifying it.
- Preserve existing architecture, idioms, and formatting conventions.
- Avoid unrelated refactoring or scope creep.
- Make the smallest reliable change that solves the problem.
- Check for existing helpers or implementations before writing new ones.
- Do not maintain backward compatibility unless explicitly requested.
- Validate changes with appropriate build or test commands.
- Report exact changes made and note anything left untested.

## 6. Context Management

- Track architectural and implementation decisions already made in the session.
- Avoid rediscovering established project layout or conventions.
- Reuse existing `CLAUDE.md` rules and project documentation.
- Keep tool calls focused on the reported issue.
- Use `/compact` or summarize findings when context grows large rather than reloading the same files.

## 7. Final Response

Every completed task must concisely report:

1. **What was changed**
2. **Which files were modified**
3. **What verification was performed**
4. **Any outstanding issues or limitations**

Never claim a build or test passed unless it was actually executed and succeeded.

## Primary Objective

Maximize engineering throughput while minimizing API costs, token usage, context bloat, and unnecessary reasoning overhead—using **Haiku 5.5 (Medium)** for lightweight tasks, **Sonnet 5.5 (Medium)** for standard implementation, and **Opus 5.5 (Medium)** for complex coding and system-level problem solving.