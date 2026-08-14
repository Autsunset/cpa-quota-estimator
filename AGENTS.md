# 项目维护说明

本文件适用于整个仓库。任何修改都应同时考虑源码发布、GitHub Release 和 CPA 插件市场状态，不能只更新其中一处。

## 基本要求

- `README.md` 是默认展示的英文文档，`README.zh-CN.md` 是中文文档；修改项目说明时必须保持两版内容和语言切换链接同步。
- 用户可见功能、仪表盘、配置或发布包发生变化时，按语义化版本规则更新 `Makefile`、`types.go` 和两份 README 中的版本示例。
- 发布前至少运行 `go test ./...`、构建目标平台插件，并检查 Markdown、JavaScript 和 Git diff。
- 只有 GitHub Release 已成功生成全部平台压缩包和 `checksums.txt` 后，才能在插件市场中引用该版本。

## GitHub 发布流程

1. 将相关修改按逻辑拆分为 Conventional Commits。
2. 推送默认分支后创建并推送 `v<major>.<minor>.<patch>` 标签。
3. 等待 Release workflow 成功完成。
4. 确认 Release 至少包含 Linux amd64/arm64、macOS amd64/arm64、Windows amd64 和 `checksums.txt`。
5. 校验发布包名称、版本号和校验文件一致。

## CPA 插件市场同步

每次发布新版本都必须检查 [CLIProxyAPI Plugins Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) 中本插件的状态，并根据实际状态处理：

- 首次上架 PR 仍为 **Open**：更新同一个 PR 的来源分支、`registry.json`、版本号、描述和 PR 正文，使其指向最新的已发布版本；不要另开重复 PR。
- 首次上架 PR 已 **Merged**：不要再修改或尝试复用旧 PR。检查官方注册表是否需要显式更新版本；如果需要，按商店流程创建新的版本更新 PR，如果商店能自动跟随 Release，则记录检查结果，不创建无意义 PR。
- PR 为 **Closed 且未合并**：检查关闭原因并修正；不能直接假设旧 PR 仍有效，必要时创建新的合规 PR。

当前首次上架 PR：<https://github.com/router-for-me/CLIProxyAPI-Plugins-Store/pull/78>

更新商店 PR 前必须重新查询其状态，不能依赖本文件记录的历史状态。
