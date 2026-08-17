# CPAMP 插件登录态桥接维护说明

## 目的

CPA 插件资源运行在 iframe 中，默认无法继承 CPAMP 内存中的 Manager Server 管理员登录态。额度容量预测页面因此可能再次要求输入密钥。

插件从 v0.4.2 起会在同源部署中兼容读取 CPAMP 通过“记住密码”持久化的 `cli-proxy-auth` 登录信息。若 CPAMP 与 CPA 跨域，或者用户没有让 CPAMP 持久保存密钥，仍需使用下述消息桥，才能完全复用当前 CPAMP 会话而不让密钥进入 iframe。

可选的 CPAMP 定制可以增加受限消息桥：插件只把 API 请求描述发送给父页面，由父页面现有 `apiClient` 附带登录态发起请求。管理员密钥本身不会进入 iframe。

## 升级前检查

1. 先检查 CPAMP 官方最新版是否已经支持插件 iframe 复用父页面认证。
2. 如果官方已实现，优先采用官方方案，不要重复应用本补丁。
3. 如果没有，在新版源码中重新修改：
   `src/features/plugins/PluginResourcePage.tsx`。
   可先尝试 `git apply patches/cpamp-plugin-auth-bridge.patch`；如果新版上下文已变化，再按下方协议手工移植。

## 桥接协议

- 请求消息类型：`cpamp:plugin-api-request`
- 响应消息类型：`cpamp:plugin-api-response`
- 只接受当前插件 iframe 的 `event.source` 和相同 `origin`。
- 只允许 `GET`、`POST`。
- 路径必须以 `/v0/management/<当前 pluginID>/` 开头。
- 父页面调用 `apiClient.get` 或 `apiClient.post` 时，要去掉路径开头的 `/v0/management`，因为 `apiClient` 的 `baseURL` 已包含该前缀。否则会请求到重复路径 `/v0/management/v0/management/...`。
- 返回状态码与 JSON 数据，不返回或传递管理员密钥。

插件仪表盘的对应实现位于 `web/dashboard.html`：先短暂探测 `postMessage` 桥；桥不存在时尝试读取同源 CPAMP 已持久化的登录信息，再回退到 CPA Management Key。回退登录可选择使用 `localStorage` 长期记住，默认仍只使用 `sessionStorage`。因此该补丁不是插件运行的必要条件。

## 验证

应用到目标 CPAMP 版本并重新构建后，至少验证：

1. 登录 CPAMP 后打开插件资源页。
2. iframe 不显示密钥框，摘要、曲线和预测均能加载。
3. 刷新或离开页面再返回，摘要与曲线仍能正常加载。
4. 浏览器网络请求中不存在重复的 `/v0/management/v0/management/`。
5. 在未应用桥接、CPAMP 未保存密钥或独立打开仪表盘时，页面能在短暂探测后显示 CPA Management Key 登录框。
