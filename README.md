<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/logo.png" alt="CPA Quota Estimator icon" width="160" height="160">
</p>

# CPA Quota Estimator

<p align="center">
  <a href="https://github.com/Autsunset/cpa-quota-estimator/blob/main/README.md"><kbd>English</kbd></a>
  <a href="https://github.com/Autsunset/cpa-quota-estimator/blob/main/README.zh-CN.md"><kbd>简体中文</kbd></a>
</p>

[![CI](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml/badge.svg)](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Autsunset/cpa-quota-estimator)](https://github.com/Autsunset/cpa-quota-estimator/releases)
[![License](https://img.shields.io/github/license/Autsunset/cpa-quota-estimator)](LICENSE)

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) quota-observability and capacity-forecasting plugin for Codex. It passively records real traffic flowing through CPA, can combine configured OAuth credentials and sampled accounts in one operational overview, separates 5-hour, weekly, and Spark quota scopes, and translates quota percentage changes into estimated Token capacity and pricing-value equivalents under the selected valuation basis.

The dashboard answers the operational questions that raw quota percentages do not: **Which account is closest to exhaustion? When will it run out? How much work is the remaining quota worth for the current request mix? What happened across previous resets and calendar months?**

> The plugin never sends probe or model requests. OpenAI does not publish a fixed Token capacity for Codex quota windows; Token, USD, and Credits figures shown here are workload-equivalent estimates derived from observed requests, not official plan face values.

<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/docs/images/dashboard-en.png" alt="Quota Estimator per-account forecast dashboard (English)" width="720">
</p>

## At a glance

- **All accounts in one view:** merge recorded accounts with the configured Codex OAuth inventory when available, including accounts awaiting their first sample and credentials that are disabled or unavailable. The table supports per-column filters, type-aware sorting, persisted resizing, and keyboard controls.
- **Independent quota scopes:** automatically separate a detected 5-hour Primary quota from its weekly Secondary quota for both the main Codex allowance and `gpt-5.3-codex-spark`, while keeping all Spark usage in a completely independent ledger.
- **Capacity in practical units:** estimate full-cycle and remaining capacity in Tokens and the selected pricing basis—official API USD rates, Codex Credits, or custom USD rates—with uncertainty ranges and confidence levels.
- **Actionable forecasts:** compare actual usage with a sustainable baseline, cumulative-average pace, and recent pace to estimate exhaustion time and whether a quota will survive until reset.
- **History that survives resets:** retain confirmed cycles, cross-cycle charts, calendar-month reports, quota-consumption equivalents, reset counts, and unused quota at reset.
- **Passive and private by design:** consume no extra quota, store no prompts or response bodies, and keep retained usage metadata in a local SQLite database.

## Detailed features

- Provides an all-account quota overview with current remaining quota, reset state, requests, Tokens, pricing value, full-cycle capacity, confidence, and burn forecast. For detected dual-quota accounts, the same row shows both 5-hour and weekly status; selecting a sampled row opens the existing detailed forecast.
- Listens to CPA's native `usage.handle`; it does not send probe requests or consume additional quota.
- Separates account-wide quota observations from CPA-only usage, with persistent per-account collection coverage. Existing estimates remain enabled with an explicit all-traffic-through-CPA assumption; mixed and unknown modes disable capacity conversions while keeping observed usage and quota trends.
- Persists Token counts, model, `service_tier`, selected-basis pricing value, and `X-Codex-Primary-*` quota metadata in a private SQLite database.
- Syncs OpenAI model pricing from `https://models.dev/catalog.json` by default.
- Accounts for cached reads/writes, output Tokens, and the context pricing tier above 272K input Tokens.
- Provides three pricing bases: **official API USD**, **Codex Credits (new-install default)**, and **custom USD**. API and Credits start from their published model rate cards, then apply a learner-derived model adjustment relative to the selected anchor (GPT-5.6 Sol by default). An unidentifiable model keeps adjustment 1 and is labeled uncalibrated. Each model retains its own official input/cache/output shape; Fast and long-context factors are shown against official rates. Custom prices, Fast multiplier, and long-context controls are editable, and custom mode never applies learner price adjustments. Legacy `legacy_api`/`current_api` settings migrate to `api`; `learned` migrates to `credits`. The old names remain accepted on the settings endpoint as aliases. Saving starts one background recalculation task and returns its ID immediately; request and sample values are rebuilt in short 500-row transactions.
- Shows a per-account **recent model usage** table for 24 hours, 7 days, or 30 days: requests, failures, uncached input, cache reads/writes, output and total Tokens, official-rate credit equivalents, learned per-model quota attribution estimates, and values under the selected pricing basis. The table also allocates selected-basis value by each cycle’s estimated value per quota percent and shows its gap from observed account growth. Observed quota growth remains account-wide; attribution is explicitly a model estimate. Models without a published credit rate are marked as unpriced.
- Builds first-integer-crossing `quota_segments` for main, weekly, Spark, and Spark weekly windows, with lag 0/1/2 feature summaries and exclusion flags for failed requests, unknown models, resets, quota anomalies, and long gaps. Reset timestamps within two minutes are grouped; a confirmed 29%→0% transition inside a stored cycle is treated as a separate learning regime. A historical backfill runs once on upgrade; later crossings refresh their affected regime. A separate idempotent repair splits previously merged long-lived early resets and reassigns event and sample cycle IDs.
- The learner uses a separate log capacity scale for each learning cycle, linked by a configurable Gaussian random walk (σ **0.35** by default). Each new crossing updates its current-cycle scale online. The current cycle borrows the previous scale before its first crossing; its point and interval estimate of selected-basis value per 1% drives remaining value, model Token allowances, and burn forecasts. Model factors are shared across cycles and are unlocked only when within-cycle mixing supplies conditional Fisher information; shared factors are fitted with cycle scales effectively free before random-walk smoothing. Cache/output ratios stay fixed to the Codex Credits shape until both their Fisher and composition-variation gates pass; the API and dashboard label prior-locked values. Huber loss, 21-day decay, and Laplace intervals remain in use. Interrupted status 0/408/499/502 counts are retained for sensitivity analysis; they are not assumed to have a fixed charge in the default fit. Per-request percentages remain estimates, not upstream billing records.
- Adds **guided passive calibration**: choose an account, reference model, Astra or another prior-locked target model, and quota target (5% per phase by default). The dashboard tracks two same-cycle model-only phases, crossing counts, and selected-basis value contamination (>5% warning), then refits weights and shows the target input multiplier before and after. A quota reset invalidates the session. Session state and event-ID spans are available through `GET /calibration`; start, end-phase, and cancel actions use `/calibration/start`, `/calibration/end`, and `/calibration/cancel`. No probe or model request is generated.
- Shows a calculated price table with each cell’s official rate, relative difference, and uncertainty; the anchor row has zero difference. Official API and Credits input/cache/output rates coincide up to a factor of 25 for the listed models, while cache writes, long context, and Fast use distinct rules. The optional `capture_codex_headers` setting omits opaque `X-Codex-Turn-State` values.
- Estimates full-cycle and remaining-quota Token/pricing-value-equivalent capacity with interquartile ranges and confidence levels, and converts the selected cycle’s remaining value into per-model uncached-input, output, and cache-hit Token allowances.
- Shows actual quota usage, a sustainable baseline, cumulative-average projection, recent-rate projection, predicted exhaustion, planned reset time, and countdown.
- Maintains an independent ledger for every confirmed quota cycle. Historical cycles remain available in the selector after a reset.
- Treats `gpt-5.3-codex-spark` headers as a separate quota scope and cycle. When Spark returns a 5-hour Primary plus weekly Secondary pair, each Spark axis gets its own percentage, reset cycle, capacity estimate, and monthly summary instead of treating the 5-hour window as weekly. Spark actual Tokens, requests, pricing value, and quota equivalents never create, split, estimate, or add to the main quota. An optional switch at the top of the dashboard displays the complete Spark statistics below all primary-quota content.
- When—and only when—the latest primary quota window is detected as approximately 5 hours and valid `X-Codex-Secondary-*` headers are present, treats the primary window as the independent 5-hour quota and the Secondary window as the independent weekly quota. Each gets its own percentage trajectory, reset cycles, Token/pricing-value-equivalent capacity, monthly quota equivalents, and dashboard section. Accounts without a detected 5-hour primary window, including weekly-only Pro accounts, keep the existing single-window behavior and never expose or calculate this secondary weekly section.
- Orders quota headers by their response observation time and confirms scheduled transitions with two consistent successful observations. Early resets require three stable successful observations spanning at least 60 seconds; a previously unseen full-window schedule can also confirm sparse readings above 5% when they remain at most half the old peak.
- Detects confirmed temporary upstream quota-regime changes that later revert to the prior reset schedule. It restores the current quota, preserves the anomalous interval as a red chart band and an evidence card, and excludes it from capacity and recent-rate estimation. If an inferred early-reset boundary preceded the recovery, two consistent successful returns near the old peak before the original reset undo that boundary transactionally.
- Handles exhausted 5-hour primary windows whose `reset_at` advances before the percentage falls: the expired cycle is closed immediately, carried-over 100% readings are quarantined until a fresh percentage arrives, and already-contaminated open cycles are repaired from retained raw observations as soon as the next fresh percentage arrives.
- Provides an explicit preview/apply repair API for historical false early-reset chains. It preserves raw usage rows and leaves confirmed normal cycles unchanged.
- Adds calendar-month reporting for actual Tokens, selected-basis request value, requests, involved cycles, confirmed resets, early resets, cumulative quota-consumption equivalents, unconsumed quota at reset, and estimated capacity allocated by cycles starting in that month.
- Automatically follows the official CPA or CPAMP panel language, supports Chinese/English manual switching, and remembers the selected mode in the browser.
- Includes a responsive embedded dashboard with dark/light themes and mobile layouts.
- Separates the forecast-cycle selector from the chart range. Changing the forecast cycle updates all selected-cycle statistics and resets the charts to that cycle. A manual chart range may span multiple cycles and changes only chart rendering, while statistics remain scoped to the selected forecast cycle. Each cycle is drawn as a separate segment at its real timestamps, with one x-axis grid interval per day and the forecast cycle highlighted.
- Retains data for 365 days by default and never stores request or response bodies.
- Runs independently of CPA Manager Plus (CPAMP).

## Pricing bases

Choose `api`, `credits`, or `custom` in the dashboard. API and Credits anchor the selected model at its published rate and adjust other models by `adj(model) / adj(anchor)`, where the learner estimates each adjustment relative to that model’s official price shape. Prior-locked adjustments equal 1. The price table displays calculated and official rates plus uncertainty; Fast and long-context factors are compared separately. Custom mode offers input, cache-read, output, and cache-write USD rates for every catalog model, a Fast multiplier, and a long-context toggle and threshold. **Restore official prices** resets the custom draft. These values affect plugin estimates only, never upstream billing.

`POST /pricing-settings` returns HTTP 202 with a task ID. Poll `GET /pricing-settings/task?id=<id>` for event/sample progress; another save receives 409 while a task runs. The in-memory pricing basis switches before the read snapshot, so new requests use it immediately. Historical rows are updated in batches; `recalculating` marks the temporary mixed state. On failure, the old basis and historical values are restored in batches. API aliases `legacy_api` and `current_api` map to `api`; `learned` maps to `credits`. Existing saved settings schedule a background batched migration on upgrade, without delaying plugin registration for repricing.

`GET /pricing-settings` lists `api`, `credits`, and `custom` in `available_modes` alongside the active `pricing_mode`.

## Estimation

For adjacent quota-growth samples in the same quota cycle:

```text
Token-equivalent full-cycle capacity = ΔToken × 100 / Δquota_percent
Pricing-value full-cycle capacity    = Δpricing_value × 100 / Δquota_percent
```

The dashboard uses the median of valid intervals and reports P25–P75 as the uncertainty range. Because quota response headers are usually integer percentages, early estimates can vary significantly and become more stable as percentage coverage increases.

The burn forecast compares elapsed cycle time with consumed quota:

```text
time progress          = elapsed seconds / cycle seconds
cumulative daily pace = used percent / elapsed days
projected reset usage = used percent / time progress
estimated exhaustion  = cycle start + elapsed time × 100 / used percent
```

The green line is the pace that reaches exactly 100% at reset. Purple is the cumulative-average projection. Orange is the recent approximately 24-hour projection, falling back to the cumulative average when recent evidence is insufficient. For an early refill that keeps the same `reset_at`, the new cycle begins at the first confirmed low-usage observation and forecasts over the shortened remaining interval.

### Collection coverage

Quota percentages and reset times describe the entire upstream account quota pool. Tokens, requests, and pricing values describe only traffic recorded through CPA. The dashboard shows these sources separately, together with the latest quota observation time.

Use **Collection coverage for this account** to select a mode and save it for the selected AuthID:

| Mode | Dashboard choice | Capacity conversions |
|---|---|---|
| `cpa_only` | Assume all usage goes through CPA | Enabled under this explicit assumption; this is the default for unconfigured accounts and preserves existing estimates |
| `mixed` | Mixed routes (CPA + direct, etc.) | Disabled because some usage is not collected |
| `unknown` | Coverage unknown | Disabled until collection coverage is established |

The selection persists in SQLite across plugin and browser restarts. It applies to that account's current and historical main, weekly, and Spark views. Saving it changes the visibility and interpretation of estimates without rewriting recorded requests, pricing values, samples, or cycle boundaries. Selecting `cpa_only` again restores the existing calculations from the retained data.

For example, 600,000 CPA Tokens divided by 12 percentage points of account-wide consumption produces a 5,000,000-Token estimate, even if only six of those points came from CPA. The plugin cannot recover the missing direct-route Tokens from percentages. More samples or model-price calibration do not establish collection coverage. **Sample sufficiency** is therefore shown separately from the coverage assumption; it measures usable growth intervals and percentage span, not whether every route was collected.

Mixed and unknown modes keep account quota observations, CPA actual usage, reset history, and percentage-based burn trends. Full-cycle capacity, remaining Token/value capacity, per-model conversions, capacity history, and monthly capacity totals are disabled. Trends describe the whole quota pool's observed historical pace; they can change with the route mix and future usage, and direct activity is only observed when a later CPA request brings fresh headers.

### Cycle and monthly accounting

`reset_at` is the upstream planned reset time and does not by itself prove a reset. A scheduled transition requires two successful observations consistently describing the new window at or after the old boundary. An early reset keeps the old schedule and usage baseline until at least three successful, nondecreasing candidate readings in the same quota regime span 60 seconds. With an unchanged schedule, readings must remain at or below 5%. With a later, previously unseen full-window schedule, readings above 5% may qualify if they remain at most half the old peak and the declared window start does not precede the latest old-plan observation by more than the five-minute schedule tolerance. This covers sequences such as `80% → 2% → 9% → 10%` and first observations already above 5%. Confirmation retains the whole uninterrupted sequence, including across plugin restarts. The new cycle starts at its first real candidate request; no inferred 0% sample is created. Failed, out-of-order, rebounding, or inconsistent observations cannot confirm the candidate. Previously observed schedules are treated as possible regime recoveries even when their reset time moves forward.

When an unused balance expires, the next full window can start on the first later request. A gap longer than five minutes, including multiple idle days, is accepted only after the old deadline, with a compatible plan and window duration and a declared start no later than the observation plus the clock tolerance. Two consistent successful observations are still required; while pending, the old schedule and peak remain intact. The old cycle closes at its original deadline and the new cycle starts at its declared activation time. Requests after the old deadline belong to the new ledger. This also works when the old balance never reached 100%.

An inferred early reset with a later schedule remains subject to correction by subsequent evidence. Before the original planned reset, two consecutive successful observations restoring that original schedule near or above its old peak, without an intervening low refill reading, reopen the previous cycle and rejoin the current cycle in one transaction. A single return leaves the current plan unchanged while confirmation is pending. All raw requests and quota samples are retained, cumulative sample totals are recalculated, and the complete `A → B → A` history remains available for anomaly detection. A real refill that returns to the original schedule at low usage and later grows back to the old peak is not merged. Automatic inference remains conservative when observations cannot distinguish a refill from an upstream correction.

Quota evidence is ordered by when its response headers were observed: request time plus TTFT for streaming requests, or total latency when TTFT is unavailable. Actual Tokens and monthly request attribution continue to use the original request timestamp. Sampling baselines are tracked per declared reset schedule, so a confirmed server-side rollback can lower the current percentage instead of remaining pinned to a temporary higher value. When a repeated alternate schedule later returns to the prior schedule, the API reports an `upstream_regime_reverted` anomaly; the dashboard preserves that interval in red, breaks estimation across both boundaries, and continues forecasting from trustworthy samples. Exact before/start/peak/recovery anchors are reconstructed from retained raw requests. The full-cycle chart draws one thin canonical spike instead of overplotting every dense anomalous sample, while the anomaly card includes a readable detail sparkline. Capacity history remains disconnected through the anomalous interval and resumes at the recovery boundary with the last trustworthy estimate, even when an older plugin version did not sample the lower restored percentages.

Spark has model-specific quota scopes and reset schedules that are independent of the main Codex allowance. A detected 5-hour Spark Primary window remains the Spark 5-hour axis, while its weekly Secondary headers feed a separate `spark_weekly` axis; their percentages, reset cycles, and capacity estimates are never mixed, although both axes attribute actual Tokens and pricing value from the same Spark requests. Older Spark observations that expose only one weekly Primary window retain their single-window behavior. Spark requests are excluded from the main monthly actual Tokens, requests, pricing value, cycle ledger, charts, consumed-quota equivalent, and capacity estimates. If a Spark planned reset is corrected before the old boundary while usage continues to rise, the plugin updates the affected Spark axis instead of creating overlapping cycles or counting the same requests twice. The dashboard hides Spark quota by default; enable **Show Spark quota** at the top to display both detected axes below all main-quota content, each with its own quota, cumulative Token, and cumulative pricing-value curve plus a full monthly cycle table. The weekly curve always spans the complete declared seven-day cycle, with its start and expected reset shown explicitly above the chart. The plugin does not actively poll upstream quota, and the dashboard's **Refresh** button only reloads stored observations. If a scheduled Spark reset passes without another Spark request, the expired cycle is closed at its scheduled boundary and the current window/next reset are projected from the prior schedule. Current usage remains **Awaiting sample** until a successful Spark request returns fresh headers; two consistent successful observations are still required to confirm the new scheduled window.

Calendar-month totals use Asia/Shanghai boundaries:

- actual Tokens and requests are assigned by request timestamp; request pricing value uses the active valuation basis and is assigned by the same timestamp;
- quota-consumption equivalent is the sum of observed percentage growth attributable to the month, so multiple cycles can exceed `100%` or `1.00×`;
- reset counts and unconsumed quota include confirmed scheduled, early, and migrated historical resets;
- estimated monthly capacity sums the median full-cycle estimates for cycles that start in the selected month. It remains a workload-equivalent estimate rather than an official allowance;
- quota-equivalent data is marked as partial when a cycle crosses the month boundary without an earlier sample from which to establish the month-start baseline.

## Installation

### CPA plugin marketplace

Install the latest release directly from **Plugin Management** in the CPA management center. The marketplace follows this repository’s latest compatible GitHub Release.

### Manual installation

Download the zip matching your platform from [GitHub Releases](https://github.com/Autsunset/cpa-quota-estimator/releases). Extract the dynamic library into the matching CPA plugin directory, for example:

```text
plugins/linux/amd64/cpa-quota-estimator.so
plugins/linux/arm64/cpa-quota-estimator.so
plugins/darwin/arm64/cpa-quota-estimator.dylib
plugins/windows/amd64/cpa-quota-estimator.dll
```

Add optional configuration to `config.yaml`:

```yaml
plugins:
  configs:
    cpa-quota-estimator:
      enabled: true
      data_path: /CLIProxyAPI/data/cpa-quota-estimator.sqlite
      sample_interval_minutes: 5
      price_source_url: https://models.dev/catalog.json
      price_sync_interval_minutes: 1440
      pricing_mode: credits # credits (default) | api | custom
      anchor_model: gpt-5.6-sol
      long_context_threshold: 272000
      history_days: 365
      capture_codex_headers: false
      weight_half_life_days: 21
      weight_random_walk_sigma: 0.35
      weight_fit_interval_minutes: 60
  enabled: true
```

Restart CPA. The log should include:

```text
plugin registered plugin_id=cpa-quota-estimator plugin_name=CPA Quota Estimator
```

> **Upgrade safety:** If the active AI client reaches its upstream through New API and CPA, do not stop either service or run `docker compose down` during a plugin upgrade. Keep the services running while downloading, verifying, taking an online database backup, and atomically replacing the plugin file. Then perform only one CPA restart and immediately verify health and plugin-registration logs. Stopping either service can sever the active AI session and prevent the maintenance operation from continuing.

Open **额度容量预测 / Quota Estimator** from CPAMP. The dashboard first tries the authenticated plugin bridge. If the bridge is unavailable in a same-origin deployment, it can reuse the Management Key already persisted by CPAMP's **Remember password** option. Cross-origin deployments and non-persisted CPAMP sessions fall back to CPA Management Key login. Enable **Remember the key in this browser** there to keep the fallback key in this browser's `localStorage`; otherwise it remains only in the current tab's `sessionStorage`. Do not enable persistent storage on a shared device.

> **Cold start:** Installing the plugin does not immediately show quota samples. The plugin is completely passive — it only records requests that actually flow through CPA and cannot reconstruct usage that happened before installation. The all-account overview attempts to merge configured Codex OAuth credentials from CPA's protected `auth-files` management endpoint; credentials that have not yet passed a request through CPA are shown as awaiting their first sample instead of appearing to be missing. Current remaining quota and reset time appear after the first real Codex request, and capacity estimates start only once the recorded used percentage grows between samples (`delta used percentage > 0`). Because quota headers report integer percentages, the first usable estimate may take several requests, and results stabilize as consumption accumulates.

Plugin upgrades migrate SQLite in place. The confirmed historical missed-reset repair is idempotent; the separate false-reset-chain repair remains an explicit management action. Existing saved pricing names migrate to the three current bases, and retained request/sample values are recalculated from raw Tokens. In Docker, persist the directory containing `data_path`—by default `/CLIProxyAPI/data`—with a volume or bind mount.

GPT-6 Astra, Sol, Luna, and GPT-5.6 Sol use verified [OpenAI Standard API rates](https://developers.openai.com/api/docs/pricing) where the catalog lags. The [Codex Credits card](https://learn.chatgpt.com/docs/pricing) gives their separate subscription credit rates. Official API Fast and long-context rates are displayed beside learner-adjusted estimates; API Batch/Flex requests retain their 50% rate. Neither published card alone determines an account’s included subscription quota.

## Token and pricing-value rules

The Token charts use input + output Tokens. Cached Tokens are normally included in input Tokens and are therefore not added again. Pricing-value calculation still applies cache rates independently:

```text
cached read   = max(CacheReadTokens, CachedTokens)
uncached input = max(InputTokens - cached read - cached write, 0)

pricing value = uncached input × input rate
              + cached read × cache-read rate
              + cached write × cache-write rate
              + output × output rate
```

The active rate is per one million Tokens and is denominated in USD (`api`/`custom`) or Codex Credits (`credits`). `ReasoningTokens` are already included in output Tokens and are not charged twice. The learner supplies identifiable model, Fast, and long-context adjustments; cache/output ratios retain each model’s published shape.

When Fast or long context is calibrated, its learned multiplier replaces the official tier multiplier on the calculated Standard rate. A factor locked to its prior uses the official tier instead. The long-context column shows the calculated and official input multipliers plus the learned uncertainty when available. Scheduled weight updates refit hourly; rolling backtests run at most daily or on explicit calibration.

- `api`: current [official API USD rates](https://developers.openai.com/api/docs/pricing), with verified overrides where the catalog lags. Select an anchor model; its calculated Standard rate is exactly its official rate.
- `credits`: [published Codex Credits rates](https://learn.chatgpt.com/docs/pricing), with the same anchor rule. The published card has no separate cache-write charge or long-context price. Models without a listed credit rate use an API-derived estimate.
- `custom`: editable USD input/cache-read/output/cache-write rates per model, Fast multiplier, and long-context toggle/threshold (default 272,000 Tokens). It starts from official API rates and ignores learner price adjustments.

When a pricing task runs, the dashboard disables the controls and polls progress. A separate read-only connection computes a snapshot, then 500-row transactions update events and per-cycle sample prefix sums while releasing the writer connection between batches. New events accepted during the task already use the target basis; affected cycle samples receive a final catch-up pass. The API and dashboard mark the task as `recalculating` because historical charts may briefly mix old and new values. Failed tasks restore the old settings and values. Usage writes retry busy/timeout failures for about 30 seconds; persistent failures increment `metadata.dropped_usage_events` and appear in the dashboard. Fit-driven repricing requires a >2% change in an active factor and runs at most once every six hours; custom never triggers it. WAL checkpoints run on a separate connection. JSON fields ending in `_cost_usd` are retained for compatibility; `pricing_mode` and `value_unit` give their actual unit.

For each selected primary cycle—and for the independent weekly cycle when detected—the dashboard lists remaining uncached-input, output, and cache-hit Tokens for supported Codex models. Each column is a separate hypothetical: it assumes all remaining pricing value is spent only on that model and Token category, using Standard and base-context rates.

## Management API

All management routes are protected by CPA Management Key:

| Method | Path | Purpose |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/overview` | Current primary and detected weekly-quota overview for every recorded account |
| GET | `/v0/management/cpa-quota-estimator/usage?account=<AuthID>&days=7` | Recent per-model usage for 1, 7, or 30 days, with official-rate credit references and account-wide quota growth |
| GET | `/v0/management/cpa-quota-estimator/weights` | Latest learned model weights, intervals, factors, and diagnostics |
| GET | `/v0/management/cpa-quota-estimator/weights/backtest` | Latest rolling-backtest comparison with lag diagnostics |
| GET | `/v0/management/cpa-quota-estimator/calibration/options` | Available models and currently prior-locked target models |
| GET | `/v0/management/cpa-quota-estimator/calibration?account=<AuthID>` | Guided calibration state, progress, contamination, and request-ID spans |
| POST | `/v0/management/cpa-quota-estimator/calibration/start` | Start a same-cycle passive comparison for an account and model pair |
| POST | `/v0/management/cpa-quota-estimator/calibration/end` | End the current phase; completion triggers a learner refit |
| POST | `/v0/management/cpa-quota-estimator/calibration/cancel` | Cancel an active calibration session |
| GET | `/v0/management/cpa-quota-estimator/summary` | Selected quota-cycle and forecast summary |
| GET | `/v0/management/cpa-quota-estimator/series` | Selected quota-cycle chart samples |
| GET | `/v0/management/cpa-quota-estimator/monthly` | Calendar-month usage, reset, and capacity summary |
| GET | `/v0/management/cpa-quota-estimator/repair/early-resets` | Preview historical false early-reset candidates without changing data |
| POST | `/v0/management/cpa-quota-estimator/repair/early-resets` | Transactionally merge all currently detected candidates |
| GET | `/v0/management/cpa-quota-estimator/prices` | Official and calculated model price rows with adjustment intervals |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | Trigger an immediate models.dev sync |
| GET | `/v0/management/cpa-quota-estimator/pricing-settings` | Read the saved basis, anchor, and custom prices |
| POST | `/v0/management/cpa-quota-estimator/pricing-settings` | Start a batched background repricing task; returns HTTP 202 and task ID |
| GET | `/v0/management/cpa-quota-estimator/pricing-settings/task?id=<id>` | Poll repricing progress and completion status |
| GET | `/v0/management/cpa-quota-estimator/coverage-settings?account=<AuthID>` | Read the account's collection mode and capacity assumption |
| POST | `/v0/management/cpa-quota-estimator/coverage-settings?account=<AuthID>` | Save `{"mode":"cpa_only"}`, `{"mode":"mixed"}`, or `{"mode":"unknown"}` without rewriting usage |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | Embedded dashboard resource |

`overview` returns one lightweight current-cycle record per sampled account, including plan type, remaining quota, requests, Tokens, pricing value, full-cycle Token/pricing-value capacity estimates, confidence, and burn forecast. Primary-quota fields stay at the account level; when a 5-hour Primary plus weekly Secondary pair is detected, the record also contains `five_hour_quota_detected: true` and an independent `weekly_quota` snapshot. The response-level `pricing_mode` and `value_unit` identify whether compatibility fields ending in `_cost_usd` currently hold USD or Credits.

The dashboard additionally reads CPA's Codex OAuth inventory and merges credentials without samples into the same table. Exact `id` and `name` aliases are matched without exposing secrets; disabled and unavailable credentials are shown separately from enabled credentials awaiting their first sample. For dual-quota accounts, the remaining, reset, capacity, confidence, and forecast cells show both the 5-hour and weekly scopes while sorting uses the most constrained or most urgent scope. The overview can be collapsed, every column has its own client-side filter, and selecting a column heading toggles type-aware ascending/descending sorting. Drag a heading boundary to resize that column from the 40 px technical minimum up to 2000 px, or double-click the resize handle to restore its default width; keyboard users can focus the separator and use the arrow keys (`Shift` for a larger step) or `Home` to reset it. Number and time filters accept `>`, `>=`, `<`, `<=`, and `=` comparisons; view preferences and column widths persist in the current browser. Selecting a sampled row opens that account in the existing detailed view without issuing AI requests. If neither the restricted parent bridge nor a reusable Management Key can read the inventory, sampled accounts remain available and the dashboard explicitly reports that the OAuth inventory is unavailable.

Use `?account=<AuthID>` to select a credential, `?cycle_id=<ID>` on `summary` or `series` to select the forecast cycle, and `?month=YYYY-MM` on `monthly` to select a month. On `series`, pass Unix-second `?start_at=<timestamp>&end_at=<timestamp>` values to return chart samples and capacity trajectories across every quota cycle overlapping that range.

`summary`, `series`, monthly summaries, and overview account records expose `collection_coverage`: `mode`, `configured`, `usage_source: "cpa"`, `quota_source: "account_quota_pool"`, and `capacity_estimation_enabled`. The default has `configured: false` and `assumption: "all_usage_through_cpa"`; this is an assumption, not verified coverage. Estimates also expose `coverage_mode`, `assumption`, and `sample_confidence`. In mixed/unknown modes, `available` is false, `confidence` is `"unavailable"`, and `unavailable_reason` is `"partial_usage_collection"` or `"usage_coverage_unknown"`. Capacity amounts and ranges serialize as JSON `null`, capacity-history arrays are empty, and model allowances are cleared. `sample_confidence` and sample counts remain available independently. Monthly capacity fields follow the same policy; `quota_coverage_complete` still refers only to month-boundary baseline coverage, not collection of external routes.

`summary` and `series` include `remaining_by_model`, while an automatically detected `weekly_quota` includes its own list. Pricing settings expose `pricing_mode` and `value_unit`; changing the mode through `POST /pricing-settings` returns a task ID immediately, and `/pricing-settings/task` reports when historical recalculation completes.

For an account whose latest valid primary observation is a 5-hour window and also contains a larger Secondary window, `summary`, `series`, and `monthly` return `five_hour_quota_detected: true`. `series` then automatically includes an independent `weekly_quota`, and `monthly` includes `weekly_summary`; no opt-in query parameter is needed. Weekly calculations use only requests carrying a detected 5-hour primary window, so weekly-only Pro accounts retain the original primary-only response shape and accounting.

`summary` and `series` expose confirmed temporary main-quota state changes as `quota_anomalies`. Each item records `before_at`, `started_at`, `peak_at`, `ended_at`, the corresponding `before_used_percent`, `anomalous_used_percent`, `peak_used_percent`, and `restored_used_percent`, all reset schedules, and the observation count. `series` also returns `range_quota_anomalies` for the requested chart range. Anomalous points carry `anomalous: true`, and the first trustworthy quota and capacity-history segments after an anomaly carry `break_before: true`; consumers should not derive capacity across those boundaries.

Pass `include_spark=1` on `series` to include the latest independent Spark Primary cycle as `spark_quota`. When the current Spark response shape is a 5-hour Primary plus weekly Secondary pair, the response also returns `spark_five_hour_quota_detected: true` and an independent `spark_weekly_quota`. The same parameter on `monthly` returns `spark_summary` and, for the dual-axis shape, `spark_weekly_summary`. Both summaries use only Spark requests and never mix with the main quota. The dashboard sends this parameter only when its Spark display switch is enabled.

Historical repair requires `?account=<AuthID>`. Always call GET first and inspect the returned cycle IDs. A candidate must be part of a chain containing at least two adjacent suspicious `early_reset` boundaries and keep the same primary reset schedule across each boundary. Each boundary must either be caused by a separately scoped Spark low reading followed within 10 minutes by the primary quota continuing near its preceding peak, or begin with a primary low reading that fails the new confirmation rule and quickly rebounds. Isolated early resets are deliberately left unchanged. POST revalidates every boundary and applies the full candidate set in one transaction; any failure rolls back all merges. Raw `usage_events` and overall Token, cost, and request totals are preserved. Spark rows are detached from the primary cycle ledger, and only their erroneous primary-quota samples and artificial boundaries are removed. Take an online SQLite backup before POST.

## Build

Requires Go 1.22+, GCC, and CGO:

```bash
make test
make build
make package VERSION=0.15.0
```

`make package` produces a marketplace-compatible zip and `checksums.txt` under `dist/`. Tagged releases are built for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by GitHub Actions.

The optional browser integration test requires Node.js 22+ and Chrome or Chromium. It covers both languages, mobile layout, mixed/unknown modes, account isolation, saved preferences, failed saves/refreshes, and stale responses during account switching:

```bash
CPA_BROWSER_TESTS=1 go test -run TestCoverageBrowser -v ./...
```

Set `CPA_BROWSER_ARTIFACT_DIR` to retain screenshots in a chosen directory.

## Privacy

The SQLite database contains credential identifiers, model names, Token counts, selected-basis pricing values, failure status, and quota metadata. It does **not** store prompts, request bodies, or response bodies. The default retention period is 365 days.

## Optional CPAMP authentication bridge

The plugin does not require CPAMP. A custom CPAMP deployment can optionally reuse its existing login state through a restricted `postMessage` bridge; see [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md). The dashboard automatically falls back when the bridge is absent.

## Acknowledgements

Thanks to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the underlying proxy capabilities and native plugin system.

Thanks to the [Linux.do community](https://linux.do/) for testing, feedback, and technical discussion.
