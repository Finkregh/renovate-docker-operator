# AGENTS.md — internal/parser

Parses Renovate's **NDJSON** log output into structured `api.PRActivity`, `api.LogIssues`, and a summary result status. Called by the executor after each container exit.

## Non-obvious: input is NDJSON, not JSON
Renovate emits one JSON object per line (`RENOVATE_LOG_FORMAT=json`, forced by the executor). We `bufio.Scanner` line-by-line with a **1 MB max line size** (`64 KB` initial, `1 MB` max) — Renovate can emit very long lines with embedded config. Lines that fail to parse as JSON are silently skipped; we do not attempt any recovery.

## What we look at
Parsing is **message-driven**: we partial-unmarshal each line into a targeted struct based on `msg`. There is no schema — Renovate can add fields any time, and we only pull out what we need. See the top-of-file structs (`repositoryFinishedEntry`, `prCreateUpdateEntry`, `prUnchangedEntry`, `gitPushEntry`, `prCreatedEntry`, `branchesInfoEntry`) for the exact fields consumed per event.

Message dispatch (in `ParseRenovateLogs`):
- `"Repository finished"` → sets `RenovateResultStatus` (Disabled / No Config / Onboarding Closed / raw `result`).
- `"Creating PR"` / `"Updating PR"` → seeds/updates `branchMap[branch]` action.
- `"does not need updating"` → marks the branch as unchanged.
- `"git push"` → captures PR URLs from the push message.
- `"PR created"` → merges PR metadata into the branch entry.
- `"PR automerged"` → flags automerge.
- `"branches info extended"` → the richest event, backfills details for known branches.

## Level → severity mapping
Renovate uses **pino** numeric levels, not strings. Anything ≥ 40 is a warning; ≥ 50 is an error. We count both and dedupe messages via `seenMessages`.

## Bounds (CRD-legacy, kept to prevent bloat)
- `MaxPRDetails = 100` — max branch entries in `PRActivity`.
- `MaxLogIssues = 20` — max distinct issue messages surfaced.
- `MaxIssueMessageLen = 256` — per-message truncation.
- Once a bound is hit, `LogIssues.Truncated` is set. Do not raise these without checking downstream UI/CRD field limits.

## Nil vs. empty semantics — important
`ParseRenovateLogs` returns:
- `LogParseResult{HasIssues:false, PRActivity:nil, LogIssues:nil}` when input is empty **or** no line ever parsed as JSON (`parsedAnyLine == false`).
- Non-nil (possibly zero-count) `PRActivity` and `LogIssues` when at least one JSON line was seen.

Downstream code (`executor.handleContainerExit`, `statestore.UpdateProjectStatus`) uses this distinction: `nil` means "keep whatever was there / no signal", non-nil means "overwrite with this result". Do not conflate the two.

## Testing pattern
Tests fixture a multi-line NDJSON string and assert against the returned struct. There are no test files or golden files — everything is inline. If you add new event handling, add a table-driven case beside the existing ones and cover both the "not present" and "present but empty" branches.

## Don't
- Don't parse into a big union struct — partial unmarshal per event is intentional and keeps us resilient to Renovate schema drift.
- Don't emit slog output from parsing — it runs per-container after every exit; noise adds up quickly.
- Don't rely on message ordering; a `branches info extended` may arrive before or after per-PR events. `getOrCreateDetail` merges on branch name.
