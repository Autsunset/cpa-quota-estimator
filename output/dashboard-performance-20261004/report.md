# Dashboard performance review — 0.19.4

Measured on 2026-10-04 against an online SQLite backup containing **one account**, 151,205 usage events, 7,891 quota samples, 14 cycles, and 3,558 calibration segments. The live services remained running. No production plugin or database schema was changed.

## Findings

- CPU profiling attributed about 70% of the measured endpoint work to reconstructing online cycle scales. The cost-prefix query used `idx_usage_account_scope_time`, repeatedly scanning the account's history for individual cycles.
- Cycle listings aggregated usage for every cycle before applying the requested limit. Latest-quota detection also scanned and sorted account history.
- The page waited for account inventory and overview before starting charts, then waited for all reports and calibration requests before revealing the page.

## Changes

- Added indexes for account/scope/cycle/reset lookups, covering cycle usage totals, and valid quota observations ordered by observed time.
- Applied the cycle limit before usage aggregation. Filtered online scale segments by account, main scope, lag, and observation cutoff in SQL.
- Prioritized summary and chart requests, revealing them before starting secondary requests. Kept the updating indicator until secondary requests finish and cleared dependent account data while waiting.
- Retained request-sequence checks, including after the pricing-task lookup, so superseded loads cannot overwrite a newer selection.

## Measurements

Local endpoint execution time, median of three runs on the same snapshot; excludes network, host proxy overhead, and browser rendering. The original build used the original snapshot; the optimized build used a copy with the new indexes. These are individual endpoint measurements, not live page-load times.

| Endpoint | Before | After | Reduction |
| --- | ---: | ---: | ---: |
| Overview | 1194 ms | 163 ms | 86% |
| Summary | 1218 ms | 298 ms | 76% |
| Series | 1211 ms | 298 ms | 75% |
| Monthly | 363 ms | 194 ms | 47% |
| Usage | 1104 ms | 302 ms | 73% |

Weights, backtest, and prices remained below 1 ms each. Initial index creation adds a one-time migration cost and additional index maintenance on writes; no stale-response cache was introduced.

## Validation

- `go test ./...`, `go vet ./...`, and Linux amd64 `make build`.
- Browser integration test, including an intentionally blocked monthly response: quota and charts remain visible while the page still indicates pending work. Existing navigation, account isolation, stale-read protection, pricing, coverage, mobile, and bilingual checks pass with zero JavaScript exceptions.
- Snapshot comparisons preserve monthly reports, weights, backtests, prices, and capacity estimates, within floating-point tolerance. Forecast timestamps and rolling usage windows advance between runs and are not expected to match byte for byte.
- JavaScript syntax, Markdown structure and bilingual links, and `git diff --check`.

To repeat endpoint measurements on an existing, consistent backup with the appropriate schema, run `DASHBOARD_SNAPSHOT=/absolute/path/to/backup.sqlite go test -run '^TestDashboardSnapshotPerformance$' -count=3 -v .`. The test opens the backup read-only and does not run migrations. Optional `DASHBOARD_OUTPUT` saves response JSON locally for comparison.

The source and local build are prepared as 0.19.4. Publishing a GitHub Release and upgrading the live plugin remain separate steps; the live plugin was still 0.19.3 during this review.
