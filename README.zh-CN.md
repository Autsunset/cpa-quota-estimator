<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/logo.png" alt="CPA Quota Estimator 图标" width="160" height="160">
</p>

# CPA Quota Estimator

<p align="center">
  <a href="https://github.com/Autsunset/cpa-quota-estimator/blob/main/README.md"><kbd>English</kbd></a>
  <a href="https://github.com/Autsunset/cpa-quota-estimator/blob/main/README.zh-CN.md"><kbd>简体中文</kbd></a>
</p>

[![CI](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml/badge.svg)](https://github.com/Autsunset/cpa-quota-estimator/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Autsunset/cpa-quota-estimator)](https://github.com/Autsunset/cpa-quota-estimator/releases)
[![License](https://img.shields.io/github/license/Autsunset/cpa-quota-estimator)](LICENSE)

一个原生 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）用量插件：记录实际发生的 Codex 额度周期，根据额度百分比增长估计 Token 与美元等效容量，并汇总定时重置及官方提前重置所形成的月度用量。

> OpenAI 没有公布 Codex 周额度对应的固定 Token 数或美元金额。本项目展示的是当前请求结构下的等效容量估计，并非官方标称额度。

<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/docs/images/dashboard-zh.png" alt="额度容量预测仪表盘（简体中文）" width="720">
</p>

## 功能特性

- 监听 CPA 原生 `usage.handle` 事件，不发送探测请求，也不会额外消耗额度。
- 将 Token 数、模型、`service_tier`、估算费用和 `X-Codex-Primary-*` 额度元数据持久化到独立的 SQLite 数据库。
- 默认从 `https://models.dev/catalog.json` 同步 OpenAI 模型价格。
- 计算缓存读写、输出 Token，以及输入超过 272K Token 时的长上下文价格层级。
- 仪表盘提供三种可持久化计价口径：当前 API 价格、优惠前 API 价格和订阅 Credits。订阅 Credits 固定采用无促销的 Codex Rate Card（`优惠前价格 × 25`），绝不使用临时 API/购买 Credits 优惠。保存计价方式或加价开关后，会在单个事务中重算全部保留请求、当前与历史额度周期采样、月度汇总和计价等效容量；切回任意口径时都从原始 Token 字段重新计算。
- 支持两种可配置的 Fast 定价方式：
  - `multiplier`：在普通或长上下文价格上应用倍数，默认 **2.5×**；
  - `source`：使用 models.dev 中明确提供的 `experimental.modes.fast.cost` 价格。
- 估计完整周期与剩余额度的 Token/计价等效容量，并提供四分位数区间和置信度；还会把所选周期剩余计价值分别换算为各模型的未缓存输入、输出和缓存命中 Token 余量。
- 展示实际额度轨迹、可持续基准、累计平均预测、近期速率预测、预计耗尽时间、计划重置时间和倒计时。
- 为每个已确认的额度周期建立独立账本；重置后，旧周期仍可在下拉框中选择和回看。
- 将 `gpt-5.3-codex-spark` 响应头视为独立额度口径和周期。Spark 的实际 Token、请求数、费用、额度消耗当量、周期容量及月度汇总均单独统计，不会创建、拆分、估算或累加到主额度；仪表盘可通过页面顶部的可选开关在主额度内容下方显示完整 Spark 统计。
- 只有当最新主额度窗口被检测为约 5 小时，并且同时存在有效的 `X-Codex-Secondary-*` 响应头时，才把 Primary 作为独立 5 小时额度、Secondary 作为独立周限额；两者分别计算使用率轨迹、重置周期、Token/USD 等效容量、月度额度当量，并在仪表盘分区展示。未检测到 5 小时主额度的账号（包括只有周限额的 Pro 账号）继续沿用原来的单窗口逻辑，不会启用或计算 Secondary 周限额区域。
- 按响应头实际观察时间排序额度证据；计划周期切换需连续两个成功观测一致，当 `reset_at` 不变时则需三个成功、稳定且跨越至少 60 秒的低用量观测，才确认提前重置。
- 兼容已耗尽的 5 小时主额度在使用率下降前先延后 `reset_at` 的行为：立即关闭已到期周期，将沿用的 100% 读数隔离到出现新鲜使用率为止，并在下一条新鲜使用率到达时依据保留的原始观测自动修复已被污染的进行中周期。
- 提供显式的历史伪提前重置预览/修复接口；原始用量记录保持不变，已确认的正常周期不会被合并。
- 增加自然月统计：实际 Token、所选口径请求计价值、请求数、涉及周期数、已确认重置数、提前重置数、累计额度消耗当量、重置时未消耗额度，以及本月开始周期的估计总容量。
- 自动跟随官方 CPA 或 CPAMP 面板语言，支持中英文手动切换，并在浏览器中保存选择。
- 提供响应式嵌入式仪表盘，支持深色、浅色主题和移动端布局。
- 将预测周期选择与曲线范围分离：切换预测周期会更新该周期的全部统计数值，并将曲线范围自动重置到该周期；手动曲线范围可以跨越多个周期，但只改变曲线显示，统计数值仍严格归属于所选预测周期。各周期按真实时间分别分段绘制，x 轴每一天一格，并高亮当前预测周期。
- 默认保留 365 天数据，不存储请求正文或响应正文。
- 可独立于 CPA Manager Plus（CPAMP）运行。

## 估算方法

对于同一额度周期内相邻的额度增长样本：

```text
完整周期 Token 等效容量 = ΔToken × 100 / Δ额度百分比
完整周期美元等效容量    = Δ费用 × 100 / Δ额度百分比
```

仪表盘取所有有效增长区间估计值的中位数，并用 P25–P75（Q1–Q3）表示不确定性区间。由于额度响应头通常只提供整数百分比，早期估计可能波动较大；随着已覆盖额度百分比增加，结果通常会逐渐稳定。

额度消耗预测会比较当前周期的时间进度和已用额度：

```text
时间进度       = 已经过秒数 / 周期总秒数
累计日均速率   = 已用额度百分比 / 已经过天数
重置时预计用量 = 已用额度百分比 / 时间进度
预计耗尽时间   = 周期开始时间 + 已经过时间 × 100 / 已用额度百分比
```

绿色线表示在重置时恰好达到 100% 的可持续基准速率；紫色线表示累计平均预测；橙色线表示近期约 24 小时的速率预测，近期样本不足时回退到累计平均预测。如果官方提前补充额度但 `reset_at` 没有改变，新周期会从首次确认的低用量观测开始，并以缩短后的剩余区间进行预测。

### 周期识别与月度统计口径

`reset_at` 表示上游计划的未来重置时间，本身不等同于“已经发生重置”。计划周期切换只有在两个成功观测都一致指向旧周期边界后的新窗口时才会确认。如果计划时间不变，但额度使用率突然回到接近 0%，插件要求同一额度口径下出现三个成功、非递减的低用量读数，且首尾至少跨越 60 秒。失败、陈旧、乱序或很快回弹的读数都不会创建新周期。

额度证据按响应头被观察到的时间排序：流式请求使用“请求时间 + TTFT”，没有 TTFT 时使用总延迟。实际 Token 和自然月请求归属仍按原始请求发生时间计算。

Spark 使用与 Codex 主额度相互独立的模型专属额度口径和重置计划。Spark 请求不会进入主额度的自然月实际 Token、请求数、估算费用、周期账本、曲线、额度消耗当量或容量估算。Spark 的计划重置时间如果在旧边界到达前发生修正，而使用率仍连续增长，只会更新当前 Spark 周期，不会制造重叠周期或重复累计请求。仪表盘默认隐藏 Spark 额度；勾选页面顶部的**显示 Spark 额度**后，会在全部主额度内容下方展示按 Spark 自身时间划分的最新周期曲线、完整周期与剩余额度 Token/USD 等效容量、采样质量、月度汇总及周期明细。所有 Spark 数值只使用 Spark 响应头与 Spark 请求计算。插件不会主动轮询上游额度，仪表盘的**刷新**按钮也只会重新读取已保存的观测。如果 Spark 计划重置已经到达，但之后没有新的 Spark 请求，插件会在计划边界关闭已过期周期，并按上一周期计划推算当前窗口与下次重置时间；当前使用率会保持为**待采样**，直到下一次成功 Spark 请求返回新的响应头；新的计划窗口仍需两个一致的成功观测才能正式确认。

自然月统计以 Asia/Shanghai 为边界：

- 实际 Token 和请求数按请求发生时间归属月份；请求费用依据 models.dev 价格估算，并按同一时间归属；
- 额度消耗当量是归属于该月的额度百分比增量之和，因此跨多个周期时可以超过 `100%` 或 `1.00×`；
- 重置次数和重置时未消耗额度只统计已确认的定时重置、提前重置及迁移得到的历史重置；
- 月度估计总容量汇总“在所选月份开始”的各周期完整容量中位数；它仍是当前请求结构下的等效估计，并非官方配额；
- 如果某个周期跨越月初，且月初之前没有可用于建立基线的样本，额度当量统计会标记为“部分覆盖”。

## 安装

### CPA 插件商店

插件被官方注册表收录后，可直接在 CPA 管理中心的**插件管理**页面安装。

### 手动安装

从 [GitHub Releases](https://github.com/Autsunset/cpa-quota-estimator/releases) 下载与运行平台匹配的压缩包，将其中的动态库解压到对应的 CPA 插件目录，例如：

```text
plugins/linux/amd64/cpa-quota-estimator.so
plugins/linux/arm64/cpa-quota-estimator.so
plugins/darwin/arm64/cpa-quota-estimator.dylib
plugins/windows/amd64/cpa-quota-estimator.dll
```

可在 `config.yaml` 中加入以下配置：

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
      long_context_threshold: 272000
      apply_long_context_pricing: false
      history_days: 365
  enabled: true
```

重启 CPA 后，日志中应出现：

```text
plugin registered plugin_id=cpa-quota-estimator plugin_name=CPA Quota Estimator
```

> **升级安全提醒：** 如果当前 AI 客户端通过 New API 和 CPA 访问上游，升级插件时不要停止 CPA 或 New API，也不要执行 `docker compose down`。应在服务保持运行时完成下载、校验、在线数据库备份和插件文件原子替换，最后只执行一次 CPA 重启并立即检查健康状态与插件注册日志。直接停止任一服务可能切断当前 AI 会话，使维护过程无法继续。

在 CPAMP 中打开**额度容量预测 / Quota Estimator**。仪表盘会优先复用已认证的插件桥接；同源部署且桥接不可用时，也会自动复用 CPAMP 通过**记住密码**选项持久保存的 Management Key。跨域部署或 CPAMP 未持久保存登录态时，才回退到 CPA Management Key 登录。此时勾选**在此浏览器记住密钥**后，兜底密钥会保存在当前浏览器的 `localStorage` 中；不勾选则仍只保存在当前标签页的 `sessionStorage` 中。共享设备请勿开启持久保存。

> **冷启动说明：** 安装插件后并不会立即看到额度。插件完全被动，只记录真实流经 CPA 的请求，也无法回补安装之前的历史用量。全部账号概览会尝试从受保护的 CPA `auth-files` 管理接口合并已配置的 Codex OAuth；尚未流经 CPA 的账号会显示为“等待首次经 CPA 调用”，而不是被误判为凭证丢失。首次真实的 Codex 请求之后，才会出现当前剩余额度和重置时间；只有当记录样本之间的已使用额度百分比确实增长（Δ已使用百分比 > 0）时，才开始计算容量估算。由于额度响应头只提供整数百分比，首个可用估算可能需要多次请求才会出现，并随着消耗累积逐渐稳定。

插件升级会原位迁移 SQLite 表结构，不会主动清空历史用量，通常也不会自动改写历史周期；唯一的自动边界修复是“已耗尽 5 小时额度沿用 100%”的定向修复，并且只有下一条新鲜 Primary 使用率证明进行中周期跨越了已到期重置时才会执行。在仪表盘保存费用开关时，会有意重算历史计价值及派生容量估计，但不会修改 Token 数或其他额度周期边界。历史伪提前重置只有在显式调用下述修复 POST 后才会变更。使用 Docker 时，应通过 volume 或 bind mount 持久化 `data_path` 所在目录；默认目录是 `/CLIProxyAPI/data`。如果替换容器时没有挂载该目录，容器内的本地数据库也会随之被替换。

## Token 与费用计算规则

Token 图表使用输入 Token 与输出 Token 之和。缓存 Token 通常已经包含在输入 Token 中，因此不会重复相加；费用计算仍会单独应用缓存价格：

```text
缓存读取     = max(CacheReadTokens, CachedTokens)
非缓存输入   = max(InputTokens - 缓存读取 - 缓存写入, 0)

费用 = 非缓存输入 × 输入价格
     + 缓存读取 × 缓存读取价格
     + 缓存写入 × 缓存写入价格
     + 输出 × 输出价格
```

当前计价值按每一百万 Token 计算，单位可以是 USD，也可以是订阅 Credits。`ReasoningTokens` 已包含在输出 Token 中，不会重复计量。记录的 `service_tier` 为 `priority` 或 `fast` 时使用配置的 Fast 策略；`auto` 和 `default` 保持 1×。当 `InputTokens > long_context_threshold` 时使用长上下文档位，默认阈值为 272,000。

仪表盘计价方式包括：

- `current_api`：models.dev/API 当前价格，包含现行优惠；
- `legacy_api`：优惠前 API 等效价；GPT-5.6 Sol/Terra/Luna 的输入/缓存命中/输出分别使用 `$5/$0.50/$30`、`$2.50/$0.25/$15`、`$1/$0.10/$6`；
- `credits`：订阅套餐内、无促销的 Codex Credits，严格按优惠前价格 × 25 计算；Sol/Terra/Luna 每百万输入/缓存命中/输出分别为 `125/12.5/750`、`62.5/6.25/375`、`25/2.5/150` Credits，明确排除购买 Credits 的临时优惠；缓存写入按订阅 Rate Card 记为 0 Credits。

仪表盘同时保留 **>272K 长上下文加价** 和 **Fast 加价** 开关。点击**保存并重算**后，三项设置会保存到 SQLite，并在单个事务中重建全部保留的 `usage_events.cost_usd` 兼容值和所有额度采样累计值。当前周期、任意历史周期、跨周期曲线、5 小时与周限额区域、月度汇总都会统一使用新口径；再次切回时从原始输入/输出/缓存 Token 重算，不会在上一次结果上继续换算。JSON 中带 `_cost_usd` 的字段为兼容旧客户端而保留，实际单位由 `pricing_mode` 和 `value_unit` 指明。

对于所选主额度周期，以及检测到的独立周限额周期，仪表盘会列出各 Codex 模型的剩余未缓存输入、输出和缓存命中 Token。每一列都是独立假设：剩余计价值全部用于该模型及该 Token 类型，并采用 Standard、基础上下文单价。

## Management API

所有管理接口均受 CPA Management Key 保护：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/v0/management/cpa-quota-estimator/overview` | 所有已记录账号的当前主额度及已检测周额度概览 |
| GET | `/v0/management/cpa-quota-estimator/summary` | 所选额度周期与预测摘要 |
| GET | `/v0/management/cpa-quota-estimator/series` | 所选额度周期图表采样数据 |
| GET | `/v0/management/cpa-quota-estimator/monthly` | 自然月用量、重置与容量汇总 |
| GET | `/v0/management/cpa-quota-estimator/repair/early-resets` | 只读预览历史伪提前重置候选 |
| POST | `/v0/management/cpa-quota-estimator/repair/early-resets` | 在单个事务中合并当前全部候选 |
| GET | `/v0/management/cpa-quota-estimator/prices` | 已同步价格与 Fast 策略 |
| POST | `/v0/management/cpa-quota-estimator/prices/sync` | 立即触发 models.dev 价格同步 |
| GET | `/v0/management/cpa-quota-estimator/pricing-settings` | 读取已保存的计价口径、长上下文与 Fast 开关 |
| POST | `/v0/management/cpa-quota-estimator/pricing-settings` | 保存计价口径与开关，并在事务中重算全部保留历史周期计价值 |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | 嵌入式仪表盘资源 |

`overview` 为每个已采样账号返回一条轻量的当前周期记录，包括计划类型、当前剩余额度、请求数、Token、计价值、完整周期 Token/计价值容量估计、置信度和消耗预测。主额度字段保留在账号记录顶层；检测到 5 小时 Primary 与周 Secondary 组合时，同一记录还会返回 `five_hour_quota_detected: true` 和独立的 `weekly_quota` 快照。响应顶层的 `pricing_mode` 与 `value_unit` 用于说明兼容字段 `_cost_usd` 当前实际表示 USD 还是 Credits。

仪表盘还会只读查询 CPA 的 Codex OAuth 清单，把未采样账号合并进同一张表格。账号只按精确的 `id`/`name` 别名匹配，不读取或暴露凭证秘密；已停用、当前不可用与仍在等待首次样本的凭证会分别展示。对于双额度账号，剩余、重置、容量、置信度和预测单元格会同时列出 5 小时与周额度，排序则使用约束更紧或状态更紧急的额度。概览支持折叠；每一列都有独立的浏览器端筛选框，点击列名可按字段类型切换升序/降序。拖动表头列边界可在 `40px` 技术下限至 `2000px` 之间调整列宽，双击拖动手柄可恢复该列默认宽度；键盘用户可聚焦分隔条后使用方向键调整（按住 `Shift` 增大步长），或按 `Home` 重置。数字和时间筛选支持 `>`、`>=`、`<`、`<=`、`=` 比较符，显示偏好和列宽会保存在当前浏览器。点击有样本的账号行即可进入现有详情视图，整个过程不会发起 AI 请求。若父面板未提供受限桥接且浏览器没有可复用的 Management Key，仪表盘仍会正常展示已采样账号，并明确提示 OAuth 清单暂不可读。

使用 `?account=<AuthID>` 可选择指定凭证；在 `summary` 或 `series` 中使用 `?cycle_id=<ID>` 可选择预测周期；在 `monthly` 中使用 `?month=YYYY-MM` 可选择月份。`series` 还可传入 Unix 秒级的 `?start_at=<时间戳>&end_at=<时间戳>`，返回该范围内所有重叠额度周期的曲线采样点和容量估计轨迹。

`summary` 和 `series` 会返回 `remaining_by_model`；自动检测到的 `weekly_quota` 也包含自己的模型余量列表。计价设置会返回 `pricing_mode` 与 `value_unit`；通过 `POST /pricing-settings` 切换口径时，接口只会在全部保留历史周期完成重算后返回成功。

当某账号的最新有效 Primary 观测为 5 小时窗口，并且同时包含更大的 Secondary 窗口时，`summary`、`series` 和 `monthly` 会返回 `five_hour_quota_detected: true`；`series` 会自动增加独立的 `weekly_quota`，`monthly` 会增加 `weekly_summary`，无需额外查询参数。周限额计算只使用带有已检测 5 小时 Primary 窗口的请求，因此只有周限额的 Pro 账号仍保持原来的主额度单窗口响应结构和统计口径。

在 `series` 中传入 `include_spark=1` 可同时返回最新的独立 Spark 额度周期、仅属于 Spark 的累计 Token/费用采样点、采样质量及容量估算。在 `monthly` 中传入同一参数会返回独立的 `spark_summary`，其中实际消耗、额度当量、容量和周期明细均不与主额度混合。仪表盘只会在用户启用 Spark 显示开关时传入该参数。

历史修复必须传入 `?account=<AuthID>`。应先调用 GET 并核对返回的周期 ID。候选必须属于至少两个相邻可疑 `early_reset` 边界组成的链，并且边界两侧的主额度重置计划一致。每个边界还必须满足以下证据之一：由独立口径的 Spark 低位读数触发，随后 10 分钟内主额度继续保持在前一峰值附近；或主额度低位读数不满足新确认规则并迅速回弹。孤立的提前重置会被刻意保留。POST 会重新校验每个边界，并在单个事务内应用全部候选；任何一步失败都会整体回滚。修复会保留原始 `usage_events` 以及总体 Token、费用和请求数；Spark 记录只会脱离主周期账本，同时移除其错误生成的主额度采样点和伪周期边界。调用 POST 前请先在线备份 SQLite 数据库。

## 构建

需要 Go 1.22+、GCC 和 CGO：

```bash
make test
make build
make package VERSION=0.7.0
```

`make package` 会在 `dist/` 下生成兼容插件商店的压缩包和 `checksums.txt`。带版本标签的发布会通过 GitHub Actions 构建 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 版本。

## 隐私

SQLite 数据库包含凭证标识符、模型名称、Token 数、估算费用、失败状态和额度元数据。它**不会**存储提示词、请求正文或响应正文。默认数据保留时间为 365 天。

## 可选的 CPAMP 授权桥接

本插件不依赖 CPAMP。自定义 CPAMP 部署可以通过受限的 `postMessage` 桥接复用现有登录状态，详见 [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md)。无法使用桥接时，仪表盘会自动回退。

## 致谢

感谢 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 提供底层代理能力与原生插件机制。

感谢 [Linux.do 社区](https://linux.do/) 在测试、反馈与技术交流方面提供的支持。
