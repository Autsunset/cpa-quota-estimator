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

The complete Go suite, race detector, `go vet`, Linux amd64 shared-library
build, embedded JavaScript syntax checks, Markdown lint, bilingual/version
consistency checks, and Git whitespace checks passed. The browser integration
suite passed desktop/mobile, both languages, loading/error/offline handling,
coverage settings, and pricing-task checks with zero JavaScript exceptions.

## Release And Deployment

Release workflow 38060914960 and CI workflow 38060911708 succeeded.
The regular v0.19.6 GitHub Release was published on October 10, 2026,
with Linux amd64/arm64, macOS amd64/arm64, Windows amd64, and
`checksums.txt`. All five downloaded archives passed SHA-256, root-library
layout, and ZIP CRC checks. Marketplace registration PR 78 was rechecked
and is merged; no registry update or replacement registration PR is needed.

The deployed library is
`/opt/proxy/cpa/plugins/linux/amd64/cpa-quota-estimator-v0.19.6.so`.
The online backup, previous library, and configuration are retained under
`/opt/proxy/cpa/backups/image-quota-v0.19.6-20261010T145707Z/`.
Preparation and atomic replacement completed while CPA remained running.
CPA was restarted once; the new version loaded and registered successfully.
Rollback was not needed. New API remained healthy with its original
October 9, 2026 container start time throughout.

The deployment backup contains 165,312 raw requests. Comparison against
the running database found zero missing or changed immutable raw fields.
Eight independent image requests, totaling 22,675 Tokens, were isolated,
and five misplaced main-quota samples were removed. The main cycle retains
its seven-day schedule and no longer contains daily image-pool chart points.
SQLite integrity remains `ok`, and the dropped-usage counter remains zero.

The asynchronous refit completed with 1,016 eligible segments, lag 2,
and eligibility version 6. The subsequent historical repricing task
succeeded for 165,343 requests. That normal repricing updates selected-basis
values after fitting; it is separate from the repair's preservation checks.
Credits mode and the GPT-5.6 Sol display anchor were preserved. Final live
HTTP, pricing-task, database, and service checks passed at 23:12 on
October 10, 2026 (Asia/Shanghai).
