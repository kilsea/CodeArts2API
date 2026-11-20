---
id: websearch
type: module-design
title: websearch —— 联网检索与正文抓取
status: draft
parent: architecture
references: [adr-0001, adr-0007]
---

# websearch

## 职责

把「联网搜索」做成网关自己的一个下层能力：

1. **检索**：把查询发给可替换的后端（自托管 SearXNG / Tavily / Brave），拿回
   `title / url / snippet`；
2. **抽取**：可选地并发抓取前几条结果的正文（纯标准库 HTML → 文本）；
3. **拼装**：把结果拼成可注入 `system` 的文本块（带引用编号与「不可信数据」声明）。

HTTP 层的协议识别、注入位置、错误映射都在 `internal/server`（见 `server/websearch.go`）。

## 边界

- **不依赖任何内部包**（零第三方依赖，见 ADR-0001）。检索后端的 HTTP 调用、
  HTML 解析、SSRF 防护全部标准库实现。
- **不知道协议**：不认 `tools` / `input` / `messages`，只接受一个查询字符串。
- **不持久化**：只有进程内 LRU 风格缓存（`CacheTTL`、上限 256 条），重启即失效。
- **不重试**：一次检索一次尝试；重试等于放大对检索后端与目标站点的压力。

## 不变量

1. **抓取任意 URL 必须过 SSRF 防护**：只允许 http/https + 80/443，**在拨号时**校验
   解析后的 IP（环回 / 私网 / 链路本地 / CGNAT / 保留段一律拒绝），重定向最多 3 跳且每跳重校验。
2. **调用检索后端的客户端不做内网拦截。** SearXNG 通常就跑在 127.0.0.1 或 docker 内网，
   用同一个拦截器会把自家后端挡掉。两个客户端必须分开（`providers.go` 的 `apiClient`
   与 `extract.go` 的 `fetcher`）。
3. **抓正文失败不失败整次检索**：单页出错只丢该页正文，保留摘要。
4. **结果必须去重并按条数截断**：元搜索会从多个引擎返回同一页面。
5. **配置指纹变化必须清缓存**（`Service.Update`）：换了后端还命中旧结果会让人误判「新配置没生效」。
6. **注入块必须声明内容不可信**：网页是提示注入的高发面。

## 关键决策

- **只注入一轮，不做工具循环。** 多轮要多打一次上游（多花积分、多一次延迟），
  而主流客户端对内置搜索的预期就是「一问一答 + 引用」。需要深挖的客户端自己声明
  `type=function` 工具。
- **抓正文默认关闭。** 摘要通常够用，抓取是延迟与失败率的主要来源；打开后只抓前 3 条。
- **自写 HTML 抽取而不引入解析器。** 目标只是「读得懂」，不是还原排版；
  写错最坏是多几行噪音，不会让整页失效。
- **`web_search` 关闭时显式 400。** 静默摘掉工具会让用户以为模型查过了。

## 已知限制

- 不执行 JS、不解析 PDF / 附件（只处理 `text/html` 与 `text/*`）。
- 单轮检索：Anthropic 的 `max_uses` / `allowed_domains` / `blocked_domains` 不生效。
- 无搜索过程事件（Responses 不产 `web_search_call`，Anthropic 不产 `server_tool_use`）。
- 缓存键是 `provider|endpoint|条数|query`；`FetchPages` / `MaxPageChars` 属于配置变更，由 `Service.Update` 整体清缓存。
