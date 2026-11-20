# 客户端兼容矩阵

> 目标：**任何主流 AI 客户端都能无痛接入**，且「不支持」的部分必须写在这里、
> 而不是让用户自己撞出来。
>
> 本文件是**承诺**：下面标 ✅ 的行为都有测试钉住（`internal/server/*_test.go`
> 与 `scripts/contract_test.sh`）。改行为必须先改这张表。
>
> 对齐版本：v0.1.8 基线 + 兼容改造（角色规范化 / 模型校验 / 错误归一 /
> thinking signature / 请求校验 / 能力矩阵 / 并发闸门 / 采样重放），
> 以及 **Responses API 翻译层**（`docs/adr/0007-responses-api.md`）。

---

## 1. 端点

| 端点 | 状态 | 说明 |
| --- | --- | --- |
| `POST /v1/chat/completions` | ✅ | OpenAI 兼容，流式 / 非流式 / tools |
| `POST /v1/messages` | ✅ | Anthropic Messages，含 thinking signature |
| `POST /v1/messages/count_tokens` | ✅ | 本地估算（与日志里的 `prompt_len` 同一口径），不打上游 |
| `GET /v1/models` | ✅ | 上游清单 + 自定义别名 |
| `POST /v1/responses` | ✅ | OpenAI Responses API（流式 / 非流式 / tools / `previous_response_id`） |
| `POST /v1/embeddings` | ❌ | **未实现** |
| `POST /v1/audio/*` | ❌ | **未实现** |
| `POST /v1/images/generations` | ❌ | 上游有多模态模型，但网关不做图像生成 |
| `GET /metrics` | ✅ | Prometheus 文本格式（管理鉴权） |
| `/admin/api/*` | ✅ | 控制台 API（管理鉴权 + 同源校验） |
| `POST /admin/api/replay/<request-id>` | ✅ | 失败请求重放（需开采样，见 §9） |
| `GET /admin/api/samples` | ✅ | 调试采样列表（最新在前；开关/采样率/目录/保留天数） |
| `GET /admin/api/samples/<request-id>` | ✅ | 单条采样详情（脱敏后的请求体） |
| `GET /admin/api/logs` | ✅ | 请求日志，**最新在前**；支持 `status`（精确/`4xx`）/ `class` / `model` / `q` / `since` / `limit` |
| `GET /admin/api/usage` | ✅ | 用量趋势（按「天 + 模型」聚合，`?days=7`） |

---

## 2. 角色映射（`messages[].role`）

上游只认 `system` / `user` / `assistant` / `tool`，其余一律回
`HTTP 400 {"error_code":…,"message":"Message role cannot empty"}` ——
那句文案是错的（role 并不空），会把排查方向带偏。网关在转发前统一改写：

| 客户端发来的 role | 网关处理 | 状态 |
| --- | --- | --- |
| `system` / `user` / `assistant` / `tool` | 原样（统一小写） | ✅ |
| `developer`（OpenAI 2024 新规范） | → `system` | ✅ |
| `function`（旧 tool calling） | → `tool`，`name` 映射为 `tool_call_id` | ✅ |
| `""` / 字段缺失 | 按位置推断：第 0 条 → `system`，其余 → `user` | ✅ |
| `assistant` + `content:null` + `tool_calls` | 原样保留（上游支持） | ✅ |
| 其它（`critic` / `narrator` …） | **400** `invalid_role` + `param=messages[i].role` | ✅ |

`function → tool` 的 `tool_call_id` 优先复用前面 assistant 消息里声明的真实 id
（按函数名匹配），找不到才合成一个稳定值 `call_<函数名>_<下标>`。

---

## 3. 请求参数

| 参数 | 策略 | 状态 |
| --- | --- | --- |
| `model` | 必须是清单里的模型或已配置别名，否则 **404** `model_not_found` + `param=model` | ✅ |
| `temperature` | `[0, 2]`，越界 **400** `invalid_value` | ✅ |
| `top_p` | `[0, 1]`，越界 **400** | ✅ |
| `presence_penalty` / `frequency_penalty` | `[-2, 2]`，越界 **400** | ✅ |
| `max_tokens` / `max_completion_tokens` | 必须为正整数，否则 **400** | ✅ |
| `n` | **只支持 1**；`n != 1` → **400** `unsupported_parameter`（明确拒绝，不悄悄降级） | ✅ |
| `response_format` | 只支持 `text` / `json_object`，其余 **400** `unsupported_parameter` | ✅ |
| `seed` | **不支持**：从请求体摘掉，响应头 `X-Gateway-Unsupported: seed` | ✅ |
| `logprobs` / `top_logprobs` | **不支持**：摘掉并声明 | ✅ |
| `tools` / `tool_choice` | 透传（上游支持 function calling） | ✅ |
| `stop` / `stream` / `user` / `metadata` | 透传 | ✅ |
| 请求体大小 | > 32 MiB → **413** `request_too_large` | ✅ |
| `messages` 条数 | > 2000 → **413** `request_too_large` + `param=messages` | ✅ |

**二选一原则**：不支持的能力要么显式 4xx，要么「摘掉 + 响应头声明」。
**绝不允许**静默丢弃 —— 客户端必须能看出「我以为生效的东西其实没生效」。

---

## 4. 模型名与别名

| 场景 | 行为 | 状态 |
| --- | --- | --- |
| 清单里的模型 ID（大小写无关） | 直接使用 | ✅ |
| 控制台配置的 `model_aliases` | 解析到目标模型（用户显式声明，不算回落） | ✅ |
| 内置兼容别名（`gpt-4o` / `claude-sonnet-4` / `gpt-5` …） | **默认 404**；只有显式打开「启用兼容别名」才生效 | ✅ |
| 打开兼容别名后命中 | 200 + 响应头 `X-Gateway-Model-Fallback: <实际模型>` + `X-Gateway-Model-Requested: <请求模型>` + 告警日志 | ✅ |
| 响应体 `model` 字段 | **始终是实际执行的模型**，不是请求里的名字 | ✅ |

> 为什么默认关：`gpt-4o` 实际由 GLM 回答并真实扣积分。用户以为换了模型、
> 其实只是换了个名字 —— 这是「静默烧钱」，与 README 的 404 承诺相反。
> 要长期使用这些名字，请配 `model_aliases`。

---

## 5. 错误语义

### 5.1 响应形状

OpenAI 侧：

```json
{"error":{"message":"…","type":"invalid_request_error",
          "code":"invalid_role","param":"messages[2].role",
          "upstream_error":"<仅当映射不出来时出现>"}}
```

Anthropic 侧：`{"type":"error","error":{"type":"…","message":"…","code":"…"}}`。

所有响应（成功与失败）都带 **`X-Request-Id`**，同值写入请求日志的
`request_id` 字段 —— 客户端贴一个 id，就能在控制台日志里定位那一条。
可重试的错误（429）额外带 `Retry-After`。

### 5.2 规范错误码

| `code` | HTTP | 含义 |
| --- | --- | --- |
| `invalid_role` | 400 | `messages[i].role` 不认识（`param` 指到具体下标） |
| `invalid_value` | 400 | 参数越界 / 类型不对 |
| `unsupported_parameter` | 400 | 参数合法但网关/上游不支持（`n>1`、`response_format` 等） |
| `request_too_large` | 413 | 请求体或 `messages` 超限 |
| `model_not_found` | 404 | 模型名不存在（不会静默换成别的模型） |
| `model_not_multimodal` | 400 | 给纯文本模型发了图片（上游 406） |
| `model_name_mismatch` | 400 | 上游 406：模型名与请求头不一致 |
| `content_policy_violation` | 400 | 命中内容审核（换账号也一样被拒，不重试） |
| `concurrency_limit_exceeded` | 429 | 全部账号的并发槽位占满，排队超时（带 `Retry-After`） |
| `rate_limit_exceeded` | 429 | 网关侧按密钥限流 / 上游 429 |
| `credentials_expired` | 502 | 账号登录态失效，需要重新登录（**别重试**） |
| `no_account_available` | 503 | 账号池为空 |
| `upstream_error` | 502 | 其余上游故障；原文在 `upstream_error` 字段 |
| `internal_error` | 500 | 网关自身问题 |

上游业务码到规范码的映射表见 [`UPSTREAM.md` §8](UPSTREAM.md)。

---

## 6.1 Responses API 兼容细节

上游只有 Chat Completions，所以 Responses 是**网关侧翻译**：请求变成 `messages`，
响应变成 `response` 对象与事件流。

| 能力 | 状态 | 说明 |
| --- | --- | --- |
| `input` 字符串 / 条目数组 | ✅ | `message` / `function_call` / `function_call_output` / `reasoning` |
| `instructions` | ✅ | 映射为前置 `system`；续写时**覆盖**历史里的 system 提示 |
| `max_output_tokens` / `temperature` / `top_p` / `parallel_tool_calls` | ✅ | |
| `text.format` = `text` / `json_object` | ✅ | |
| `text.format` = `json_schema` | ❌ | 400 `unsupported_parameter`（上游不支持结构化输出约束） |
| `tools`（`type=function`，扁平） | ✅ | 翻译为 Chat 的嵌套 `function` 形态 |
| 其它工具类型（`web_search` / `mcp` / `code_interpreter` …） | ❌ | 400 `unsupported_parameter`，不静默降级 |
| `tool_choice` = `auto` / `none` / `required` / 指定函数 | ✅ | `allowed_tools` → 400 |
| `input_image`（`image_url`） | ✅ | `file_id` 形式不支持 |
| `input_file` | ❌ | 400；请把文件内容当文本传入 |
| `item_reference` | ❌ | 400；请直接回传条目内容 |
| `reasoning_content` → `reasoning` 条目 | ✅ | 完整思维链放在 `summary[].summary_text` |
| 流式事件（`response.created` … `response.completed`） | ✅ | 带递增 `sequence_number`；**不发** `[DONE]` |
| `store` / `GET /v1/responses/{id}` / `DELETE` / `input_items` | ✅ | 仅**内存**（TTL 1h、上限 256 条） |
| `previous_response_id` | ✅ | 失效/重启后 404 `previous_response_not_found` |
| `POST /v1/responses/input_tokens` | ✅ | 本地估算，不打上游、不扣积分 |
| `background` / `conversation` / `prompt` / `truncation:auto` | ❌ | 400 `unsupported_parameter` |
| `reasoning.effort` / `reasoning.summary` / `text.verbosity` / `include` / `max_tool_calls` / `service_tier` | ⚠️ | 摘掉 + `X-Gateway-Unsupported` 声明 |

> **为什么 `store` 只放内存**：条目里是完整对话正文，落盘等于把全部对话永久留在磁盘上
> （项目默认不记录任何对话内容）。代价是进程重启 / 条目淘汰后旧 `previous_response_id`
> 失效 —— 客户端重发完整上下文即可。

---

## 6.2 Anthropic 兼容细节

| 特性 | 状态 | 说明 |
| --- | --- | --- |
| `text` / `image`（base64、url） | ✅ | |
| `tool_use` / `tool_result` | ✅ | |
| `thinking` 块 | ✅ | 上游 `reasoning_content` → `thinking` |
| `thinking.signature` | ✅ | 网关用 HMAC(正文) 派生；流式在块关闭前发 `signature_delta` |
| 客户端回传 thinking（带陌生 / 缺失签名） | ✅ | **忽略**而不拒绝（真实 Anthropic 签名我们验不了） |
| `redacted_thinking` | ⚠️ | 不回流（上游不产生） |
| `stop_sequences` | ✅ | 映射为 OpenAI `stop` |
| `system`（字符串或文本块数组） | ✅ | |
| `cache_control` | ❌ | 上游无 prompt caching，字段被忽略 |

> 为什么 signature 重要：Claude Code / 官方 SDK 在多轮回传 assistant 内容时
> 会校验 thinking 块的 signature，缺失会让**第二轮**直接 400 ——
> 表现是「第一轮能用、第二轮崩」，错误出现在客户端，极难排查。
>
> 多实例部署时设置同一个 `CODEARTS_THINKING_KEY`，签名才能互认
> （不设也能用：校验失败一律忽略）。

---

## 7. 模型能力矩阵

上游行为会漂移（`glm-5.3` 曾只支持流式，2026-11-20 实测非流式已返回 200），
因此网关维护一份**实测**能力快照，落在状态文件的 `capabilities` 字段：

| 字段 | 含义 |
| --- | --- |
| `stream_only` | 非流式会得到空 body 的 400，必须走「流式 + 本地聚合」 |
| `tools` / `images` / `response_format` | 支持的工具 / 多模态 / 结构化输出 |
| `probed_at` / `source` | 最后一次实测时间与来源（`probe` / `observed`） |

规则：

- **未知即可用**：没探测到的能力不写 false，探测失败绝不阻断请求（保守降级）；
- 请求路径观测到「空 body 的 4xx」→ 自动标记 `stream_only` 并改用流式聚合；
- 控制台「模型测活」发现非流式恢复可用 → **反向清除**标记；
- 只支持流式的模型在测活里报「可用 + stream_only」，不会误判成红叉。

---

## 8. 并发与限流

| 机制 | 行为 |
| --- | --- |
| 每账号并发闸门 | 上游硬上限 3 个并发会话；超限在网关**排队**（最长 20s），不把超限请求打到上游 |
| 排队超时 | 429 + `Retry-After: 5` + `code=concurrency_limit_exceeded` |
| 按密钥限流 | QPS / 突发 / 在飞并发，超限 429 + `Retry-After`（被拦下的请求不消耗额度） |
| 账号故障 | 冷却 + 半开探测（2m → 5m → 15m → 1h），到期自动放回轮转 |

---

## 9. 调试采样与重放

默认**不记录任何对话正文**。需要排障时在控制台「日志 → 调试采样」打开开关即可
（**默认关闭**，热生效，不用重启）：

- 采样率可选「全部 / 1/10 / 1/100」；
- 落盘位置：`<状态文件目录>/samples/`，可用 `CODEARTS_DUMP_DIR` 改；
- 文件按天切分：`requests-<YYYYMMDD>.jsonl`，单文件 8 MiB 封顶；
- **保留 3 天，超过 3 天的文件自动整份删除**（含旧版遗留的 `requests.jsonl`）；
- 存的是**改写前**的原始请求体（重放要复现客户端的真实行为）；
- 脱敏：超长字符串（图片 base64）替换为 `[redacted N bytes]`；权限 0600（非 Windows）；
- 控制台：`GET /admin/api/samples` 列表（最新在前）、`GET /admin/api/samples/<id>` 详情；
- 重放：`POST /admin/api/replay/<request-id>`（管理鉴权）。

`CODEARTS_DUMP_SAMPLE` / `CODEARTS_DUMP_DIR` 仍然可用，但只决定**启动时**的初始值，
运行期间以控制台设置为准（否则「页面上关了却还在写」会让人以为开关坏了）。

---

## 10. 明确「不支持」

以下能力**没有实现**，请不要假设它们存在：

- `/v1/embeddings`、`/v1/audio/*`、图像生成；
- Responses API 的 `background`（异步作业）、`conversation`、`prompt` 模板、
  `truncation:auto`、非 function 工具；
- per-key 配额与用量账单（只有 QPS / 突发 / 并发三道自我保护闸门）；
- 内置 TLS 与反向代理方案（请在前面放 nginx / caddy）；
- 多租户隔离（密钥之间共享账号池）；
- prompt caching（`cache_control` 被忽略）；
- `n > 1`（明确 400，不降级）；
- `logprobs` / `seed`（摘掉 + `X-Gateway-Unsupported` 声明）。

---

## 11. 客户端自检清单

接入前逐条确认：

1. `/v1/models` 能列出模型 → 说明鉴权与清单正常；
2. 发一句 `{"role":"user","content":"hi"}` → 200；
3. 发 `{"role":"developer",…}` → 200（说明角色规范化生效）；
4. 发 `{"model":"gpt-4o"}` → **404**（说明没有静默回落）；
5. 发 `{"temperature":99}` → 400 且 `error.param == "temperature"`；
6. 看响应头有没有 `X-Request-Id`；
7. Anthropic 客户端：跑两轮对话，第二轮不要崩；
8. Responses 客户端：非流式拿到 `object=response`，流式拿到 `response.completed`（且没有 `[DONE]`）。

上面 7 条就是 `scripts/contract_test.sh` 断言的内容。
