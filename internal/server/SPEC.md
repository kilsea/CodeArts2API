---
id: server
type: module-design
title: server —— HTTP 兼容层与控制台
status: draft
parent: architecture
references: [adr-0002, adr-0007]
---

# server

## 职责

网关唯一的组合根，也是唯一的对外表面：OpenAI / Anthropic 兼容的推理接口、
内嵌控制台的 API 与静态资源、`/metrics`、以及每日领取运营活动的后台调度。

它负责所有「请求上下文相关」的判断 —— 鉴权、跨域、限流、选模型、账号轮换、重试退避、流式转发。

## 边界

- **可以依赖四个下层包**（`apierr` `codearts` `protocol` `registry` `store`），
  下层不得反向依赖它。
- **不实现协议转换**（`protocol`）、**不实现签名与上游交互**（`codearts`）、
  **不自己拼错误码表**（`apierr`）。

## 关键决策

- **「本机直连」必须同时看 `RemoteAddr` 与代理头。** 同机反向代理
  （`nginx proxy_pass 127.0.0.1:10087`）会把**所有**公网请求的 `RemoteAddr` 变成 `127.0.0.1`。
  只看 `RemoteAddr`，未设管理密码时的管理接口（含导出完整登录态）就会暴露给公网。
  因此只要出现任一常见代理头，就一律按「非本机」处理。
- **跨域默认只放行同源，不提供通配符。** 需要别的来源时在设置里显式列出。
  曾经默认放行任意 localhost 端口，等于让本机随便一个 Web 服务都能读到管理接口的响应。
- **未签发密钥时，匿名调用仅限本机直连。** 否则一旦监听 `0.0.0.0`（Dockerfile 的默认 CMD），
  任何人都能白嫖账号额度。对外匿名必须显式打开 `allow_anonymous`。
- **管理接口的写操作做同源校验**，阻断跨站请求伪造。
- **限流挂在鉴权之后，拒绝发生在调用上游之前。** 顺序反了会让未鉴权的请求也能建桶；
  放在上游之前则保证被拦下的请求**不消耗账号额度**。
- **每个端点自己声明错误风格**（`errStyle`），不按 URL 猜协议。两套 SDK 的错误体结构不通用：
  OpenAI 是 `{"error":{...}}`，Anthropic 是 `{"type":"error",...}`。
- **每日领取用「定时轮询 + 按本地日期去重」，不算准时刻触发一次。** 这样可以避开休眠唤醒、
  时区切换、进程重启导致的漏领。只领服务端说能领的，绝不伪造触发事件。
- **测活是「花钱的端点」，成本闸门必须内置。** 每个模型都是一次真实上游请求、真实扣积分，
  因此 `POST /admin/api/models/probe` 自带三道闸：单次最多 64 个模型（超限报 400 而不是静默截断）、
  同一时刻只允许一个测活任务（`probeMu.TryLock`，并发返回 `409`）、每次「模型 × 账号」尝试 30s 超时。
  前端在全量测活前还要二次确认。
- **活动福利按账号查询，不替用户选账号。** 每个账号的可领项与邀请码都不同，
  用 `NextAccount()` 随机挑一个会让人误以为「只有这个号能领」（邀请链接尤其坑：
  看到 A 号却生成 B 号的链接）。所以接口接受 `?id=`，并把 `accounts` 列表一并返回给下拉框。
- **积分额度是只读统计查询，必须缓存。** `GET /admin/api/credits` 每个账号一次上游请求
  （snap-manager 统计插件），不消耗积分但也不该被切页签反复触发：服务端按账号缓存 60 秒，
  `?fresh=1` 才回源；并发 4 路，避免打满单账号 3 个并发会话的上限。

- **请求侧的错误必须在触达上游之前判定。** `rolenorm.go`（角色规范化）与
  `validate.go`（参数校验）都在 `rewriteModel` 之前跑：上游对非法 role 的报错文案是
  「Message role cannot empty」（与真实原因无关），而任何一次非法参数的透传都是一次
  真实扣积分的上游调用。判定完再转发，客户端拿到的也是带 `code` / `param` 的规范错误体。
- **模型名绝不静默回落。** 内置兼容别名（`gpt-4o` → `glm-5.3` …）默认关闭；
  打开后每次命中都要回写 `X-Gateway-Model-Fallback` 并记告警日志，响应体 `model`
  始终是实际执行的模型。响应体回填由 `respmodel.go` 负责（流式逐片、非流式整体）。
- **每账号并发闸门在网关侧排队，不打到上游。** `store.AcquireSlot` 是每账号容量 3 的
  带缓冲 channel：调度器看 `len(ch)` 决定跳过谁，请求路径在 `ch` 上排队。
  排队超时返回 `apierr.AccountBusy`（429 + `Retry-After`），而不是让上游回 `TM.00001041`。
- **请求关联 id 不依赖中间件。** `withRequestIDValue` 在 handler 入口兜底生成
  `X-Request-Id` 并塞进 context，因此直接调用 handler（测试、重放）也满足
  「响应头一定带 id」这条契约。
- **采样默认关闭，且存的是改写前的原始请求体。** 存改写后的体等于让重放永远成功，
  失去排障价值。见 `dump.go` / `replay.go`。
- **Responses 的 `store` / `previous_response_id` 只存内存（`responsestore.go`）。**
  条目里是完整对话正文，落盘等于把全部对话永久留在磁盘上（与「默认不记录任何对话
  内容」冲突）。TTL 1h / 上限 256 条；失效返回 404 `previous_response_not_found`，
  客户端重发上下文即可。见 adr-0007。
- **Responses 翻译出来的 Chat 请求体要再走一遍 `validateChatRequest` + `normalizeRoles`。**
  否则会出现「Responses 能过、Chat 不能过」的不一致，而且翻译错误只能到上游才暴露。

## 已知限制

- **Responses 的 `store` 不跨进程、不跨实例。** 重启或换实例后旧 `previous_response_id`
  一律 404；多实例部署需要粘性路由（或客户端自己携带完整上下文）。
- **Responses 的 `background` / `conversation` / `prompt` / 非 function 工具未实现。**
  都返回 400 `unsupported_parameter`，不静默降级。

- **限流只做单密钥维度。** 网关侧的 `ratelimit` 只按密钥做 QPS / 突发 / 在飞并发；
  账号维度的并发由 `store` 的每账号信号量兜住（每账号 3 个，排队 20s），
  但**没有全局并发上限** —— 密钥数量 × 每密钥并发仍可能超过账号池总容量。
- 控制台是单页 + 轮询，不用 SSE（`adr-0004` 记录了这个取舍）。
