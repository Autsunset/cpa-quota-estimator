<p align="center">
  <img src="./logo.png" alt="CPA Quota Estimator icon" width="160" height="160">
</p>

# CPA Quota Estimator

<p align="center">
  <a href="./README.md"><img src="https://img.shields.io/badge/Language-English-0969da?style=for-the-badge" alt="English"></a>
  <a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/语言-简体中文-d0d7de?style=for-the-badge" alt="简体中文"></a>
</p>

[![CI](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml/badge.svg)](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Autsunset/cpa-quota-estimator)](https://github.com/Autsunset/cpa-quota-estimator/releases)
[![License](https://img.shields.io/github/license/Autsunset/cpa-quota-estimator)](LICENSE)

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) usage plugin that records actual Codex quota cycles, maps quota changes to estimated Token and USD-equivalent capacity, and summarizes usage across scheduled or early official resets.

> OpenAI does not publish a fixed Token or USD value for the Codex weekly quota. The estimates shown here are workload-equivalent capacity estimates, not official plan face values.

## Features

- Listens to CPA's native `usage.handle`; it does not send probe requests or consume additional quota.
- Persists Token counts, model, `service_tier`, estimated cost, and `X-Codex-Primary-*` quota metadata in a private SQLite database.
- Syncs OpenAI model pricing from `https://models.dev/catalog.json` by default.
- Accounts for cached reads/writes, output Tokens, and the context pricing tier above 272K input Tokens.
- Supports configurable Fast pricing:
  - `multiplier`: multiply normal/long-context pricing, default **2.5×**;
  - `source`: use explicit `experimental.modes.fast.cost` pricing from models.dev.
- Estimates full-cycle and remaining-quota Token/USD-equivalent capacity with interquartile ranges and confidence levels.
- Shows actual quota usage, a sustainable baseline, cumulative-average projection, recent-rate projection, predicted exhaustion, planned reset time, and countdown.
- Maintains an independent ledger for every confirmed quota cycle. Historical cycles remain available in the selector after a reset.
- Detects normal resets from schedule transitions and confirms same-`reset_at` early resets only after two consecutive low-usage observations, reducing false splits from a single stale header.
- Adds calendar-month reporting for actual Tokens, estimated request cost, requests, involved cycles, confirmed resets, early resets, cumulative quota-consumption equivalents, unconsumed quota at reset, and estimated capacity allocated by cycles starting in that month.
- Automatically follows the official CPA or CPAMP panel language, supports Chinese/English manual switching, and remembers the selected mode in the browser.
- Includes a responsive embedded dashboard with dark/light themes and mobile layouts.
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

`reset_at` is treated as the upstream planned reset time, not by itself as proof that a reset has already occurred. A normal schedule transition closes the preceding cycle. If the planned time remains unchanged but used quota abruptly returns near 0%, the plugin waits for a second consistent observation before confirming an early reset; it then assigns the first low observation and subsequent requests to the new cycle.

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
      long_context_threshold: 272000
      history_days: 365
  enabled: true
```

Restart CPA. The log should include:

```text
plugin registered plugin_id=cpa-quota-estimator plugin_name=CPA Quota Estimator
```

Open **额度容量预测 / Quota Estimator** from the management center. When the host panel cannot provide an authenticated plugin bridge, the dashboard falls back to CPA Management Key login and stores the key only in the current tab's `sessionStorage`.

Plugin upgrades migrate the SQLite schema in place and do not intentionally clear usage history. In Docker, persist the directory containing `data_path`—the default is `/CLIProxyAPI/data`—with a volume or bind mount; replacing a container without that mount also replaces its local database.

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

All prices are USD per one million Tokens. `ReasoningTokens` are already included in output Tokens and are not charged twice. Requests whose recorded `service_tier` is `priority` or `fast` use the configured Fast pricing policy; `auto` and `default` remain at 1×.

## Management API

All management routes are protected by CPA Management Key:

| Method | Path | Purpose |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/summary` | Selected quota-cycle and forecast summary |
| GET | `/v0/management/cpa-quota-estimator/series` | Selected quota-cycle chart samples |
| GET | `/v0/management/cpa-quota-estimator/monthly` | Calendar-month usage, reset, and capacity summary |
| GET | `/v0/management/cpa-quota-estimator/prices` | Synced prices and Fast policy |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | Trigger an immediate models.dev sync |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | Embedded dashboard resource |

Use `?account=<AuthID>` to select a credential, `?cycle_id=<ID>` on `summary` or `series` to select a historical cycle, and `?month=YYYY-MM` on `monthly` to select a month.

## Build

Requires Go 1.22+, GCC, and CGO:

```bash
make test
make build
make package VERSION=0.4.0
```

`make package` produces a marketplace-compatible zip and `checksums.txt` under `dist/`. Tagged releases are built for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by GitHub Actions.

## Privacy

The SQLite database contains credential identifiers, model names, Token counts, estimated costs, failure status, and quota metadata. It does **not** store prompts, request bodies, or response bodies. The default retention period is 365 days.

## Optional CPAMP authentication bridge

The plugin does not require CPAMP. A custom CPAMP deployment can optionally reuse its existing login state through a restricted `postMessage` bridge; see [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md). The dashboard automatically falls back when the bridge is absent.

## Acknowledgements

Thanks to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the underlying proxy capabilities and native plugin system.

Thanks to the [Linux.do community](https://linux.do/) for testing, feedback, and technical discussion.
