#!/usr/bin/env bash
# 契约测试：对一个**正在运行**的网关断言状态码与关键字段。
#
# 用途：每次改动后跑一遍，确认「任何主流客户端都能无痛接入」这条承诺没被打破。
# 与 `go test ./...` 的分工：单测覆盖网关内部逻辑（顶替上游），
# 这个脚本覆盖**真实上游**的行为 —— 上游会漂移，只有真打才知道。
#
# 用法：
#   BASE_URL=http://127.0.0.1:10087 CODEARTS_API_KEY=xxx ./scripts/contract_test.sh
#
#   BASE_URL        网关地址（默认 http://127.0.0.1:10087）
#   CODEARTS_API_KEY  API 密钥（未签发密钥且本机直连时可留空）
#   ADMIN_PASSWORD  管理密码（仅 --record-golden 需要）
#   MODEL           用于正常请求的模型（默认 glm-5.3）
#
#   --record-golden  记录 /v1/models 的 golden 快照（其余用例照常跑）
#   --skip-slow      跳过并发用例（#7）
#
# 退出码：0 = 全部通过；1 = 有失败；2 = 环境不可用（连不上 / 无账号）。
#
# 注意：每条正常请求都会**真实消耗积分**。脚本刻意把请求压到最小
# （max_tokens 很小、消息只有一句），全量跑一次的成本是几次最小对话。

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:10087}"
API_KEY="${CODEARTS_API_KEY:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
MODEL="${MODEL:-glm-5.3}"
RECORD_GOLDEN=0
SKIP_SLOW=0

for arg in "$@"; do
  case "$arg" in
    --record-golden) RECORD_GOLDEN=1 ;;
    --skip-slow)     SKIP_SLOW=1 ;;
    -h|--help)       sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

HERE="$(cd "$(dirname "$0")" && pwd)"
GOLDEN_DIR="$HERE/golden"
TMPDIR_RUN="$(mktemp -d 2>/dev/null || mktemp -d -t contract)"
trap 'rm -rf "$TMPDIR_RUN"' EXIT

# Windows 上 python 向管道输出时用系统代码页（GBK），中文内容会被转成非法 UTF-8，
# 后续 json.load 直接报 UnicodeDecodeError（表现为「第二轮请求体为空」）。
# 强制 UTF-8 让所有平台行为一致。
export PYTHONIOENCODING=utf-8

PASS=0
FAIL=0
SKIP=0

c_green() { printf '\033[32m%s\033[0m' "$1"; }
c_red()   { printf '\033[31m%s\033[0m' "$1"; }
c_yellow(){ printf '\033[33m%s\033[0m' "$1"; }

ok()   { PASS=$((PASS+1)); echo "  $(c_green PASS)  $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  $(c_red FAIL)  $1"; }
skip() { SKIP=$((SKIP+1)); echo "  $(c_yellow SKIP)  $1"; }

# json_get <file> <python 表达式，d 为已解析的 JSON>
json_get() {
  python - "$1" "$2" <<'PY' 2>/dev/null
import json,sys
raw=open(sys.argv[1],encoding='utf-8').read()
try:
    d=json.loads(raw)
except Exception:
    print(""); raise SystemExit(0)
try:
    v=eval(sys.argv[2], {"d": d, "len": len, "str": str, "json": json})
except Exception:
    v=""
if isinstance(v,bool):
    print("true" if v else "false")
elif v is None:
    print("")
else:
    print(v)
PY
}

# request <method> <path> <body> [extra curl args...]
# 设置全局 STATUS / BODY_FILE / HEADER_FILE
request() {
  local method="$1" path="$2" body="${3:-}"; shift 3 || true
  BODY_FILE="$TMPDIR_RUN/body.$RANDOM.json"
  HEADER_FILE="$TMPDIR_RUN/head.$RANDOM.txt"
  local args=(-sS -X "$method" -o "$BODY_FILE" -D "$HEADER_FILE" -w '%{http_code}' --max-time 300)
  [ -n "$API_KEY" ] && args+=(-H "Authorization: Bearer $API_KEY")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$body")
  fi
  STATUS="$(curl "${args[@]}" "$@" "$BASE_URL$path" 2>"$TMPDIR_RUN/curl.err")"
  if [ -z "$STATUS" ]; then
    echo "  curl 失败: $(cat "$TMPDIR_RUN/curl.err")" >&2
    STATUS="000"
  fi
}

header_of() { grep -i "^$1:" "$HEADER_FILE" 2>/dev/null | head -1 | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'; }

body() { cat "$BODY_FILE" 2>/dev/null; }

expect_status() {
  local want="$1" name="$2"
  if [ "$STATUS" = "$want" ]; then
    ok "$name（$STATUS）"
    return 0
  fi
  bad "$name（状态码 $STATUS，期望 $want）：$(head -c 400 <<<"$(body)")"
  return 1
}

# ============================================================
# 前置：网关可达 + 有可用账号
# ============================================================

echo "== 环境 =="
echo "  网关: $BASE_URL"
echo "  模型: $MODEL"

if ! curl -sS -o /dev/null --max-time 10 "$BASE_URL/ping" 2>/dev/null; then
  echo "  $(c_red FAIL)  网关不可达：$BASE_URL/ping"
  exit 2
fi
ok "网关可达"

request GET /health ""
ACCOUNTS="$(json_get "$BODY_FILE" 'd.get("accounts",0)')"
READY="$(json_get "$BODY_FILE" 'd.get("ready",0)')"
echo "  账号: $ACCOUNTS（可用 $READY）"
if [ "${READY:-0}" -lt 1 ]; then
  echo "  $(c_red FAIL)  没有可用账号，无法跑真实上游用例"
  exit 2
fi

# 所有 /v1 响应都必须带 X-Request-Id（P0-3 的契约）。
RID="$(header_of X-Request-Id)"
if [ -n "$RID" ]; then ok "响应带 X-Request-Id（$RID）"; else bad "响应缺少 X-Request-Id"; fi

echo
echo "== 用例 =="

# ------------------------------------------------------------
# #1 developer → 200，且上游收到的 role 是 system
#    网关侧无法直接看上游请求体，这里断言 200 + 响应正常；
#    「上游收到 system」由 go test 的 compat_test 精确覆盖。
# ------------------------------------------------------------
echo "#1 role=developer"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"developer\",\"content\":\"你是助手\"},{\"role\":\"user\",\"content\":\"回答 ok 两个字\"}]}"
expect_status 200 "developer 角色被接受"

# ------------------------------------------------------------
# #2 function → 200（带 name，网关映射为 tool + tool_call_id）
# ------------------------------------------------------------
echo "#2 role=function"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"查天气\"},{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"id\":\"call_contract_1\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},{\"role\":\"function\",\"name\":\"weather\",\"content\":\"晴\"}]}"
expect_status 200 "function 角色被接受"

# ------------------------------------------------------------
# #3 空 role → 200（按位置推断：第一条 system，其余 user）
# ------------------------------------------------------------
echo "#3 role 为空"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"\",\"content\":\"你是助手\"},{\"content\":\"回答 ok\"}]}"
expect_status 200 "空 role 按位置推断"

# ------------------------------------------------------------
# #4 未知模型 → 404 model_not_found，且不消耗积分
# ------------------------------------------------------------
echo "#4 未知模型"
request POST /v1/chat/completions '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
if expect_status 404 "gpt-4o 被拒绝"; then
  CODE="$(json_get "$BODY_FILE" 'd["error"].get("code","")')"
  PARAM="$(json_get "$BODY_FILE" 'd["error"].get("param","")')"
  [ "$CODE" = "model_not_found" ] && ok "error.code=model_not_found" || bad "error.code=$CODE，期望 model_not_found"
  [ "$PARAM" = "model" ] && ok "error.param=model" || bad "error.param=$PARAM，期望 model"
  if grep -q '"model":"gpt-4o"' <<<"$(body)"; then
    bad "404 响应里出现了 gpt-4o 的模型名（可能回落了）"
  fi
fi

# ------------------------------------------------------------
# #5 n>1 → 策略：明确拒绝 400 unsupported_parameter
#    策略必须与文档、响应头一致（docs/COMPAT.md「n」一行）。
# ------------------------------------------------------------
echo "#5 n=2"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"n\":2,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
if expect_status 400 "n>1 被明确拒绝"; then
  CODE="$(json_get "$BODY_FILE" 'd["error"].get("code","")')"
  PARAM="$(json_get "$BODY_FILE" 'd["error"].get("param","")')"
  [ "$CODE" = "unsupported_parameter" ] && ok "error.code=unsupported_parameter" || bad "error.code=$CODE，期望 unsupported_parameter"
  [ "$PARAM" = "n" ] && ok "error.param=n" || bad "error.param=$PARAM，期望 n"
fi

# ------------------------------------------------------------
# #5b 不支持字段 → 200 + X-Gateway-Unsupported 声明
# ------------------------------------------------------------
echo "#5b seed/logprobs 声明"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"max_tokens\":16,\"seed\":42,\"logprobs\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
if expect_status 200 "带不支持字段的请求仍成功"; then
  UNSUP="$(header_of X-Gateway-Unsupported)"
  case "$UNSUP" in
    *seed*) ok "X-Gateway-Unsupported=$UNSUP" ;;
    *) bad "X-Gateway-Unsupported 未声明 seed（实际 '$UNSUP'）" ;;
  esac
fi

# ------------------------------------------------------------
# #6 /v1/messages 两轮（第二轮回传 assistant 内容，含 thinking + signature）
# ------------------------------------------------------------
echo "#6 Anthropic 两轮对话"
request POST /v1/messages "{\"model\":\"$MODEL\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"回答 ok 两个字\"}]}"
if expect_status 200 "第一轮 200"; then
  # 把第一轮的 content 原样回传（这正是 Claude Code 的行为）。
  CONTENT="$(json_get "$BODY_FILE" 'json.dumps(d.get("content",[]),ensure_ascii=False)')"
  HAS_THINKING="$(python - "$BODY_FILE" <<'PY' 2>/dev/null
import json,sys
d=json.load(open(sys.argv[1],encoding='utf-8'))
print("1" if any(b.get("type")=="thinking" for b in d.get("content",[])) else "0")
PY
)"
  if [ "$HAS_THINKING" = "1" ]; then
    SIG_OK="$(python - "$BODY_FILE" <<'PY' 2>/dev/null
import json,sys
d=json.load(open(sys.argv[1],encoding='utf-8'))
bs=[b for b in d.get("content",[]) if b.get("type")=="thinking"]
print("1" if bs and bs[0].get("signature") else "0")
PY
)"
    [ "$SIG_OK" = "1" ] && ok "thinking 块带 signature" || bad "thinking 块缺少 signature"
  else
    skip "本轮没有 thinking 块（模型未返回推理内容），跳过 signature 断言"
  fi

  # 注意：content 不能当命令行参数传给 python —— Windows 上非 ASCII 参数会被
  # 转成 ANSI 代码页，json.loads 直接报错（第二轮就会拿到空请求体）。
  # 改成写临时文件、由 python 读文件。
  CONTENT_FILE="$TMPDIR_RUN/anthropic-content.json"
  printf '%s' "$CONTENT" > "$CONTENT_FILE"
  TURN2="$(python - "$MODEL" "$CONTENT_FILE" <<'PY'
import json,sys
model = sys.argv[1]
content = json.load(open(sys.argv[2], encoding='utf-8'))
print(json.dumps({"model":model,"max_tokens":32,"messages":[
    {"role":"user","content":"回答 ok 两个字"},
    {"role":"assistant","content":content},
    {"role":"user","content":"再说一次"}]},ensure_ascii=False))
PY
)"
  request POST /v1/messages "$TURN2"
  expect_status 200 "第二轮（回传 assistant 内容）200"
fi

# ------------------------------------------------------------
# #7 并发：4 个账号 × 12 个请求，无上游 429、全部成功
# ------------------------------------------------------------
echo "#7 并发排队"
if [ "$SKIP_SLOW" = "1" ]; then
  skip "并发用例被 --skip-slow 跳过"
elif [ "${ACCOUNTS:-0}" -lt 4 ]; then
  skip "账号数 $ACCOUNTS < 4，并发用例需要 4 个账号才能验证排队"
else
  CONC_DIR="$TMPDIR_RUN/conc"
  mkdir -p "$CONC_DIR"
  START=$(date +%s)
  for i in $(seq 1 12); do
    (
      code=$(curl -sS -o "$CONC_DIR/$i.json" -w '%{http_code}' --max-time 300 \
        ${API_KEY:+-H "Authorization: Bearer $API_KEY"} \
        -H 'Content-Type: application/json' \
        --data-binary "{\"model\":\"$MODEL\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
        "$BASE_URL/v1/chat/completions" 2>/dev/null)
      echo "$code" > "$CONC_DIR/$i.code"
    ) &
  done
  wait
  ELAPSED=$(( $(date +%s) - START ))
  N_OK=0; N_429=0; N_OTHER=0
  for f in "$CONC_DIR"/*.code; do
    case "$(cat "$f")" in
      200) N_OK=$((N_OK+1)) ;;
      429) N_429=$((N_429+1)) ;;
      *)   N_OTHER=$((N_OTHER+1)) ;;
    esac
  done
  echo "  结果: 200×$N_OK  429×$N_429  其它×$N_OTHER  耗时 ${ELAPSED}s"
  if [ "$N_OK" -eq 12 ]; then ok "12 个并发请求全部成功"; else bad "只有 $N_OK/12 成功"; fi
  if [ "$N_429" -eq 0 ]; then ok "没有出现 429（排队生效）"; else bad "出现 $N_429 个 429"; fi
fi

# ------------------------------------------------------------
# #8 错误体形状：4xx + 规范错误体 + X-Request-Id
#    这里用「网关自己判定的请求侧错误」（temperature 越界）作为确定性触发；
#    上游业务错误（error_code）的映射由 go test 的 apierr/compat 用例精确覆盖。
# ------------------------------------------------------------
echo "#8 错误语义"
request POST /v1/chat/completions "{\"model\":\"$MODEL\",\"temperature\":99,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
if expect_status 400 "越界参数返回 400"; then
  TYPE="$(json_get "$BODY_FILE" 'd["error"].get("type","")')"
  CODE="$(json_get "$BODY_FILE" 'd["error"].get("code","")')"
  PARAM="$(json_get "$BODY_FILE" 'd["error"].get("param","")')"
  [ "$TYPE" = "invalid_request_error" ] && ok "error.type=invalid_request_error" || bad "error.type=$TYPE"
  [ "$CODE" = "invalid_value" ] && ok "error.code=invalid_value" || bad "error.code=$CODE"
  [ "$PARAM" = "temperature" ] && ok "error.param=temperature" || bad "error.param=$PARAM"
fi
RID="$(header_of X-Request-Id)"
[ -n "$RID" ] && ok "错误响应带 X-Request-Id（$RID）" || bad "错误响应缺少 X-Request-Id"

# ------------------------------------------------------------
# 回归基线：/v1/models 与 /v1/messages 的形状
# ------------------------------------------------------------
echo "#9 既有接口形状"
request GET /v1/models ""
if expect_status 200 "/v1/models 200"; then
  OBJ="$(json_get "$BODY_FILE" 'd.get("object","")')"
  N="$(json_get "$BODY_FILE" 'len(d.get("data",[]))')"
  [ "$OBJ" = "list" ] && ok "object=list" || bad "object=$OBJ，期望 list"
  [ "${N:-0}" -ge 1 ] && ok "模型数 $N" || bad "模型数为 0"

  if [ "$RECORD_GOLDEN" = "1" ]; then
    mkdir -p "$GOLDEN_DIR"
    python - "$BODY_FILE" "$GOLDEN_DIR/models.txt" <<'PY'
import json,sys
d=json.load(open(sys.argv[1],encoding='utf-8'))
ids=sorted(m["id"] for m in d.get("data",[]))
open(sys.argv[2],"w",encoding="utf-8").write("\n".join(ids)+"\n")
PY
    ok "已记录 golden：$GOLDEN_DIR/models.txt"
  elif [ -f "$GOLDEN_DIR/models.txt" ]; then
    GOLDEN_OUT="$(python - "$BODY_FILE" "$GOLDEN_DIR/models.txt" <<'PY'
import json,sys
d=json.load(open(sys.argv[1],encoding='utf-8'))
got=sorted(m["id"] for m in d.get("data",[]))
want=[l.strip() for l in open(sys.argv[2],encoding='utf-8') if l.strip()]
if got==want:
    print("GOLDEN_OK")
else:
    print("GOLDEN_DIFF 新增=%s 消失=%s" % (sorted(set(got)-set(want)), sorted(set(want)-set(got))))
PY
)"
    case "$GOLDEN_OUT" in
      GOLDEN_OK) ok "/v1/models 与 golden 快照一致" ;;
      *)         bad "/v1/models 与 golden 快照不一致：$GOLDEN_OUT" ;;
    esac
  else
    skip "没有 golden 快照（用 --record-golden 记录）"
  fi
fi

# ------------------------------------------------------------
# #10 Responses API：非流式 / 流式 / 续写 / 显式拒绝
# ------------------------------------------------------------
echo "#10 Responses API"
request POST /v1/responses "{\"model\":\"$MODEL\",\"input\":\"Reply with exactly: ok\",\"max_output_tokens\":32}"
RESP_ID=""
if expect_status 200 "非流式 200"; then
  OBJ="$(json_get "$BODY_FILE" 'd.get("object","")')"
  ST="$(json_get "$BODY_FILE" 'd.get("status","")')"
  NOUT="$(json_get "$BODY_FILE" 'len(d.get("output",[]))')"
  RESP_ID="$(json_get "$BODY_FILE" 'd.get("id","")')"
  [ "$OBJ" = "response" ] && ok "object=response" || bad "object=$OBJ，期望 response"
  [ "$ST" = "completed" ] && ok "status=completed" || bad "status=$ST"
  [ "${NOUT:-0}" -ge 1 ] && ok "output 条目数 $NOUT" || bad "output 为空"
fi

if [ -n "$RESP_ID" ]; then
  request POST /v1/responses "{\"model\":\"$MODEL\",\"previous_response_id\":\"$RESP_ID\",\"input\":\"继续，只回 ok\",\"max_output_tokens\":32}"
  expect_status 200 "previous_response_id 续写 200"
fi

request POST /v1/responses "{\"model\":\"$MODEL\",\"input\":\"hi\",\"stream\":true,\"max_output_tokens\":16}"
if expect_status 200 "流式 200"; then
  if grep -q 'event: response.completed' "$BODY_FILE"; then
    ok "流式含 response.completed"
  else
    bad "流式缺少 response.completed"
  fi
  if grep -q '\[DONE\]' "$BODY_FILE"; then
    bad "Responses 流不应出现 [DONE]"
  else
    ok "流式无 [DONE]（符合 Responses 规范）"
  fi
fi

request POST /v1/responses "{\"model\":\"$MODEL\",\"input\":\"hi\",\"background\":true}"
if expect_status 400 "background 被明确拒绝"; then
  CODE="$(json_get "$BODY_FILE" 'd["error"].get("code","")')"
  PARAM="$(json_get "$BODY_FILE" 'd["error"].get("param","")')"
  [ "$CODE" = "unsupported_parameter" ] && ok "error.code=unsupported_parameter" || bad "error.code=$CODE"
  [ "$PARAM" = "background" ] && ok "error.param=background" || bad "error.param=$PARAM"
fi

# ============================================================
echo
echo "#11 Anthropic count_tokens 与日志筛选"

request POST /v1/messages/count_tokens "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
if expect_status 200 "count_tokens 200"; then
  TOKENS="$(json_get "$BODY_FILE" 'str(d.get("input_tokens",0))')"
  if [ "${TOKENS:-0}" -gt 0 ] 2>/dev/null; then
    ok "input_tokens=$TOKENS"
  else
    bad "input_tokens=$TOKENS（应大于 0）"
  fi
fi

request GET "/admin/api/logs?limit=5&status=2xx" ""
if expect_status 200 "日志 status=2xx 200"; then
  N="$(json_get "$BODY_FILE" 'str(len(d.get("logs",[])))')"
  if [ "${N:-0}" -gt 0 ] 2>/dev/null; then
    ok "命中 $N 条"
  else
    bad "status=2xx 未命中任何日志"
  fi
fi

request GET "/admin/api/logs?limit=10" ""
if expect_status 200 "日志 200"; then
  ORDERED="$(json_get "$BODY_FILE" '"1" if all(a["time"]>=b["time"] for a,b in zip(d["logs"],d["logs"][1:])) else "0"')"
  [ "$ORDERED" = "1" ] && ok "日志最新在前" || bad "日志顺序不是最新在前"
fi

request GET "/admin/api/usage?days=7" ""
if expect_status 200 "usage 200"; then
  DAYS="$(json_get "$BODY_FILE" 'str(d.get("days"))')"
  [ "$DAYS" = "7" ] && ok "days=7" || bad "days=$DAYS"
fi

# ============================================================
echo
echo "== 结果 =="
echo "  $(c_green PASS=$PASS)  $(c_red FAIL=$FAIL)  $(c_yellow SKIP=$SKIP)"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
