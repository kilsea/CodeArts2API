# 积分记账：逐请求账单 + 按账号归档

> 适用版本：2026-11-20 起（`feat/credit-usage`）。
> 结论来源：CodeArts Agent 26.11.200 安装包逆向（`out/extension.js` 的
> `queryTurnUsage` / `querySessionUsage` / `calculateTurnCredits`）+ 真实扣费标定。

## 0. 一句话

上游**有**逐请求的积分账单接口：`GET /v1/ops/credit/usage?type=msg&id=<user-msg-id>`。
网关自己决定 `user-msg-id` 请求头，所以每个请求结束后可以反查「这次到底扣了多少」；
查不到（网络/上游异常/该请求没计费）时才按模型价格档位在本地估算。

## 1. 上游账单接口

```text
GET https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/credit/usage
    ?type=msg&id=<user-msg-id>        # 一轮对话
    ?type=session&id=<x-ot-session-id> # 一个会话窗口的累计
Authorization: SDK-HMAC-SHA256 ...      # 与其它 snap 接口同一套签名
Agent-Type: PromptCenter
```

响应 `data`（驼峰字段，网关内部转成下划线命名）：

| 字段 | 含义 |
| --- | --- |
| `promptTokenUsed` / `completionTokenUsed` / `cachedTokenUsed` / `totalTokenUsed` | 计费口径的 token 数 |
| `cacheHitRatio` | 缓存命中率（0–1，部分响应不带） |
| `creditCost` | **上游账单口径的积分消耗**，两位小数 |

`code != 0` 且无 `data` 表示查询失败（例如 `40401 session not found`），
网关按「无账单」处理并退回估算。

实现：`internal/codearts/creditusage.go`（`FetchCreditUsage`）。
`user-msg-id` 由 `codearts.WithMsgID` 逐请求指定（`internal/codearts/client.go`），
账目结算在 `internal/server/credits.go`（`settleCredits`）。

### 为什么不是「先查再回」

反查发生在**响应已经写给客户端之后**（流式是 `[DONE]` 之后），最多等 2 秒；
它只影响处理函数返回的时刻，不增加用户等待。日志里的 `latency_ms` 不含这段
（`withElapsed` 单独覆盖）。

## 2. 本地估算公式（兜底）

与上游前端 `calculateTurnCredits` 逐字一致：

```text
credits = ( (input - cached) × input_price
          + cached          × cache_price
          + output          × output_price ) / ratio / 1e6
```

- `ratio` 是「1 积分折合多少元」，**不是**折扣系数；
- 三个 price 的单位是「元 / 百万 token」，来自 `GET /v1/model/builtin` 的 `credit[]`；
- 只取第一档（同一模型多档时，实测扣费用第一档）；
- 结果四舍五入到两位小数（与上游 `Math.round(x*100)/100` 一致）。

### 标定证据（2026-11-20，真实扣费）

| 模型（ratio / in / cache / out） | 用量 | 实测 | 公式 |
| --- | --- | --- | --- |
| deepseek-v4.1-flash-1017（0.0486 / 2 / 0.2 / 8） | 36568 无缓存 | 1.51 | 1.5062 |
| 同上 | 27035 输入 / 26880 缓存 | 0.12 | 0.1183 |
| glm-5.3（0.0443 / 8 / 2 / 28） | 35608 无缓存 | 6.44 | 6.4354 |
| 网关实测（消息账单接口） | 36568 无缓存 | **1.51** | — |

「实测」列来自账号积分快照差值（`/snap-manager/v1/statistics/plugin` 的
`package_credit_used`）与消息账单接口的 `creditCost`，两者互相印证。

## 3. 数据模型（积分跟着账号）

| 数据 | 位置 | 粒度 | 来源 |
| --- | --- | --- | --- |
| `LogEntry` | 日志（JSONL + 内存） | 单条请求 | `credits` / `credit_source` / `account_id` / token |
| `UsageRow` | `state.usage` | **天 × 账号 × 模型** | 逐请求累加（优先账单，兜底估算） |
| `CreditSpendRow` | `state.credit_spend` | 天 × 账号 | 积分快照 `used` 的差值（**账号真实账单**） |
| `BalanceSample` | `state.balances` | 30 分钟 × 账号 | 积分快照的**剩余额度**（水位曲线，保留 32 天） |

- `credit_source`：`upstream`（账单接口） / `estimate`（本地估算） / 空（未知，没价格也没账单）。
- 旧版本（v2）的 `UsageRow` 没有账号，加载后 `account=""`，在控制台归到
  「（历史，无账号）」，但仍计入总账。
- `CreditSpendRow` 覆盖**整个账号**：IDE 自己用掉的积分也会体现在差值里，
  因此它 ≥ 网关记录值，两者并排看才有意义。
- 快照由后台循环每 10 分钟刷新一次；额度重置（`used` 变小）不记负数。
- 基线用账号上的 `credit_used_at` 标记（不是「`used > 0`」）：重置到 0 之后
  第一次增长（0 → x）也必须记上，旧状态文件升级后第一次刷新只建立基线。
- `BalanceSample` 和 `CreditSpendRow` 是互补的：后者是「花掉多少」的账本
  （只有增量，画不出水位），前者是「还剩多少」的快照（能看出充值台阶、
  额度重置和掉额斜率）。同桶（30 分钟）覆盖写，保留 32 天。
  第一次采样时会用上一次快照补一个历史起点，所以新部署不用等半小时才有线。

## 4. 聚合与接口

`GET /admin/api/usage?days=N` 一次返回所有切面（口径只有服务端一处）：

```json
{
  "days": 7,
  "rows":       [ { "day","account_id","account","model","requests","chars_in","chars_out",
                    "in_tokens","out_tokens","cached_tokens","credits" } ],
  "total":      { "requests","chars_in","chars_out","in_tokens","out_tokens","cached_tokens","credits" },
  "by_day":     [ ... ],
  "by_model":   [ ... ],
  "by_account": [ ... ],
  "actual":     [ { "day","account_id","account","credits" } ],
  "actual_total": 12.34
}
```

可选 `&account=<账号 ID 或展示名>&model=<模型 ID>` 收窄所有切面（含 `actual`）。

`GET /admin/api/logs` 新增两个筛选参数：

- `account=<展示名>` —— 只看某个账号；
- `min_credits=<浮点>` —— 只看积分 ≥ 该值的请求（「哪些请求最烧钱」）。

## 5. 控制台

- **概览**：只放总体口径（总请求、失败、累计输入/输出 token、累计积分、账号就绪数），
  明细全部在「用量」页 —— 概览不做表格，避免首屏被用量明细淹没；
  「服务运行时间」放在侧栏底部（进程启动至今，重启即归零）。
- **用量 → 每小时用量**：一张图三条序列（默认「今天」，可切 24/48/72 小时）：
  请求数 = 柱（左轴）、token = 实线 + 面积（右轴）、积分 = 虚线（左内轴）。
  三个单位量纲差太远，所以各给一根轴，颜色与图例一一对应；窄屏隐藏积分轴，
  积分仍可从图例与悬停提示读。
  小时桶由 `state.usage_hour` 提供（保留 72 小时，超期自动裁掉）。
- **用量 → 按模型 / 按账号**：请求、输入/输出 token、缓存命中率、积分、平均延迟、
  吐字速度（输出 token ÷ 有输出的请求耗时，含首字等待）。
  平均延迟与吐字速度只算各自「有记录」的请求（分母不同：`timed_requests` / `out_latency_ms`），
  避免升级前没有耗时的历史行把速度算成虚高。
- **用量 → 每日用量与积分**：按天并列「网关账单 / 上游实测 / 差额」；差额来自账号在
  IDE 或其它客户端的消耗，以及快照刷新滞后。
- **日志**：新增「积分」列（`≈` 表示估算），支持「积分 ≥」筛选与账号筛选。
- **账号**：额度格子里补「今日 x 积分」（来自 `/admin/api/usage?days=1`）。
- **余额**（`GET /admin/api/balance-trend?days=7/14/30`）：
  - 顶部总账：账号数 / 可用数、当前合计、窗口净变化（末值 − 首值）、采样点数；
  - 趋势图：每账号一条**阶梯线**（余额在两次采样之间不变），Y 轴不从 0 起；
    `downsampleBalance` 在点数超过 400 时按组保留**最后一个**值（不取平均，
    否则充值台阶会被抹成斜坡）；
  - 每日变化：一天一列，列内按账号顺序并排；绿 = 当日净增、红 = 当日净减。
    「采样不完整」用**首末采样覆盖的时间跨度**判定（≥ 75% 当天时长），
    而不是数采样点：上游慢导致间隔变稀不等于覆盖不全；
  - 按账号表：当前 / 最低 / 最高 / 净变化 / 今日 / 昨日 / 消耗* / 到账* / 采样点。
    今日 = 今日末 − 昨 日末；消耗 / 到账把相邻采样的正负差分别累加
    （充值、额度重置都计入到账）。

## 6. 上游调用频率（防封）

余额 / 用量页本身**不打上游**：`/admin/api/balance-trend` 和 `/admin/api/usage` 都只读状态文件。
真正的上游请求只有这几处，都可以调或关：

| 来源 | 频率 | 怎么关 |
| --- | --- | --- |
| 后台积分快照（余额页的数据源） | 每个账号 10 分钟 1 次只读统计查询（`/v1/ops/credit/info`，不消耗积分），账号间随机错开 0.3~1.5s | `-credit-refresh 0` |
| 账号页积分列 | 页面开着时 5 分钟 1 次（客户端节流 + 服务端 60s 缓存）；手动「刷新」立即回源 | 不打开账号页即可 |
| 模型清单 | 启动 1 次 + 每 `-models-sync`（默认 6h） | `-models-sync 0` |
| 每笔请求的账单反查 | 与真实调用量 1:1（`/v1/ops/credit/usage`，2s 超时，失败退估算） | `-credit-lookup 0` |
| 续期（`auto_refresh`） | 凭据快过期时 1 次 | `-no-refresh` / 控制台关闭自动续期 |

失败处理：积分快照刷新失败按 10m→20m→…→2h 指数退避，不会拿过期凭据每 10 分钟反复敲门；
成功后立刻恢复。

## 7. 已知限制

1. **精度 0.01**：账单接口本身就是两位小数，几十 token 的请求会显示 0.00。
2. **重试共享 msg id**：一次网关请求（换号重试 N 次）用同一个 `user-msg-id`；
   失败尝试通常不计费，极端情况下（上游已开始推理再断）可能与成功那次合并。
3. **账单查询是额外一次上游 GET**：免费、只读，超时 2 秒；失败静默退回估算。
   测试里通过 `Server.creditLookup` 关闭（`newTestServer` 构造的服务默认关）。
4. **快照差值有滞后**：最长 10 分钟；把网关停机期间的消耗也记到恢复后那一天。
5. **历史用量行无账号**（升级前数据），无法再拆分。

## 8. 排查

| 症状 | 看什么 |
| --- | --- |
| 日志积分显示「≈」 | 账单接口失败 → 查 `credit_source=estimate`，对照上游 `/v1/ops/credit/usage` 是否 5xx |
| 积分全是「—」 | 模型没有价格档位（`/v1/model/builtin` 的 `credit[]` 为空）且账单查询也失败 |
| 网关统计远小于「上游实测」 | 该账号在 IDE 里也有用量；差值属正常 |
| 想知道某个请求到底扣多少 | 用 `x-request-id` 找到日志行，看 `credits` / `credit_source`；日志里的 `upstream_msg_id` 可直接拿去上游 `GET /v1/ops/credit/usage?type=msg&id=<它>` 复核账单 |
