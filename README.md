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
- **Independent quota scopes:** automatically separate a detected 5-hour Primary quota from its weekly Secondary quota, while keeping `gpt-5.3-codex-spark` usage in a completely independent ledger.
- **Capacity in practical units:** estimate full-cycle and remaining capacity in Tokens and the selected pricing basis—current API prices, pre-discount API prices, or subscription Credits—with uncertainty ranges and confidence levels.
- **Actionable forecasts:** compare actual usage with a sustainable baseline, cumulative-average pace, and recent pace to estimate exhaustion time and whether a quota will survive until reset.
- **History that survives resets:** retain confirmed cycles, cross-cycle charts, calendar-month reports, quota-consumption equivalents, reset counts, and unused quota at reset.
- **Passive and private by design:** consume no extra quota, store no prompts or response bodies, and keep retained usage metadata in a local SQLite database.

## Detailed features

- Provides an all-account quota overview with current remaining quota, reset state, requests, Tokens, pricing value, full-cycle capacity, confidence, and burn forecast. For detected dual-quota accounts, the same row shows both 5-hour and weekly status; selecting a sampled row opens the existing detailed forecast.
- Listens to CPA's native `usage.handle`; it does not send probe requests or consume additional quota.
- Persists Token counts, model, `service_tier`, selected-basis pricing value, and `X-Codex-Primary-*` quota metadata in a private SQLite database.
- Syncs OpenAI model pricing from `https://models.dev/catalog.json` by default.
- Accounts for cached reads/writes, output Tokens, and the context pricing tier above 272K input Tokens.
- Provides three persistent pricing bases: current API prices, pre-discount API prices, and subscription Credits. Subscription Credits deliberately use the non-promotional Codex rate card (`pre-discount price × 25`) and never use temporary API/purchased-credit discounts. Saving a basis or surcharge switch transactionally recalculates every retained request, current and historical quota-cycle sample, monthly total, and pricing-value-equivalent capacity estimate; switching back always recalculates from raw Token fields.
- Provides a separate **Model quota calibration** always-visible Astra multiplier control (default **1.8×**, range **0.01–100**; set **1×** for no calibration); official/catalog base prices remain unchanged ($10 input, $1 cache read, $50 output per million Tokens). The dashboard shows base prices, the model multiplier, and effective rates ($18/$1.8/$90), and includes Astra in remaining-Token allowances. The multiplier applies once in all pricing modes, on top of the existing optional Fast/context rules. On upgrade, Astra request values and affected cycle samples are transactionally recalculated from raw Tokens; other models are unchanged. This is a provisional workload calibration against Sol, not an official price increase.
- Supports configurable Fast pricing:
  - `multiplier`: multiply normal/long-context pricing, default **2.5×**;
  - `source`: use explicit `experimental.modes.fast.cost` pricing from models.dev.
- Estimates full-cycle and remaining-quota Token/pricing-value-equivalent capacity with interquartile ranges and confidence levels, and converts the selected cycle’s remaining value into per-model uncached-input, output, and cache-hit Token allowances.
- Shows actual quota usage, a sustainable baseline, cumulative-average projection, recent-rate projection, predicted exhaustion, planned reset time, and countdown.
- Maintains an independent ledger for every confirmed quota cycle. Historical cycles remain available in the selector after a reset.
- Treats `gpt-5.3-codex-spark` headers as a separate quota scope and cycle. Spark actual Tokens, requests, pricing value, quota equivalents, cycle capacity, and monthly summaries are all accounted for separately; they never create, split, estimate, or add to the primary quota. An optional switch at the top of the dashboard displays the complete Spark statistics below all primary-quota content.
- When—and only when—the latest primary quota window is detected as approximately 5 hours and valid `X-Codex-Secondary-*` headers are present, treats the primary window as the independent 5-hour quota and the Secondary window as the independent weekly quota. Each gets its own percentage trajectory, reset cycles, Token/pricing-value-equivalent capacity, monthly quota equivalents, and dashboard section. Accounts without a detected 5-hour primary window, including weekly-only Pro accounts, keep the existing single-window behavior and never expose or calculate this secondary weekly section.
- Orders quota headers by their response observation time, confirms scheduled transitions with two consistent successful observations, and confirms same-`reset_at` early resets only after three stable successful low-usage observations spanning at least 60 seconds.
- Handles exhausted 5-hour primary windows whose `reset_at` advances before the percentage falls: the expired cycle is closed immediately, carried-over 100% readings are quarantined until a fresh percentage arrives, and already-contaminated open cycles are repaired from retained raw observations as soon as the next fresh percentage arrives.
- Provides an explicit preview/apply repair API for historical false early-reset chains. It preserves raw usage rows and leaves confirmed normal cycles unchanged.
- Adds calendar-month reporting for actual Tokens, selected-basis request value, requests, involved cycles, confirmed resets, early resets, cumulative quota-consumption equivalents, unconsumed quota at reset, and estimated capacity allocated by cycles starting in that month.
- Automatically follows the official CPA or CPAMP panel language, supports Chinese/English manual switching, and remembers the selected mode in the browser.
- Includes a responsive embedded dashboard with dark/light themes and mobile layouts.
- Separates the forecast-cycle selector from the chart range. Changing the forecast cycle updates all selected-cycle statistics and resets the charts to that cycle. A manual chart range may span multiple cycles and changes only chart rendering, while statistics remain scoped to the selected forecast cycle. Each cycle is drawn as a separate segment at its real timestamps, with one x-axis grid interval per day and the forecast cycle highlighted.
- Retains data for 365 days by default and never stores request or response bodies.
- Runs independently of CPA Manager Plus (CPAMP).

## Model quota calibration

In **Pricing rules**, edit the always-visible **Model quota calibration → Astra ×** field (default **1.8**). It works with **current API prices, pre-discount API prices, and Credits**. Set **1** for no multiplier—there is no separate switch. Click **Save and recalculate** to persist the value and transactionally rebuild historical estimates, cycle capacity, and remaining-Token allowances. Switching pricing bases or restarting CPA preserves the multiplier. The dashboard saves `astra_multiplier` with `apply_model_calibration: true` through `POST /pricing-settings`; the former boolean remains accepted for older API clients, and a previously disabled setting appears as **1×** in the new UI.

This setting affects **plugin estimates only**. It never changes catalog prices, raw Tokens, quota percentages, or **New API billing**. Official/base rates remain visible alongside the applied multiplier and effective rates.

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

### Cycle and monthly accounting

`reset_at` is treated as the upstream planned reset time, not by itself as proof that a reset has already occurred. A scheduled transition is accepted only after two successful observations consistently describe the new window at the old boundary. If the planned time remains unchanged but used quota abruptly returns near 0%, the plugin requires three successful low readings in the same quota regime, in nondecreasing order, spanning at least 60 seconds. Failed, stale, out-of-order, or quickly rebounding readings do not create a new cycle.

Quota evidence is ordered by when its response headers were observed: request time plus TTFT for streaming requests, or total latency when TTFT is unavailable. Actual Tokens and monthly request attribution continue to use the original request timestamp.

Spark has a model-specific quota scope and reset schedule that are independent of the primary Codex allowance. Spark requests are excluded from the primary monthly actual Tokens, requests, pricing value, cycle ledger, charts, consumed-quota equivalent, and capacity estimates. If Spark's planned reset time is corrected before the old boundary while usage continues to rise, the plugin updates the current Spark cycle instead of creating overlapping cycles or counting the same requests twice. The dashboard hides Spark quota by default; enable **Show Spark quota** at the top to display, below all primary-quota content, the latest Spark cycle curve, full-cycle and remaining Token/pricing-value-equivalent capacity, sample quality, monthly summary, and cycle details. Every Spark figure is calculated only from Spark headers and Spark requests. The plugin does not actively poll upstream quota, and the dashboard's **Refresh** button only reloads stored observations. If a scheduled Spark reset passes without another Spark request, the expired cycle is closed at its scheduled boundary and the current window/next reset are projected from the prior schedule. Current usage remains **Awaiting sample** until a successful Spark request returns fresh headers; two consistent successful observations are still required to confirm the new scheduled window.

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
      fast_pricing_mode: multiplier
      fast_multiplier: 2.5
      pricing_mode: current_api # current_api | legacy_api | credits
      apply_fast_pricing: true
      astra_multiplier: 1.8
      long_context_threshold: 272000
      apply_long_context_pricing: false
      history_days: 365
  enabled: true
```

Restart CPA. The log should include:

```text
plugin registered plugin_id=cpa-quota-estimator plugin_name=CPA Quota Estimator
```

> **Upgrade safety:** If the active AI client reaches its upstream through New API and CPA, do not stop either service or run `docker compose down` during a plugin upgrade. Keep the services running while downloading, verifying, taking an online database backup, and atomically replacing the plugin file. Then perform only one CPA restart and immediately verify health and plugin-registration logs. Stopping either service can sever the active AI session and prevent the maintenance operation from continuing.

Open **额度容量预测 / Quota Estimator** from CPAMP. The dashboard first tries the authenticated plugin bridge. If the bridge is unavailable in a same-origin deployment, it can reuse the Management Key already persisted by CPAMP's **Remember password** option. Cross-origin deployments and non-persisted CPAMP sessions fall back to CPA Management Key login. Enable **Remember the key in this browser** there to keep the fallback key in this browser's `localStorage`; otherwise it remains only in the current tab's `sessionStorage`. Do not enable persistent storage on a shared device.

> **Cold start:** Installing the plugin does not immediately show quota samples. The plugin is completely passive — it only records requests that actually flow through CPA and cannot reconstruct usage that happened before installation. The all-account overview attempts to merge configured Codex OAuth credentials from CPA's protected `auth-files` management endpoint; credentials that have not yet passed a request through CPA are shown as awaiting their first sample instead of appearing to be missing. Current remaining quota and reset time appear after the first real Codex request, and capacity estimates start only once the recorded used percentage grows between samples (`delta used percentage > 0`). Because quota headers report integer percentages, the first usable estimate may take several requests, and results stabilize as consumption accumulates.

Plugin upgrades migrate the SQLite schema in place and do not intentionally clear usage history. Upgrading does not generally rewrite historical cycles; the sole automatic boundary repair is the targeted exhausted-5-hour carry-over fix, which runs only when the next fresh primary percentage proves that an open cycle spans an expired reset. Saving the dashboard pricing switches intentionally recalculates historical pricing values and derived capacity estimates, but does not alter Token counts or other quota-cycle boundaries. Historical false early resets are changed only through the explicit repair POST described below. In Docker, persist the directory containing `data_path`—the default is `/CLIProxyAPI/data`—with a volume or bind mount; replacing a container without that mount also replaces its local database.

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

The active rate is per one million Tokens and is denominated in either USD or subscription Credits. `ReasoningTokens` are already included in output Tokens and are not charged twice. Requests whose recorded `service_tier` is `priority` or `fast` use the configured Fast policy; `auto` and `default` remain at 1×. The long-context tier is selected when `InputTokens > long_context_threshold` (272,000 by default).

The dashboard pricing selector provides:

- `current_api`: current models.dev/API prices, including active discounts;
- `legacy_api`: pre-discount API-equivalent prices; GPT-5.6 Sol/Terra/Luna use `$5/$0.50/$30`, `$2.50/$0.25/$15`, and `$1/$0.10/$6` for input/cache-hit/output;
- `credits`: subscription, non-promotional Codex Credits, calculated as the pre-discount rate × 25; therefore Sol/Terra/Luna use `125/12.5/750`, `62.5/6.25/375`, and `25/2.5/150` Credits per one million input/cache-hit/output Tokens. Temporary purchased-credit discounts are intentionally excluded, and cache writes are assigned zero Credits according to the subscription rate-card treatment.

The dashboard also exposes **>272K long-context surcharge** and **Fast surcharge** switches. **Save and recalculate** persists all three settings and transactionally rebuilds every retained `usage_events.cost_usd` compatibility value plus all quota-sample cumulative values. The selected current or historical cycle, cross-cycle charts, 5-hour and weekly quota panels, and monthly summaries then use the same basis. Switching back recalculates from raw input/output/cache Token fields rather than converting the prior result. JSON field names containing `_cost_usd` are retained for API compatibility; `pricing_mode` and `value_unit` identify whether their active value is USD or Credits.

For each selected primary cycle—and for the independent weekly cycle when detected—the dashboard lists remaining uncached-input, output, and cache-hit Tokens for supported Codex models. Each column is a separate hypothetical: it assumes all remaining pricing value is spent only on that model and Token category, using Standard and base-context rates.

## Management API

All management routes are protected by CPA Management Key:

| Method | Path | Purpose |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/overview` | Current primary and detected weekly-quota overview for every recorded account |
| GET | `/v0/management/cpa-quota-estimator/summary` | Selected quota-cycle and forecast summary |
| GET | `/v0/management/cpa-quota-estimator/series` | Selected quota-cycle chart samples |
| GET | `/v0/management/cpa-quota-estimator/monthly` | Calendar-month usage, reset, and capacity summary |
| GET | `/v0/management/cpa-quota-estimator/repair/early-resets` | Preview historical false early-reset candidates without changing data |
| POST | `/v0/management/cpa-quota-estimator/repair/early-resets` | Transactionally merge all currently detected candidates |
| GET | `/v0/management/cpa-quota-estimator/prices` | Synced prices and Fast policy |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | Trigger an immediate models.dev sync |
| GET | `/v0/management/cpa-quota-estimator/pricing-settings` | Read the saved pricing basis plus long-context and Fast switches |
| POST | `/v0/management/cpa-quota-estimator/pricing-settings` | Save the pricing basis and switches, then transactionally recalculate all retained historical cycle values |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | Embedded dashboard resource |

`overview` returns one lightweight current-cycle record per sampled account, including plan type, remaining quota, requests, Tokens, pricing value, full-cycle Token/pricing-value capacity estimates, confidence, and burn forecast. Primary-quota fields stay at the account level; when a 5-hour Primary plus weekly Secondary pair is detected, the record also contains `five_hour_quota_detected: true` and an independent `weekly_quota` snapshot. The response-level `pricing_mode` and `value_unit` identify whether compatibility fields ending in `_cost_usd` currently hold USD or Credits.

The dashboard additionally reads CPA's Codex OAuth inventory and merges credentials without samples into the same table. Exact `id` and `name` aliases are matched without exposing secrets; disabled and unavailable credentials are shown separately from enabled credentials awaiting their first sample. For dual-quota accounts, the remaining, reset, capacity, confidence, and forecast cells show both the 5-hour and weekly scopes while sorting uses the most constrained or most urgent scope. The overview can be collapsed, every column has its own client-side filter, and selecting a column heading toggles type-aware ascending/descending sorting. Drag a heading boundary to resize that column from the 40 px technical minimum up to 2000 px, or double-click the resize handle to restore its default width; keyboard users can focus the separator and use the arrow keys (`Shift` for a larger step) or `Home` to reset it. Number and time filters accept `>`, `>=`, `<`, `<=`, and `=` comparisons; view preferences and column widths persist in the current browser. Selecting a sampled row opens that account in the existing detailed view without issuing AI requests. If neither the restricted parent bridge nor a reusable Management Key can read the inventory, sampled accounts remain available and the dashboard explicitly reports that the OAuth inventory is unavailable.

Use `?account=<AuthID>` to select a credential, `?cycle_id=<ID>` on `summary` or `series` to select the forecast cycle, and `?month=YYYY-MM` on `monthly` to select a month. On `series`, pass Unix-second `?start_at=<timestamp>&end_at=<timestamp>` values to return chart samples and capacity trajectories across every quota cycle overlapping that range.

`summary` and `series` include `remaining_by_model`, while an automatically detected `weekly_quota` includes its own list. Pricing settings expose `pricing_mode` and `value_unit`; changing the mode through `POST /pricing-settings` recalculates all retained historical cycles before the response succeeds.

For an account whose latest valid primary observation is a 5-hour window and also contains a larger Secondary window, `summary`, `series`, and `monthly` return `five_hour_quota_detected: true`. `series` then automatically includes an independent `weekly_quota`, and `monthly` includes `weekly_summary`; no opt-in query parameter is needed. Weekly calculations use only requests carrying a detected 5-hour primary window, so weekly-only Pro accounts retain the original primary-only response shape and accounting.

Pass `include_spark=1` on `series` to include the latest independent Spark quota cycle, its scoped cumulative Token/cost points, sample quality, and capacity estimate. Pass the same parameter on `monthly` to return a separate `spark_summary`, whose actual usage, quota equivalents, capacity, and cycle details never mix with the primary quota. The dashboard sends this only when its Spark display switch is enabled.

Historical repair requires `?account=<AuthID>`. Always call GET first and inspect the returned cycle IDs. A candidate must be part of a chain containing at least two adjacent suspicious `early_reset` boundaries and keep the same primary reset schedule across each boundary. Each boundary must either be caused by a separately scoped Spark low reading followed within 10 minutes by the primary quota continuing near its preceding peak, or begin with a primary low reading that fails the new confirmation rule and quickly rebounds. Isolated early resets are deliberately left unchanged. POST revalidates every boundary and applies the full candidate set in one transaction; any failure rolls back all merges. Raw `usage_events` and overall Token, cost, and request totals are preserved. Spark rows are detached from the primary cycle ledger, and only their erroneous primary-quota samples and artificial boundaries are removed. Take an online SQLite backup before POST.

## Build

Requires Go 1.22+, GCC, and CGO:

```bash
make test
make build
make package VERSION=0.10.1
```

`make package` produces a marketplace-compatible zip and `checksums.txt` under `dist/`. Tagged releases are built for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by GitHub Actions.

## Privacy

The SQLite database contains credential identifiers, model names, Token counts, selected-basis pricing values, failure status, and quota metadata. It does **not** store prompts, request bodies, or response bodies. The default retention period is 365 days.

## Optional CPAMP authentication bridge

The plugin does not require CPAMP. A custom CPAMP deployment can optionally reuse its existing login state through a restricted `postMessage` bridge; see [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md). The dashboard automatically falls back when the bridge is absent.

## Acknowledgements

Thanks to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the underlying proxy capabilities and native plugin system.

Thanks to the [Linux.do community](https://linux.do/) for testing, feedback, and technical discussion.
