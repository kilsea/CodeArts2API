---
id: adr-0007
type: architecture-design
title: "ADR 0007：Responses API 翻译到 Chat Completions，状态只放内存"
status: active
parent: architecture
references: [adr-0003]
---

# ADR 0007：Responses API 翻译到 Chat Completions，状态只放内存

- 状态：已采纳
- 日期：2026-11-20

## 背景

OpenAI 的 `/v1/responses` 已成为官方 SDK 的默认推荐接口，不少客户端（Codex CLI、
新版 Agent 框架）只发这一种请求。而本网关的上游（CodeArts inferhub）**只有**
`POST /api/v2/chat/completions`（见 `docs/UPSTREAM.md` §1）。

因此只有两个选择：

1. 不支持 —— 这些客户端直接不可用；
2. 网关侧做双向翻译，和 Anthropic 面走同一条路。

选 2。与 Anthropic 面的差别在于：Responses 是**有状态**协议
（`store` / `previous_response_id`），并且流式事件是语义事件而不是增量 chunk。

## 决策

### 1. 请求 / 响应 / 事件流全部翻译，不做透传

- `input`（字符串或条目数组）→ `messages`；
- `instructions` → 前置 `system`，续写时**覆盖**历史里的 system 提示；
- 扁平 `tools` → Chat 的嵌套 `function`；
- `function_call` / `function_call_output` ↔ `assistant.tool_calls` / `role=tool`；
- 上游 `reasoning_content` → `reasoning` 输出条目（完整思维链放 `summary`）；
- 上游 SSE → `response.created` … `response.completed` 事件序列。

翻译出来的 Chat 请求体**再走一遍** `validateChatRequest` + `normalizeRoles`，
保证两个协议面的参数策略、错误码、`param` 路径完全一致。

### 2. 转换层放在 `internal/protocol`，流式做成状态机

沿用 `adr-0003` 的边界：`protocol` 不碰 HTTP、不依赖内部包。

流式不能写成「边读边写」—— 那样事件顺序只能靠真实调用验证。这里做成
`ResponsesStream.Feed(chunk) ([]StreamEvent, error)` / `Finish()` / `Fail()`：
输入分片，输出「本次该写哪些事件」。于是 20+ 条事件的顺序、`sequence_number`
连续性、`response.completed` 的完整性都能零成本单测
（`internal/protocol/responses_test.go`）。

### 3. `store` / `previous_response_id` 只存内存

- 内存 TTL 1 小时、上限 256 条（FIFO 淘汰）；
- **不落盘**：条目里是完整对话正文，写进 `state.json` 等于把全部对话永久留在
  磁盘上，与「默认不记录任何对话内容」直接冲突；
- 失效后返回 404 `previous_response_not_found`（OpenAI 同码），客户端据此重发上下文。

### 4. 不支持的能力显式 400，不静默降级

`background`、`conversation`、`prompt`、`truncation:auto`、非 function 工具、
`input_file`、`item_reference`、`text.format=json_schema` 一律 400
`unsupported_parameter` + 精确 `param`。

`reasoning.effort` / `reasoning.summary` / `text.verbosity` / `include` /
`max_tool_calls` / `service_tier` 这类「上游没有对应能力但丢掉无害」的字段，
摘掉并在 `X-Gateway-Unsupported` 声明 —— 与 Chat 面对 `seed` / `logprobs` 的策略一致。

### 5. 流式必须注入 `stream_options.include_usage`

上游默认不在流里发 usage，而 `response.completed.usage` 是客户端读计费信息的唯一来源。
2026-11-20 实测上游支持该字段（末片 `choices: []` + `usage`）；不支持时退化为
字符数估算，至少保证字段存在。

## 后果

### 好处

- 只发 Responses 的客户端可直接接入，且工具调用、思维链、流式事件都完整；
- 事件顺序被单测钉死，不需要靠真实调用反复试错；
- 不落盘 → 不新增磁盘上的对话正文，安全边界不变。

### 代价

- `background` / `conversation` / 内置工具等能力**没有**，必须由客户端规避；
- 多轮续写依赖进程内状态：重启或多实例（无粘性路由）后旧 id 失效；
- `reasoning` 条目只有 `summary`（上游给的是完整 CoT，没有 summary 通道），
  与 OpenAI 原生「`content` 存加密推理、`summary` 存摘要」的语义不完全一致。

### 回归防护

`internal/protocol/responses_test.go`（金标准）与
`internal/server/responses_test.go`（端到端，顶替上游）覆盖：

- input 三种条目、连续 `function_call` 合并、`instructions` 覆盖；
- 工具声明 / `tool_choice` / `text.format` 的翻译；
- 拒绝清单与摘掉清单；
- 非流式文本 / 工具 / `finish_reason=length` 的映射；
- 流式事件完整序列 + `sequence_number` 连续性 + 无 usage 兜底 + `response.failed`；
- `previous_response_id` 续写（含「流式 → 存快照 → 续写」）、未知 id 404；
- `store=false`、GET / DELETE / `input_items`、`input_tokens`、路由优先级。

`scripts/contract_test.sh` 的 #10 用**真实上游**再验一遍非流式 / 流式 / 续写 / 拒绝。
