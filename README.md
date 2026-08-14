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

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) usage plugin that maps Codex weekly quota changes to estimated Token and USD-equivalent capacity, then visualizes burn rate and exhaustion forecasts.

> OpenAI does not publish a fixed Token or USD value for the Codex weekly quota. The estimates shown here are workload-equivalent capacity estimates, not official plan face values.

## Features

- Listens to CPA's native `usage.handle`; it does not send probe requests or consume additional quota.
- Persists Token counts, model, `service_tier`, estimated cost, and `X-Codex-Primary-*` quota metadata in a private SQLite database.
- Syncs OpenAI model pricing from `https://models.dev/catalog.json` by default.
- Accounts for cached reads/writes, output Tokens, and the context pricing tier above 272K input Tokens.
- Supports configurable Fast pricing:
  - `multiplier`: multiply normal/long-context pricing, default **2.5×**;
  - `source`: use explicit `experimental.modes.fast.cost` pricing from models.dev.
- Estimates total and remaining weekly Token/USD-equivalent capacity with quartile ranges and confidence levels.
- Shows actual quota usage, sustainable pace, cumulative-average projection, recent 24-hour projection, predicted exhaustion, reset time, and countdown.
- Keeps each observed weekly quota window separately and lets you switch back to prior weeks to review their usage, capacity estimates, and end-of-window forecasts.
- Automatically follows the official CPA or CPAMP panel language, supports Chinese/English manual switching, and remembers the selected mode in the browser.
- Includes a responsive embedded dashboard with dark/light themes and mobile layouts.
- Retains data for 365 days by default and never stores request or response bodies.
- Runs independently of CPA Manager Plus (CPAMP).

## Estimation

For adjacent quota-growth samples in the same weekly window:

```text
Token-equivalent weekly capacity = ΔToken × 100 / Δquota_percent
USD-equivalent weekly capacity   = Δcost × 100 / Δquota_percent
```

The dashboard uses the median of valid intervals and reports P25–P75 as the uncertainty range. Because quota response headers are usually integer percentages, early estimates can vary significantly and become more stable as percentage coverage increases.

The burn forecast compares elapsed weekly time with consumed quota:

```text
time progress          = elapsed seconds / weekly window seconds
cumulative daily pace = used percent / elapsed days
projected reset usage = used percent / time progress
estimated exhaustion  = window start + elapsed time × 100 / used percent
```

The green line is the pace that reaches exactly 100% at reset. Purple is the cumulative-average projection. Orange is the recent approximately 24-hour projection, falling back to the cumulative average when recent evidence is insufficient.

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
| GET | `/v0/management/cpa-quota-estimator/summary` | Current window and forecast summary |
| GET | `/v0/management/cpa-quota-estimator/series` | Current window chart samples |
| GET | `/v0/management/cpa-quota-estimator/prices` | Synced prices and Fast policy |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | Trigger an immediate models.dev sync |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | Embedded dashboard resource |

Use `?account=<AuthID>` on `summary` and `series` to select a credential.

## Build

Requires Go 1.22+, GCC, and CGO:

```bash
make test
make build
make package VERSION=0.3.1
```

`make package` produces a marketplace-compatible zip and `checksums.txt` under `dist/`. Tagged releases are built for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 by GitHub Actions.

## Privacy

The SQLite database contains credential identifiers, model names, Token counts, estimated costs, failure status, and quota metadata. It does **not** store prompts, request bodies, or response bodies. The default retention period is 365 days.

## Optional CPAMP authentication bridge

The plugin does not require CPAMP. A custom CPAMP deployment can optionally reuse its existing login state through a restricted `postMessage` bridge; see [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md). The dashboard automatically falls back when the bridge is absent.

## Acknowledgements

Thanks to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the underlying proxy capabilities and native plugin system.

Thanks to the [Linux.do community](https://linux.do/) for testing, feedback, and technical discussion.

## License

[MIT](LICENSE)
