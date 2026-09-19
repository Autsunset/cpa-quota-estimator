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

一个面向 Codex 的原生 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）额度观测与容量预测插件。它被动记录真实流经 CPA 的请求，在权限允许时把已配置的 OAuth 凭证与已有采样账号合并到同一张运营概览中，独立识别 5 小时额度、周额度与 Spark 额度，并把额度百分比变化换算为 Token 容量与所选计价口径下的等价值。

仪表盘重点回答原始额度百分比无法直接回答的问题：**哪个账号最接近耗尽？大约什么时候用完？按当前请求结构，剩余额度还能完成多少工作？之前各次重置和自然月分别发生了什么？**

> 插件不会发送探测请求或模型请求，也不会额外消耗额度。OpenAI 没有公布 Codex 各额度窗口对应的固定 Token 容量；页面中的 Token、USD 与 Credits 均为根据真实请求推算的负载等效估计，并非官方套餐标称值。

<p align="center">
  <img src="https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/docs/images/dashboard-zh.png" alt="额度容量预测单账号详情仪表盘（简体中文）" width="720">
</p>

## 一眼看懂

- **全部账号集中查看：** 在权限允许时合并已有采样账号与 CPA 中配置的 Codex OAuth 清单，同时展示等待首次采样、已停用和当前不可用的凭证；表格支持逐列筛选、类型感知排序、列宽持久化和键盘操作。
- **额度口径彼此独立：** 对 Codex 主额度和 `gpt-5.3-codex-spark` 都会自动把检测到的 5 小时 Primary 与周 Secondary 分开计算，同时让全部 Spark 用量始终使用完全独立的额度账本。
- **把百分比换成可用容量：** 估计完整周期和剩余额度对应的 Token 与计价值，支持当前 API 价格、优惠前 API 价格和订阅 Credits，并提供不确定性区间与置信度。
- **直接给出消耗判断：** 将实际用量与可持续基准、累计平均速率和近期速率对比，预测耗尽时间，并判断额度能否坚持到重置。
- **重置后历史仍然保留：** 保存已确认周期、跨周期曲线、自然月汇总、额度消耗当量、重置次数与重置时未使用额度。
- **被动、私有、无额外消耗：** 不额外调用上游，不保存提示词或响应正文，保留的用量元数据写入本地 SQLite 数据库。

## 详细功能

- 提供全部账号额度概览，集中展示当前剩余额度、重置状态、请求数、Token、计价值、完整周期容量、置信度和消耗预测；检测到双额度时，同一行会同时显示 5 小时与周额度，点击已有样本的账号即可进入原有详细预测。
- 监听 CPA 原生 `usage.handle` 事件，不发送探测请求，也不会额外消耗额度。
- 区分账号整体额度观测与仅经 CPA 的用量，并按账号持久化采集范围。默认保留现有估算并明确标注“假设全部用量经过 CPA”；混合入口和范围未知模式会停用容量换算，保留实测用量和额度趋势。
- 将 Token 数、模型、`service_tier`、所选口径计价值和 `X-Codex-Primary-*` 额度元数据持久化到独立的 SQLite 数据库。
- 默认从 `https://models.dev/catalog.json` 同步 OpenAI 模型价格。
- 计算缓存读写、输出 Token，以及输入超过 272K Token 时的长上下文价格层级。
- 仪表盘提供三种可持久化计价口径：**优惠前 API 价格（新用户默认）**、当前 API 价格和订阅 Credits。升级时保留已有用户保存的选择。订阅 Credits 固定采用无促销的 Codex Rate Card（`优惠前价格 × 25`），绝不使用临时 API/购买 Credits 优惠。保存计价方式或加价开关后，会在单个事务中重算全部保留请求、当前与历史额度周期采样、月度汇总和计价等效容量；切回任意口径时都从原始 Token 字段重新计算。
- 提供独立的 **模型额度校准** 常驻 Astra 倍率输入项（默认 **1.8×**，范围 **0.01–100**；设为 **1×** 即不校准），官方/价格源基础价格保持不变（每百万 Token：输入 $10、缓存读取 $1、输出 $50）。仪表盘分别显示原价、模型倍率和折算价格（$18/$1.8/$90），并把 Astra 加入剩余 Token 换算表。倍率在所有计价口径中仅应用一次，与已有可选 Fast/长上下文规则叠加。升级时从原始 Token 在事务中重算 Astra 历史估值及受影响周期采样，不改变其他模型。这是相对 Sol 的暂定负载校准，不是官方涨价。
- 支持两种可配置的 Fast 定价方式：
  - `multiplier`：在普通或长上下文价格上应用倍数，默认 **2.5×**；
  - `source`：使用 models.dev 中明确提供的 `experimental.modes.fast.cost` 价格。
- 估计完整周期与剩余额度的 Token/计价等效容量，并提供四分位数区间和置信度；还会把所选周期剩余计价值分别换算为各模型的未缓存输入、输出和缓存命中 Token 余量。
- 展示实际额度轨迹、可持续基准、累计平均预测、近期速率预测、预计耗尽时间、计划重置时间和倒计时。
- 为每个已确认的额度周期建立独立账本；重置后，旧周期仍可在下拉框中选择和回看。
- 将 `gpt-5.3-codex-spark` 响应头视为独立额度口径和周期。当 Spark 返回“5 小时 Primary + 周 Secondary”组合时，两个 Spark 轴分别计算百分比、重置周期、容量估算和月度汇总，不再把 5 小时窗口当成周限额。Spark 的实际 Token、请求数、计价值和额度消耗当量不会创建、拆分、估算或累加到主额度；仪表盘可通过页面顶部的可选开关在主额度内容下方显示完整 Spark 统计。
- 只有当最新主额度窗口被检测为约 5 小时，并且同时存在有效的 `X-Codex-Secondary-*` 响应头时，才把 Primary 作为独立 5 小时额度、Secondary 作为独立周限额；两者分别计算使用率轨迹、重置周期、Token/计价值等效容量、月度额度当量，并在仪表盘分区展示。未检测到 5 小时主额度的账号（包括只有周限额的 Pro 账号）继续沿用原来的单窗口逻辑，不会启用或计算 Secondary 周限额区域。
- 按响应头实际观察时间排序额度证据；计划周期切换需连续两个成功观测一致。提前重置需三个成功、稳定且跨越至少 60 秒的观测；对于此前未出现的完整新窗口计划，只要读数始终不高于旧峰值的一半，也能确认超过 5% 的稀疏采样。
- 检测“上游额度状态临时切换、随后又恢复原重置计划”的已确认异常。恢复当前额度，同时将异常区间以红色曲线色带和证据卡保留，并从容量与近期速率估算中排除。如果此前已推断出提前重置边界，在原计划到期前，两个一致的成功观测恢复原计划及原峰值附近用量时，会在事务中撤销该边界。
- 兼容已耗尽的 5 小时主额度在使用率下降前先延后 `reset_at` 的行为：立即关闭已到期周期，将沿用的 100% 读数隔离到出现新鲜使用率为止，并在下一条新鲜使用率到达时依据保留的原始观测自动修复已被污染的进行中周期。
- 提供显式的历史伪提前重置预览/修复接口；原始用量记录保持不变，已确认的正常周期不会被合并。
- 增加自然月统计：实际 Token、所选口径请求计价值、请求数、涉及周期数、已确认重置数、提前重置数、累计额度消耗当量、重置时未消耗额度，以及本月开始周期的估计总容量。
- 自动跟随官方 CPA 或 CPAMP 面板语言，支持中英文手动切换，并在浏览器中保存选择。
- 提供响应式嵌入式仪表盘，支持深色、浅色主题和移动端布局。
- 将预测周期选择与曲线范围分离：切换预测周期会更新该周期的全部统计数值，并将曲线范围自动重置到该周期；手动曲线范围可以跨越多个周期，但只改变曲线显示，统计数值仍严格归属于所选预测周期。各周期按真实时间分别分段绘制，x 轴每一天一格，并高亮当前预测周期。
- 默认保留 365 天数据，不存储请求正文或响应正文。
- 可独立于 CPA Manager Plus（CPAMP）运行。

## 模型额度校准

在 **费用计算规则** 中修改常驻的 **模型额度校准 → Astra ×** 输入框（默认 **1.8**）。**当前 API 价格、优惠前 API 价格、Credits** 均可使用；不想加倍率就设为 **1**，不再提供额外开关。点击 **保存并重算** 后，倍率会持久化，并在事务中重算历史估值、周期容量和剩余 Token。切换计价方式或重启 CPA 都会保留该倍率。仪表盘通过 `POST /pricing-settings` 保存 `astra_multiplier` 并传入 `apply_model_calibration: true`；原布尔字段仍兼容旧 API 客户端，之前关闭校准的设置会在新界面中显示为 **1×**。

此设置 **仅影响插件估值**，不会修改价格源原价、原始 Token、额度百分比或 **New API 计费**。价格表仍分别显示原价、实际应用倍率及折算价格。

## 估算方法

对于同一额度周期内相邻的额度增长样本：

```text
完整周期 Token 等效容量 = ΔToken × 100 / Δ额度百分比
完整周期计价值等效容量  = Δ计价值 × 100 / Δ额度百分比
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

### 采集覆盖范围

额度百分比和重置时间反映上游账号的整个额度池；Token、请求数和计价值只包含经过 CPA 采集的流量。仪表盘会分别标明这两种来源，并展示最近额度观测时间。

在 **本账号采集范围** 中选择模式并保存，设置按当前 AuthID 分别生效：

| 模式 | 界面选项 | 容量换算 |
|---|---|---|
| `cpa_only` | 假设全部用量经过 CPA | 在明确标注此假设的前提下启用；未配置账号默认使用此模式，保留现有估算 |
| `mixed` | 混合入口（CPA + 直连等） | 因部分用量未被采集而停用 |
| `unknown` | 覆盖范围未知 | 在明确采集范围前停用 |

设置保存在 SQLite 中，插件和浏览器重启后仍会保留，并应用于该账号当前与历史的主额度、周额度和 Spark 视图。保存只改变估算的展示和解释，不会改写原始请求、计价值、采样或周期边界。切回 `cpa_only` 后，会从保留的数据恢复现有计算结果。

例如，CPA 记录 60 万 Token，账号额度池却消耗了 12 个百分点，即使其中只有 6 个百分点来自 CPA，公式仍会得到 500 万 Token 的估计。插件无法仅凭百分比补回直连入口的 Token。更多样本或模型价格倍率都不能确认采集是否完整，因此界面将 **采样充分程度** 与覆盖假设分开展示：前者只衡量有效增长区间和百分比跨度，不代表所有入口都已采集。

混合和未知模式会保留账号额度观测、CPA 实际用量、重置历史及基于百分比的消耗趋势；停用完整周期容量、剩余 Token/计价值、按模型换算、容量轨迹和月度容量汇总。趋势反映整个额度池已观测区间的历史节奏，会随入口比例和未来使用方式变化；直连活动也只有在后续 CPA 请求带来新响应头时才能被观察到。

### 周期识别与月度统计口径

`reset_at` 表示上游计划的未来重置时间，本身不能证明已经重置。计划周期切换需两个成功观测一致指向旧边界后的新窗口。提前重置会保留旧计划和用量基准，直到同一额度口径下至少三个成功、非递减的候选读数跨越 60 秒。计划不变时，读数仍须不超过 5%；对于重置时间更晚、此前未出现的完整新窗口，超过 5% 的读数只要始终不高于旧峰值的一半，也可参与确认，并且推导的窗口起点不得比最近旧计划观测早超过五分钟的计划容差。这覆盖 `80% → 2% → 9% → 10%` 及首次观测已超过 5% 的场景。确认保留整段连续观测，插件重启后仍可继续。新周期从首次真实候选请求开始，不会生成推导的 0% 采样。失败、乱序、回弹或不一致的观测无法确认候选。恢复此前出现过的计划时，即使重置时间向后移动，也按可能的额度状态回滚处理。

旧额度未用完就到期时，新完整窗口可能从之后的首次请求才开始。超过五分钟、甚至空闲多日的间隔也可识别，但必须已过旧到期时间、套餐与窗口长度兼容，且声明的新窗口起点不晚于观测时间加时钟容差。仍需两个一致的成功观测确认；待确认期间保留旧重置计划及峰值。旧周期在原定到期时间关闭，新周期从声明的激活时间开始，旧到期时间之后的请求归入新账本。旧额度未达到 100% 时同样适用。

推断出的“计划延后型提前重置”可由后续证据纠正。在原计划到期前，连续两个成功观测恢复原重置计划，并回到旧峰值附近或更高的用量，且这段恢复观测未出现实际补充额度后的低读数时，会在同一事务内重新打开前一周期并合回当前周期。单条恢复观测会保留为待确认，当前计划暂时不变。原始请求和额度采样全部保留，累计采样账本重新计算，完整的 `A → B → A` 历史可继续识别为异常。若实际补充额度后恢复原计划时用量仍低，随后正常消耗到旧峰值，则不会合并。观测无法区分实际补充额度与上游修正时，自动推断仍采取保守规则。

额度证据按响应头被观察到的时间排序：流式请求使用“请求时间 + TTFT”，没有 TTFT 时使用总延迟。实际 Token 和自然月请求归属仍按原始请求发生时间计算。采样基线按上游声明的重置计划分别维护，因此服务端确认回滚后，当前百分比可以下降，不会继续卡在临时高值。当重复出现的另一套计划随后恢复为原计划时，API 会报告 `upstream_regime_reverted` 异常；仪表盘用红色保留该区间，在两侧切断估算，并从可信样本继续预测。异常前、开始、峰值和恢复四个精确锚点会从保留的原始请求中重建。整周期图只画一条细的标准尖峰，不再把密集异常采样重复叠画；异常卡中另有可读的局部迷你曲线。容量估计轨迹在异常区间内保持断开，并在恢复边界沿用最后一份可信估计立即续上，即使旧版插件没有采到恢复后的低百分比也不会留下额外空白。

Spark 使用与 Codex 主额度相互独立的模型专属额度口径和重置计划。检测到 5 小时 Spark Primary 时，它会始终保留为 Spark 5 小时轴；对应的周 Secondary 响应头进入独立的 `spark_weekly` 轴。两个轴的百分比、重置周期和容量估算不会混合，但实际 Token 和计价值都来自同一批 Spark 请求。旧的、只有单个周 Primary 窗口的 Spark 历史观测仍保持单窗口行为。Spark 请求不会进入主额度的自然月实际 Token、请求数、计价值、周期账本、曲线、额度消耗当量或容量估算。Spark 任一轴的计划重置时间如果在旧边界到达前发生修正，而使用率仍连续增长，只会更新对应的当前周期，不会制造重叠周期或重复累计请求。仪表盘默认隐藏 Spark 额度；勾选页面顶部的**显示 Spark 额度**后，会在全部主额度内容下方展示检测到的两个 Spark 轴，并为两者分别绘制额度、累计 Token 和累计计价值曲线及完整月度周期表。周限曲线始终覆盖上游声明的完整 7 天周期，并在图表上方明确显示周期起点与预计重置时间。插件不会主动轮询上游额度，仪表盘的**刷新**按钮也只会重新读取已保存的观测。如果 Spark 计划重置已经到达，但之后没有新的 Spark 请求，插件会在计划边界关闭已过期周期，并按上一周期计划推算当前窗口与下次重置时间；当前使用率会保持为**待采样**，直到下一次成功 Spark 请求返回新的响应头；新的计划窗口仍需两个一致的成功观测才能正式确认。

自然月统计以 Asia/Shanghai 为边界：

- 实际 Token 和请求数按请求发生时间归属月份；请求计价值使用当前选择的计价口径，并按同一时间归属；
- 额度消耗当量是归属于该月的额度百分比增量之和，因此跨多个周期时可以超过 `100%` 或 `1.00×`；
- 重置次数和重置时未消耗额度只统计已确认的定时重置、提前重置及迁移得到的历史重置；
- 月度估计总容量汇总“在所选月份开始”的各周期完整容量中位数；它仍是当前请求结构下的等效估计，并非官方配额；
- 如果某个周期跨越月初，且月初之前没有可用于建立基线的样本，额度当量统计会标记为“部分覆盖”。

## 安装

### CPA 插件商店

可直接在 CPA 管理中心的**插件管理**页面安装最新版本；插件市场会跟随本仓库最新的兼容 GitHub Release。

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
      pricing_mode: legacy_api # legacy_api (default) | current_api | credits
      apply_fast_pricing: true
      astra_multiplier: 1.8
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

插件升级会原位迁移 SQLite 表结构，不会主动清空历史用量，也不会批量改写历史周期。新鲜观测可触发两类定向边界修正：“已耗尽 5 小时额度沿用 100%”的修复，以及在旧边界到期前确认恢复原计划和原用量水平后，撤销推断出的提前重置。在仪表盘保存费用开关时，会重算历史计价值及派生容量估计，但不会修改 Token 数或额度周期边界。其他历史伪提前重置链仍需显式调用下述修复 POST；升级不会自动补拆历史漏掉的提前重置。使用 Docker 时，应通过 volume 或 bind mount 持久化 `data_path` 所在目录；默认目录是 `/CLIProxyAPI/data`。如果替换容器时没有挂载该目录，容器内的本地数据库也会随之被替换。

## Token 与计价值计算规则

Token 图表使用输入 Token 与输出 Token 之和。缓存 Token 通常已经包含在输入 Token 中，因此不会重复相加；计价值计算仍会单独应用缓存费率：

```text
缓存读取     = max(CacheReadTokens, CachedTokens)
非缓存输入   = max(InputTokens - 缓存读取 - 缓存写入, 0)

计价值 = 非缓存输入 × 输入费率
       + 缓存读取 × 缓存读取费率
       + 缓存写入 × 缓存写入费率
       + 输出 × 输出费率
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
| GET | `/v0/management/cpa-quota-estimator/coverage-settings?account=<AuthID>` | 读取账号采集模式和容量估算假设 |
| POST | `/v0/management/cpa-quota-estimator/coverage-settings?account=<AuthID>` | 保存 `{"mode":"cpa_only"}`、`{"mode":"mixed"}` 或 `{"mode":"unknown"}`，不改写用量 |
| GET | `/v0/resource/plugins/cpa-quota-estimator/dashboard` | 嵌入式仪表盘资源 |

`overview` 为每个已采样账号返回一条轻量的当前周期记录，包括计划类型、当前剩余额度、请求数、Token、计价值、完整周期 Token/计价值容量估计、置信度和消耗预测。主额度字段保留在账号记录顶层；检测到 5 小时 Primary 与周 Secondary 组合时，同一记录还会返回 `five_hour_quota_detected: true` 和独立的 `weekly_quota` 快照。响应顶层的 `pricing_mode` 与 `value_unit` 用于说明兼容字段 `_cost_usd` 当前实际表示 USD 还是 Credits。

仪表盘还会只读查询 CPA 的 Codex OAuth 清单，把未采样账号合并进同一张表格。账号只按精确的 `id`/`name` 别名匹配，不读取或暴露凭证秘密；已停用、当前不可用与仍在等待首次样本的凭证会分别展示。对于双额度账号，剩余、重置、容量、置信度和预测单元格会同时列出 5 小时与周额度，排序则使用约束更紧或状态更紧急的额度。概览支持折叠；每一列都有独立的浏览器端筛选框，点击列名可按字段类型切换升序/降序。拖动表头列边界可在 `40px` 技术下限至 `2000px` 之间调整列宽，双击拖动手柄可恢复该列默认宽度；键盘用户可聚焦分隔条后使用方向键调整（按住 `Shift` 增大步长），或按 `Home` 重置。数字和时间筛选支持 `>`、`>=`、`<`、`<=`、`=` 比较符，显示偏好和列宽会保存在当前浏览器。点击有样本的账号行即可进入现有详情视图，整个过程不会发起 AI 请求。若父面板未提供受限桥接且浏览器没有可复用的 Management Key，仪表盘仍会正常展示已采样账号，并明确提示 OAuth 清单暂不可读。

使用 `?account=<AuthID>` 可选择指定凭证；在 `summary` 或 `series` 中使用 `?cycle_id=<ID>` 可选择预测周期；在 `monthly` 中使用 `?month=YYYY-MM` 可选择月份。`series` 还可传入 Unix 秒级的 `?start_at=<时间戳>&end_at=<时间戳>`，返回该范围内所有重叠额度周期的曲线采样点和容量估计轨迹。

`summary`、`series`、月度汇总和概览账号记录会返回 `collection_coverage`，包含 `mode`、`configured`、`usage_source: "cpa"`、`quota_source: "account_quota_pool"` 及 `capacity_estimation_enabled`。默认返回 `configured: false` 和 `assumption: "all_usage_through_cpa"`，表示假设而非已验证的覆盖范围。估算还会返回 `coverage_mode`、`assumption` 和 `sample_confidence`。混合/未知模式下，`available` 为 false，`confidence` 为 `"unavailable"`，`unavailable_reason` 分别为 `"partial_usage_collection"` 或 `"usage_coverage_unknown"`。容量数值及区间使用 JSON `null`，容量轨迹数组为空，按模型换算清空；`sample_confidence` 和样本数仍独立保留。月度容量字段采用同样规则；`quota_coverage_complete` 仍只表示月初时间基线是否完整，不代表采集到了其他入口的用量。

`summary` 和 `series` 会返回 `remaining_by_model`；自动检测到的 `weekly_quota` 也包含自己的模型余量列表。计价设置会返回 `pricing_mode` 与 `value_unit`；通过 `POST /pricing-settings` 切换口径时，接口只会在全部保留历史周期完成重算后返回成功。

当某账号的最新有效 Primary 观测为 5 小时窗口，并且同时包含更大的 Secondary 窗口时，`summary`、`series` 和 `monthly` 会返回 `five_hour_quota_detected: true`；`series` 会自动增加独立的 `weekly_quota`，`monthly` 会增加 `weekly_summary`，无需额外查询参数。周限额计算只使用带有已检测 5 小时 Primary 窗口的请求，因此只有周限额的 Pro 账号仍保持原来的主额度单窗口响应结构和统计口径。

`summary` 和 `series` 会通过 `quota_anomalies` 返回已确认的主额度临时状态切换；每项包含 `before_at`、`started_at`、`peak_at`、`ended_at`，对应的 `before_used_percent`、`anomalous_used_percent`、`peak_used_percent`、`restored_used_percent`，全部重置计划及观测数量。`series` 还会按所选曲线范围返回 `range_quota_anomalies`。异常点带有 `anomalous: true`，异常后的首个可信额度与容量轨迹分段带有 `break_before: true`；API 使用方不应跨越这些边界推算容量。

在 `series` 中传入 `include_spark=1` 会以 `spark_quota` 返回最新的独立 Spark Primary 周期。当前 Spark 响应为“5 小时 Primary + 周 Secondary”组合时，还会返回 `spark_five_hour_quota_detected: true` 和独立的 `spark_weekly_quota`。在 `monthly` 中传入同一参数会返回 `spark_summary`，双轴形态下还会返回 `spark_weekly_summary`。两份汇总都只使用 Spark 请求，且不会与主额度混合。仪表盘只会在用户启用 Spark 显示开关时传入该参数。

历史修复必须传入 `?account=<AuthID>`。应先调用 GET 并核对返回的周期 ID。候选必须属于至少两个相邻可疑 `early_reset` 边界组成的链，并且边界两侧的主额度重置计划一致。每个边界还必须满足以下证据之一：由独立口径的 Spark 低位读数触发，随后 10 分钟内主额度继续保持在前一峰值附近；或主额度低位读数不满足新确认规则并迅速回弹。孤立的提前重置会被刻意保留。POST 会重新校验每个边界，并在单个事务内应用全部候选；任何一步失败都会整体回滚。修复会保留原始 `usage_events` 以及总体 Token、费用和请求数；Spark 记录只会脱离主周期账本，同时移除其错误生成的主额度采样点和伪周期边界。调用 POST 前请先在线备份 SQLite 数据库。

## 构建

需要 Go 1.22+、GCC 和 CGO：

```bash
make test
make build
make package VERSION=0.11.1
```

`make package` 会在 `dist/` 下生成兼容插件商店的压缩包和 `checksums.txt`。带版本标签的发布会通过 GitHub Actions 构建 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 版本。

可选的浏览器集成测试需要 Node.js 22+ 和 Chrome 或 Chromium，覆盖中英文、手机布局、混合/未知模式、账号隔离、设置持久化、保存或刷新失败，以及切换账号时的旧响应保护：

```bash
CPA_BROWSER_TESTS=1 go test -run TestCoverageBrowser -v ./...
```

可通过 `CPA_BROWSER_ARTIFACT_DIR` 指定截图保留目录。

## 隐私

SQLite 数据库包含凭证标识符、模型名称、Token 数、所选口径计价值、失败状态和额度元数据。它**不会**存储提示词、请求正文或响应正文。默认数据保留时间为 365 天。

## 可选的 CPAMP 授权桥接

本插件不依赖 CPAMP。自定义 CPAMP 部署可以通过受限的 `postMessage` 桥接复用现有登录状态，详见 [`docs/CPAMP_AUTH_BRIDGE.md`](docs/CPAMP_AUTH_BRIDGE.md)。无法使用桥接时，仪表盘会自动回退。

## 致谢

感谢 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 提供底层代理能力与原生插件机制。

感谢 [Linux.do 社区](https://linux.do/) 在测试、反馈与技术交流方面提供的支持。
