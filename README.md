# CPA Quota Estimator

一个原生 CLIProxyAPI（CPA）插件：持续记录每次请求的 Token、估算费用和 Codex 周额度响应头，用额度百分比的实际增长反推当前负载结构下的**周 Token / 美元等效容量**，并绘制曲线。

> OpenAI 没有公布 Codex 周额度对应的固定 Token 或美元值，额度还可能受模型、推理强度、Fast、缓存等因素影响。因此这里展示的是等效容量和区间估计，不是官方套餐面值。

## 功能

- 监听 CPA 原生 `usage.handle`，不发送探测请求，不额外消耗额度。
- 保存请求 Token、模型、`service_tier`、费用和 `X-Codex-Primary-*` 响应头到 SQLite。
- 价格源与 CPAMP 一致：默认每天同步 `https://models.dev/catalog.json` 的 OpenAI 模型价格。
- 支持缓存读、缓存写、输出 Token 和大于 272K 上下文分层计价。
- Fast 可配置为：
  - `multiplier`：按普通/长上下文价格乘倍率，默认 **2.5×**；
  - `source`：使用 models.dev 的 `experimental.modes.fast.cost` 显式价格。
- 根据相邻额度增长区间取中位数，输出全周容量、剩余容量、四分位区间、置信度和预计耗尽时间。
- 内置中文仪表盘，无第三方前端依赖；自动跟随 CPAMP 深色/浅色主题。
- 嵌入当前定制 CPAMP 时复用面板登录态，不再重复输入密钥；独立打开资源页时仍保留手动密钥登录。
- 针对手机布局优化，图表在窄屏中可横向滑动，避免坐标与单位被压缩。
- 曲线分别显示周额度（%）、Token（Token）和计费用量（USD），横轴固定为 Asia/Shanghai 时间。
- 记录每次额度增长后的滚动中位数预测，绘制整周 Token 容量与整周美元额度的历史变化曲线。

## 估算方法

每次上游返回新的额度百分比时，插件记录：

- 当前已用百分比 `p`
- 当前窗口累计 Token `T`
- 当前窗口累计估算费用 `C`

对同一周窗口内的相邻增长区间计算：

```text
Token 等效周容量 = ΔT × 100 / Δp
美元等效周容量  = ΔC × 100 / Δp
```

最终值采用多个增长区间的中位数，P25–P75 作为波动区间。百分比响应头通常是整数，因此刚开始只有 1 个增长区间时误差会比较大；覆盖百分比和区间数量增加后会稳定。

## 构建

依赖 Go 1.22+、GCC 和 CGO：

```bash
make test
make build
```

产物：`dist/cpa-quota-estimator-v0.1.0.so`。

## 安装

复制插件到对应平台目录：

```bash
cp dist/cpa-quota-estimator-v0.1.0.so /path/to/cpa/plugins/linux/amd64/
```

在 CPA `config.yaml` 中加入：

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
      history_days: 180
  enabled: true
```

重启 CPA 后，日志应包含：

```text
plugin registered plugin_id=cpa-quota-estimator plugin_name=CPA Quota Estimator
```

在当前定制 CPAMP 左侧菜单打开“额度容量预测”时，页面通过受限的父页面消息桥复用已有登录态，不会再次显示密钥输入框，也不会把 Manager Server 管理员密钥传给插件 iframe。独立打开资源页时仍可使用 Manager Server 管理员密钥或 CPA Management Key 登录；密钥只写入当前标签页的 `sessionStorage`。

Token 曲线固定采用输入 Token + 输出 Token。缓存命中 Token 通常已经包含在输入 Token 中，因此不会再次相加；费用估算仍按缓存折扣价格独立计算。

## 管理 API

均由 CPA Management Key 保护；经 Manager Server 访问时也可使用 Manager Server 管理员密钥。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/summary` | 当前窗口和预测摘要 |
| GET | `/v0/management/cpa-quota-estimator/series` | 当前窗口曲线点 |
| GET | `/v0/management/cpa-quota-estimator/prices` | 已同步价格与 Fast 策略 |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | 立即同步 models.dev |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | 仪表盘资源 |

`summary` 和 `series` 可通过 `?account=<AuthID>` 选择凭证。

## 计费规则

单位均为 USD / 1M Token：

```text
缓存读 Token = max(CacheReadTokens, CachedTokens)
未缓存输入   = max(InputTokens - 缓存读 - 缓存写, 0)
费用 = 未缓存输入×input
     + 缓存读×cache_read
     + 缓存写×cache_write
     + 输出×output
```

`ReasoningTokens` 已包含在输出 Token 中，不重复计费。输入大于 `long_context_threshold` 时整次请求采用 context tier；`service_tier=priority` 或 `fast` 时采用 Fast 策略。

当前实例的 `fast_pricing_mode=multiplier`、`fast_multiplier=2.5`，所以实际响应记录为 `priority` 的 `/fast` 请求按 **2.5 倍**估算；普通 `auto/default` 请求仍按 1 倍估算。

## 升级与恢复

数据库在 CPA 数据卷中，升级 `.so` 不会删除历史曲线：

1. 先查看新版本官方说明，确认是否已经原生提供同等额度容量预测；如果已提供，可停用本插件，避免重复采集。
2. 只保留一个最新配置备份：
   ```bash
   rm -f config.yaml.bak config.yaml.bak-* config.yaml.backup*
   cp -a config.yaml config.yaml.bak-latest
   ```
3. 替换 `.so`，保持文件名 ID 前缀为 `cpa-quota-estimator-v<版本>.so`。
4. 重启 CPA 并检查注册日志、`summary` API 和仪表盘。
5. 如需回滚，恢复 `config.yaml.bak-latest` 和旧 `.so`；不要删除 SQLite。

当前 CPAMP 无重复密钥登录依赖一处定制桥接；升级 CPAMP 前请按 [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md) 检查官方实现并决定是否重放补丁。

## 数据与隐私

插件不保存请求正文或响应正文。数据库只包含凭证标识、模型、Token 计数、费用、失败状态和额度元数据。默认保留 180 天。

## 当前实例迁移记录

2026-08-14 已对当前服务器执行一次性 CPAMP 历史迁移：导入当前周窗口内、插件安装前后的 3260 条 CPAMP 请求并重建 115 个采样点。导入后由插件自行持续记录，**不要再次执行历史导入**。

导入时统一使用 Codex `primary` 周额度，排除了 Spark/Bengalfox 专属额度对主曲线的干扰；周窗口重置时间按分钟归一化，以容忍并发响应中 1–2 秒的偏差。
