# Image quota isolation - v0.19.6

## Diagnosis

On October 10, 2026, image-generation responses identified a separate pool with
`X-Codex-Active-Limit: imagegen_premium`, a 1,440-minute Primary window, and
0% used. Text responses still identified `premium`, a 10,080-minute window,
and 80% used. Version 0.19.5 classified both as main usage, temporarily
replaced the main cycle schedule, and stored 80% -> 0% -> 80% chart points.
This was not a genuine main-quota reset.

## Fix

- Identify the image pool from its active-limit header, independently of
  optional raw-header capture.
- When that header is absent, isolate daily `gpt-image-*` observations only.
  Preserve legacy weekly images and images explicitly reporting `premium`.
- Retain image requests in SQLite with an independent scope and no main
  cycle assignment; exclude them from main samples, totals, monthly reports,
  calibration, attribution, and learner segments.
- Run an idempotent transactional upgrade repair. Reclassify proven image
  records, remove misplaced samples, restore affected schedules, recompute
  sample totals, and rebuild derived segments and fits.
- Preserve all raw request IDs, timestamps, headers, Token counts, and
  pricing values. No image-quota dashboard is added in this patch.

## Validation

Regression coverage includes interleaved text/image responses with header
capture enabled and disabled, header precedence, legacy main-pool images,
image-only traffic, image-first historical cycles, one-time upgrade repair,
transaction rollback, and retained raw usage.

The server's preflight SQLite database was exported with the online backup
API while CPA and New API remained running. An isolated copy migrated in
approximately four seconds. All 165,201 raw events, 25,958,098,065 Tokens,
and their aggregate pricing value were preserved. Eight independent image
requests were detached from main usage, and five misplaced samples were
removed. SQLite integrity remained `ok`.

Release and live deployment checks are recorded after completion.

The complete Go suite, race detector, `go vet`, Linux amd64 shared-library
build, embedded JavaScript syntax checks, Markdown lint, bilingual/version
consistency checks, and Git whitespace checks passed. The browser integration
suite passed desktop/mobile, both languages, loading/error/offline handling,
coverage settings, and pricing-task checks with zero JavaScript exceptions.
