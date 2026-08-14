# 项目维护说明

本文件适用于整个仓库。任何发布都应同时检查源码、GitHub Release 和 CPA 插件市场兼容性，不能只更新源码而忽略商店要求。

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

## CPA 插件市场规则

以 [CLIProxyAPI Plugins Store 官方说明](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements) 为准：商店会读取插件仓库的最新 GitHub Release，Release 标签是插件版本的事实来源。`registry.json` 中的 `version` 是可选的旧版兼容字段，只在无法查询最新 Release 时用于展示。

- 每次发布新版本：只要 GitHub Release 的标签、压缩包、文件布局和 `checksums.txt` 符合官方规范，商店会自动获取最新版本，不需要修改 `registry.json`，也不需要为普通版本升级创建新 PR。
- 首次上架 PR 仍为 **Open**：更新同一个 PR 的来源分支和 PR 正文，使其引用最新的已发布版本及构建证据；如果 PR 中保留了兼容字段 `version`，也同步更新。不要另开重复 PR。
- 首次上架 PR 已 **Merged**：不要再修改或尝试复用旧 PR。后续普通版本升级只发布新的 GitHub Release。
- 只有插件的注册元数据发生变化，例如名称、描述、仓库地址、Logo、主页、许可证或标签需要调整时，才按商店流程提交新的注册表 PR。
- PR 为 **Closed 且未合并**：检查关闭原因并修正；不能直接假设旧 PR 仍有效，必要时创建新的合规上架 PR。

当前首次上架 PR：<https://github.com/router-for-me/CLIProxyAPI-Plugins-Store/pull/78>

处理商店事项前必须重新查询首次上架 PR 的状态，不能依赖本文件记录的历史状态。
