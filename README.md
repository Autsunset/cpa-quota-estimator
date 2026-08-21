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

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) usage plugin that records actual Codex quota cycles, maps quota changes to estimated Token and USD-equivalent capacity, and summarizes usage across scheduled or early official resets.

> OpenAI does not publish a fixed Token or USD value for the Codex weekly quota. The estimates shown here are workload-equivalent capacity estimates, not official plan face values.

<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/docs/images/dashboard-en.png" alt="Quota Estimator dashboard (English)" width="720">
</p>

## Features

- Listens to CPA's native `usage.handle`; it does not send probe requests or consume additional quota.
- Persists Token counts, model, `service_tier`, estimated cost, and `X-Codex-Primary-*` quota metadata in a private SQLite database.
- Syncs OpenAI model pricing from `https://models.dev/catalog.json` by default.
- Accounts for cached reads/writes, output Tokens, and the context pricing tier above 272K input Tokens.
- Provides two dashboard pricing switches, with an empirical default of long-context surcharge off and Fast surcharge on. Saving either switch persists it in SQLite and transactionally recalculates all historical request costs, quota samples, monthly totals, and USD-equivalent capacity estimates.
- Supports configurable Fast pricing:
  - `multiplier`: multiply normal/long-context pricing, default **2.5×**;
  - `source`: use explicit `experimental.modes.fast.cost` pricing from models.dev.
- Estimates full-cycle and remaining-quota Token/USD-equivalent capacity with interquartile ranges and confidence levels.
- Shows actual quota usage, a sustainable baseline, cumulative-average projection, recent-rate projection, predicted exhaustion, planned reset time, and countdown.
- Maintains an independent ledger for every confirmed quota cycle. Historical cycles remain available in the selector after a reset.
- Treats `gpt-5.3-codex-spark` headers as a separate quota scope and cycle. Spark actual Tokens, requests, cost, quota equivalents, cycle capacity, and monthly summaries are all accounted for separately; they never create, split, estimate, or add to the primary quota. An optional switch at the top of the dashboard displays the complete Spark statistics below all primary-quota content.
- Orders quota headers by their response observation time, confirms scheduled transitions with two consistent successful observations, and confirms same-`reset_at` early resets only after three stable successful low-usage observations spanning at least 60 seconds.
- Provides an explicit preview/apply repair API for historical false early-reset chains. It preserves raw usage rows and leaves confirmed normal cycles unchanged.
- Adds calendar-month reporting for actual Tokens, estimated request cost, requests, involved cycles, confirmed resets, early resets, cumulative quota-consumption equivalents, unconsumed quota at reset, and estimated capacity allocated by cycles starting in that month.
- Automatically follows the official CPA or CPAMP panel language, supports Chinese/English manual switching, and remembers the selected mode in the browser.
- Includes a responsive embedded dashboard with dark/light themes and mobile layouts.
- Separates the forecast-cycle selector from the chart range. Changing the forecast cycle updates all selected-cycle statistics and resets the charts to that cycle. A manual chart range may span multiple cycles and changes only chart rendering, while statistics remain scoped to the selected forecast cycle. Each cycle is drawn as a separate segment at its real timestamps, with one x-axis grid interval per day and the forecast cycle highlighted.
- Retains data for 365 days by default and never stores request or response bodies.
- Runs independently of CPA Manager Plus (CPAMP).

## Estimation

For adjacent quota-growth samples in the same quota cycle:

```text
Token-equivalent full-cycle capacity = ΔToken × 100 / Δquota_percent
USD-equivalent full-cycle capacity   = Δcost × 100 / Δquota_percent
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

Spark has a model-specific quota scope and reset schedule that are independent of the primary Codex allowance. Spark requests are excluded from the primary monthly actual Tokens, requests, estimated cost, cycle ledger, charts, consumed-quota equivalent, and capacity estimates. If Spark's planned reset time is corrected before the old boundary while usage continues to rise, the plugin updates the current Spark cycle instead of creating overlapping cycles or counting the same requests twice. The dashboard hides Spark quota by default; enable **Show Spark quota** at the top to display, below all primary-quota content, the latest Spark cycle curve, full-cycle and remaining Token/USD-equivalent capacity, sample quality, monthly summary, and cycle details. Every Spark figure is calculated only from Spark headers and Spark requests. The plugin does not actively poll upstream quota, and the dashboard's **Refresh** button only reloads stored observations. If a scheduled Spark reset passes without another Spark request, the expired cycle is closed at its scheduled boundary and the current window/next reset are projected from the prior schedule. Current usage remains **Awaiting sample** until a successful Spark request returns fresh headers; two consistent successful observations are still required to confirm the new scheduled window.

Calendar-month totals use Asia/Shanghai boundaries:

- actual Tokens and requests are assigned by request timestamp; request cost is estimated from models.dev pricing and assigned by the same timestamp;
- quota-consumption equivalent is the sum of observed percentage growth attributable to the month, so multiple cycles can exceed `100%` or `1.00×`;
- reset counts and unconsumed quota include confirmed scheduled, early, and migrated historical resets;
- estimated monthly capacity sums the median full-cycle estimates for cycles that start in the selected month. It remains a workload-equivalent estimate rather than an official allowance;
- quota-equivalent data is marked as partial when a cycle crosses the month boundary without an earlier sample from which to establish the month-start baseline.

## Installation

### CPA plugin marketplace

After this plugin is accepted into the official registry, install it from **Plugin Management** in the CPA management center.

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
      apply_fast_pricing: true
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

> **Cold start:** Installing the plugin does not immediately show your quota. The plugin is completely passive — it only records requests that actually flow through CPA and cannot reconstruct usage that happened before installation. The current quota percentage and reset time appear only after the first real Codex request, and capacity estimates start only once quota usage actually grows between recorded samples (Δquota_percent > 0). Because quota headers report integer percentages, the first usable estimate may take several requests, and results stabilize as consumption accumulates.

Plugin upgrades migrate the SQLite schema in place and do not intentionally clear usage history. Upgrading does not automatically rewrite historical cycles. Saving the dashboard pricing switches intentionally recalculates historical USD costs and derived capacity estimates, but does not alter Token counts or quota-cycle boundaries. Historical false early resets are changed only through the explicit repair POST described below. In Docker, persist the directory containing `data_path`—the default is `/CLIProxyAPI/data`—with a volume or bind mount; replacing a container without that mount also replaces its local database.

## Token and cost rules

The Token charts use input + output Tokens. Cached Tokens are normally included in input Tokens and are therefore not added again. Cost calculation still applies cache pricing independently:

```text
cached read   = max(CacheReadTokens, CachedTokens)
uncached input = max(InputTokens - cached read - cached write, 0)

cost = uncached input × input price
     + cached read × cache-read price
     + cached write × cache-write price
     + output × output price
```

All prices are USD per one million Tokens. `ReasoningTokens` are already included in output Tokens and are not charged twice. Requests whose recorded `service_tier` is `priority` or `fast` use the configured Fast pricing policy; `auto` and `default` remain at 1×. The long-context tier is selected when `InputTokens > long_context_threshold` (272,000 by default).

The dashboard exposes **>272K long-context surcharge** and **Fast surcharge** checkboxes. The empirical defaults are **long-context surcharge off** and **Fast surcharge on**, based on observed Codex quota-percentage consumption; both remain user-configurable. These defaults target ChatGPT/Codex quota-percentage accounting, not API invoice pricing: OpenAI API requests above the model's long-context threshold can still use the published long-context price tier, so users who want strict API-price-equivalent accounting can enable the long-context switch. **Save and recalculate** stores the selection in the plugin SQLite database and recalculates every retained `usage_events.cost_usd` row plus cumulative quota-sample costs in one transaction, so historical charts, monthly summaries, and USD-equivalent capacity estimates update immediately. Saved dashboard values take precedence over the `apply_fast_pricing` and `apply_long_context_pricing` YAML initial values for the same database. Disabling a switch removes only that surcharge; it does not change recorded Tokens, quota percentages, or cycle boundaries.

## Management API

All management routes are protected by CPA Management Key:

| Method | Path | Purpose |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/summary` | Selected quota-cycle and forecast summary |
| GET | `/v0/management/cpa-quota-estimator/series` | Selected quota-cycle chart samples |
| GET | `/v0/management/cpa-quota-estimator/monthly` | Calendar-month usage, reset, and capacity summary |
| GET | `/v0/management/cpa-quota-estimator/repair/early-resets` | Preview historical false early-reset candidates without changing data |
| POST | `/v0/management/cpa-quota-estimator/repair/early-resets` | Transactionally merge all currently detected candidates |
| GET | `/v0/management/cpa-quota-estimator/prices` | Synced prices and Fast policy |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | Trigger an immediate models.dev sync |
| GET | `/v0/management/cpa-quota-estimator/pricing-settings` | Read the saved long-context and Fast surcharge switches |
| POST | `/v0/management/cpa-quota-estimator/pricing-settings` | Save both switches and transactionally recalculate retained historical costs |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | Embedded dashboard resource |

Use `?account=<AuthID>` to select a credential, `?cycle_id=<ID>` on `summary` or `series` to select the forecast cycle, and `?month=YYYY-MM` on `monthly` to select a month. On `series`, pass Unix-second `?start_at=<timestamp>&end_at=<timestamp>` values to return chart samples and capacity trajectories across every quota cycle overlapping that range.

Pass `include_spark=1` on `series` to include the latest independent Spark quota cycle, its scoped cumulative Token/cost points, sample quality, and capacity estimate. Pass the same parameter on `monthly` to return a separate `spark_summary`, whose actual usage, quota equivalents, capacity, and cycle details never mix with the primary quota. The dashboard sends this only when its Spark display switch is enabled.

Historical repair requires `?account=<AuthID>`. Always call GET first and inspect the returned cycle IDs. A candidate must be part of a chain containing at least two adjacent suspicious `early_reset` boundaries and keep the same primary reset schedule across each boundary. Each boundary must either be caused by a separately scoped Spark low reading followed within 10 minutes by the primary quota continuing near its preceding peak, or begin with a primary low reading that fails the new confirmation rule and quickly rebounds. Isolated early resets are deliberately left unchanged. POST revalidates every boundary and applies the full candidate set in one transaction; any failure rolls back all merges. Raw `usage_events` and overall Token, cost, and request totals are preserved. Spark rows are detached from the primary cycle ledger, and only their erroneous primary-quota samples and artificial boundaries are removed. Take an online SQLite backup before POST.

## Build

Requires Go 1.22+, GCC, and CGO:

```bash
make test
make build
make package VERSION=0.5.2
```

`make package` produces a marketplace-compatible zip and `checksums.txt` under `dist/`. Tagged releases are built for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by GitHub Actions.

## Privacy

The SQLite database contains credential identifiers, model names, Token counts, estimated costs, failure status, and quota metadata. It does **not** store prompts, request bodies, or response bodies. The default retention period is 365 days.

## Optional CPAMP authentication bridge

The plugin does not require CPAMP. A custom CPAMP deployment can optionally reuse its existing login state through a restricted `postMessage` bridge; see [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md). The dashboard automatically falls back when the bridge is absent.

## Acknowledgements

Thanks to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the underlying proxy capabilities and native plugin system.

Thanks to the [Linux.do community](https://linux.do/) for testing, feedback, and technical discussion.
