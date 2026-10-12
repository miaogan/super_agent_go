#!/usr/bin/env bash
# 端到端冒烟测试：从登录一路打到 SSE 流式，验证真实 HTTP 链路。
#
# 用法：
#   scripts/smoke.sh                 # 默认打 127.0.0.1:8000
#   BASE_URL=http://gw:8000 scripts/smoke.sh
#   scripts/smoke.sh -v              # 打印每个请求的完整响应
#
# 退出码：0 全部通过；1 有用例失败。
#
# ★ 设计原则：
#   不依赖任何测试框架，只用 curl + grep。
#   这样 CI 里只需要 bash + curl，不引入额外依赖，也不会因为
#   框架版本变化让冒烟测试变得难维护。
set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8000}"
USERNAME="${AEGIS_SMOKE_USER:-admin}"
PASSWORD="${AEGIS_SMOKE_PASS:-Aegis@2026}"
VERBOSE=0
[ "${1:-}" = "-v" ] && VERBOSE=1

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
declare -a FAILURES=()

c_ok=$'\033[0;32m'; c_no=$'\033[0;31m'; c_dim=$'\033[0;90m'
c_hi=$'\033[0;36m'; c_bold=$'\033[1m'; c_off=$'\033[0m'

step()  { printf '\n%s── %s%s\n' "$c_hi" "$*" "$c_off"; }
ok()    { PASS=$((PASS+1)); printf '  %s✓%s %s\n' "$c_ok" "$c_off" "$*"; }
bad()   { FAIL=$((FAIL+1)); FAILURES+=("$*"); printf '  %s✗%s %s\n' "$c_no" "$c_off" "$*"; }

# assert_eq <实际> <期望> <说明>
assert_eq() {
  if [ "$1" = "$2" ]; then ok "$3"; else bad "$3（期望 $2，实际 $1）"; fi
}

# assert_contains <文本> <子串> <说明>
assert_contains() {
  if printf '%s' "$1" | grep -qF -- "$2"; then ok "$3"
  else bad "$3（响应中未找到 '$2'）"; fi
}

# ---- HTTP 封装 ----
# 约定：把响应体写进 $BODY，全局变量 HTTP_CODE 存状态码。
BODY=""
HTTP_CODE=""

# req <METHOD> <PATH> [DATA] [TOKEN] [CONTENT_TYPE]
req() {
  local method="$1" path="$2" data="${3:-}" token="${4:-}" ctype="${5:-application/json}"
  local args=(-sS -o "$TMP/body" -w '%{http_code}' -X "$method" --max-time 20)
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  if [ -n "$data" ]; then
    args+=(-H "Content-Type: $ctype" --data-binary "$data")
  fi
  HTTP_CODE="$(curl "${args[@]}" "$BASE_URL$path" 2>"$TMP/err")" || HTTP_CODE="000"
  BODY="$(cat "$TMP/body" 2>/dev/null)"
  if [ "$VERBOSE" = 1 ]; then
    printf '%s    → %s %s [%s]%s\n' "$c_dim" "$method" "$path" "$HTTP_CODE" "$c_off"
    printf '%s    %s%s\n' "$c_dim" "$(printf '%s' "$BODY" | head -c 400)" "$c_off"
  fi
}

# upload <PATH> <文件> <TOKEN> —— multipart 上传
upload() {
  HTTP_CODE="$(curl -sS -o "$TMP/body" -w '%{http_code}' --max-time 30 \
    -X POST "$BASE_URL$1" -H "Authorization: Bearer $3" \
    -F "file=@$2" 2>"$TMP/err")" || HTTP_CODE="000"
  BODY="$(cat "$TMP/body" 2>/dev/null)"
  if [ "$VERBOSE" = 1 ]; then
    printf '%s    → POST %s [%s]%s\n' "$c_dim" "$1" "$HTTP_CODE" "$c_off"
    printf '%s    %s%s\n' "$c_dim" "$(printf '%s' "$BODY" | head -c 400)" "$c_off"
  fi
}

# jsonq <JSON> <key>  —— 取字段（够用即可，不引 jq）
#
# 兼容两种响应形状：
#   业务接口统一包一层 {"data": {...}}，健康检查则是裸 JSON。
# 带点的路径（a.b）会在 data. 前缀下再试一次，
# 否则 message.usage 这种写法会因为外层包了 data 而取不到。
jsonq() {
  printf '%s' "$1" | python3 -c '
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)

def dig(obj, path):
    for k in path.split("."):
        if isinstance(obj, dict):
            obj = obj.get(k)
        elif isinstance(obj, list) and k.lstrip("-").isdigit():
            i = int(k)
            obj = obj[i] if -len(obj) <= i < len(obj) else None
        else:
            return None
    return obj

def render(v):
    if v is None:
        return ""
    if isinstance(v, bool):
        return "True" if v else "False"
    if isinstance(v, (dict, list)):
        return json.dumps(v, ensure_ascii=False)
    return str(v)

key = sys.argv[1]
val = dig(d, key)
if val is None:
    val = dig(d, "data." + key)
if val is None and "." not in key:
    val = dig(d, key.split(".")[0])
print(render(val))
' "$2" 2>/dev/null
}

printf '%s' "$c_bold"
cat <<'BANNER'
   ___          ___
  /   |  ____  /   |  ____
 / /| | / __ \/ /| | / __ \
/ ___ |/ /_/ / ___ |/ /_/ /
_/  |_|\____/_/  |_|\____/
BANNER
printf '%s\n' "$c_off"
printf '目标: %s\n' "$BASE_URL"

# ============ 1. 健康检查 ============
step "1. 健康检查"

req GET /healthz
assert_eq "$HTTP_CODE" "200" "GET /healthz 返回 200"
assert_eq "$(jsonq "$BODY" healthy)" "True" "healthy=true"
assert_eq "$(jsonq "$BODY" service)" "gateway" "service=gateway"

req GET /livez
assert_eq "$HTTP_CODE" "200" "GET /livez 返回 200"

req GET /readyz
assert_eq "$HTTP_CODE" "200" "GET /readyz 返回 200"

req GET /healthz/full
assert_eq "$HTTP_CODE" "200" "GET /healthz/full 聚合探活返回 200"
FULL="$(jsonq "$BODY" downstream)"
for dep in auth_service user_service rag_service; do
  assert_eq "$(jsonq "$FULL" "$dep")" "ok" "下游 $dep 健康"
done

# ============ 2. 鉴权边界 ============
step "2. 鉴权边界"

TOKEN=""
req GET /api/v1/agents
assert_eq "$HTTP_CODE" "401" "无令牌访问业务接口返回 401"

req GET /api/v1/agents "not-a-jwt"
assert_eq "$HTTP_CODE" "401" "伪造令牌返回 401"

req POST /api/v1/auth/login '{"username":"'"$USERNAME"'","password":"wrong-password"}'
assert_eq "$HTTP_CODE" "401" "错误密码返回 401"

req POST /api/v1/auth/login '{"username":"'"$USERNAME"'"}'
assert_eq "$HTTP_CODE" "400" "缺密码返回 400"

# ============ 3. 登录 ============
step "3. 登录与身份"

req POST /api/v1/auth/login '{"username":"'"$USERNAME"'","password":"'"$PASSWORD"'"}'
assert_eq "$HTTP_CODE" "200" "登录成功"
TOKEN="$(jsonq "$BODY" access_token)"
[ -n "$TOKEN" ] && ok "拿到 access_token（${#TOKEN} 字符）" || bad "未拿到 access_token"

if [ -z "$TOKEN" ]; then
  printf '\n%s登录失败，后续用例无法执行。响应：%s%s\n' "$c_no" "$BODY" "$c_off"
  exit 1
fi

req GET /api/v1/auth/me "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "GET /auth/me 返回 200"
assert_eq "$(jsonq "$BODY" uid)" "1001" "uid=1001"
assert_eq "$(jsonq "$BODY" tenant)" "default" "tenant=default"
assert_contains "$BODY" "admin" "scopes 含 admin"

# 令牌刷新：用登录响应里的 refresh_token 换新 access_token
req POST /api/v1/auth/login '{"username":"'"$USERNAME"'","password":"'"$PASSWORD"'"}'
RT="$(jsonq "$BODY" refresh_token)"
[ -n "$RT" ] && ok "拿到 refresh_token" || bad "未拿到 refresh_token"

req POST /api/v1/auth/refresh '{"refresh_token":"'"$RT"'"}'
assert_eq "$HTTP_CODE" "200" "刷新令牌成功"
NEW_TOKEN="$(jsonq "$BODY" access_token)"
if [ -n "$NEW_TOKEN" ]; then
  ok "刷新后拿到新 access_token"
  TOKEN="$NEW_TOKEN"
else
  bad "刷新后未拿到新 token，沿用旧令牌"
fi

# 旧 refresh_token 应已失效（一次性）
req POST /api/v1/auth/refresh '{"refresh_token":"'"$RT"'"}'
if [ "$HTTP_CODE" != "200" ]; then
  ok "旧 refresh_token 已失效（一次性，HTTP $HTTP_CODE）"
else
  bad "旧 refresh_token 仍可重复使用，存在重放风险"
fi

# ============ 4. Agent CRUD ============
step "4. Agent 增删改查"

req POST /api/v1/agents '{"name":"冒烟测试助手","description":"smoke","system_prompt":"你是冒烟测试助手，请用一句话回答。","model":"mock-chat","temperature":0.7,"max_tokens":512,"rag_enabled":true,"rag_top_k":3}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "创建 Agent"
AGENT_ID="$(jsonq "$BODY" agent_id)"
if [ -n "$AGENT_ID" ]; then ok "agent_id=$AGENT_ID"; else bad "未拿到 agent_id：$BODY"; exit 1; fi

req GET "/api/v1/agents/$AGENT_ID" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "查询 Agent 详情"
assert_eq "$(jsonq "$BODY" name)" "冒烟测试助手" "名称回显正确"

req PUT "/api/v1/agents/$AGENT_ID" '{"agent":{"description":"已更新","temperature":0.3},"update_mask":["description","temperature"]}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "更新 Agent"
req GET "/api/v1/agents/$AGENT_ID" "" "$TOKEN"
assert_eq "$(jsonq "$BODY" description)" "已更新" "更新已生效"

req GET "/api/v1/agents?page=1&page_size=10" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "分页列出 Agent"

# 越权防线：body 里的 agent_id 不能盖过路径参数
req PUT "/api/v1/agents/$AGENT_ID" '{"agent":{"agent_id":999999,"description":"越权尝试"},"update_mask":["description"]}' "$TOKEN"
req GET "/api/v1/agents/$AGENT_ID" "" "$TOKEN"
if [ "$(jsonq "$BODY" agent_id)" = "$AGENT_ID" ]; then
  ok "body 中的 agent_id 未覆盖路径参数（越权防线生效）"
else
  bad "agent_id 被 body 覆盖：$(jsonq "$BODY" agent_id)"
fi

# ============ 5. 会话 ============
step "5. 会话"

req POST /api/v1/sessions '{"agent_id":'"$AGENT_ID"',"title":"冒烟会话"}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "创建会话"
SESSION_ID="$(jsonq "$BODY" session_id)"
[ -n "$SESSION_ID" ] && ok "session_id=$SESSION_ID" || bad "未拿到 session_id：$BODY"

req GET "/api/v1/sessions/$SESSION_ID" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "查询会话详情"

req GET "/api/v1/sessions?page=1&page_size=10" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "列出会话"

# ============ 6. 知识库 ============
step "6. 知识库与文档"

req POST /api/v1/knowledge-bases '{"name":"冒烟知识库","description":"smoke","chunk_size":256,"chunk_overlap":32}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "创建知识库"
KB_ID="$(jsonq "$BODY" kb_id)"
[ -n "$KB_ID" ] && ok "kb_id=$KB_ID" || bad "未拿到 kb_id：$BODY"

req GET "/api/v1/knowledge-bases/$KB_ID" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "查询知识库详情"

req GET "/api/v1/knowledge-bases?page=1&page_size=10" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "列出知识库"

# multipart 上传（kb_id 走路径参数，不是 query）
printf 'Aegis 冒烟测试文档。\n\n本文件用于验证 RAG 检索链路是否通畅。\n关键指标：检索命中率与降级行为。\n' > "$TMP/doc.md"
upload "/api/v1/knowledge-bases/$KB_ID/documents" "$TMP/doc.md" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "multipart 上传文档"
DOC_ID="$(jsonq "$BODY" doc_id)"
if [ -n "$DOC_ID" ]; then ok "doc_id=$DOC_ID"; else bad "未拿到 doc_id：$BODY"; fi

# 重复上传应命中去重（按内容哈希）
upload "/api/v1/knowledge-bases/$KB_ID/documents" "$TMP/doc.md" "$TOKEN"
assert_eq "$(jsonq "$BODY" duplicated)" "True" "重复上传命中去重"

# JSON 方式上传（file_name 必填 —— rag_service 要按名推断类型与去重）
req POST "/api/v1/knowledge-bases/$KB_ID/documents" '{"file_name":"inline.txt","file_type":"txt","content":"JSON 上传的正文：Aegis 网关支持两种上传模式。"}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "JSON 方式上传文档"
assert_eq "$(jsonq "$BODY" file_name)" "inline.txt" "file_name 回显正确"

# 缺 file_name 的 JSON 上传应被拒
req POST "/api/v1/knowledge-bases/$KB_ID/documents" '{"content":"缺文件名的请求"}' "$TOKEN"
if [ "$HTTP_CODE" != "200" ]; then ok "缺 file_name 被拒（HTTP $HTTP_CODE）"; else bad "缺 file_name 竟被接受"; fi

req GET "/api/v1/knowledge-bases/$KB_ID/documents?page=1&page_size=20" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "列出文档"

if [ -n "$DOC_ID" ]; then
  req GET "/api/v1/knowledge-bases/documents/$DOC_ID/chunks?page=1&page_size=10" "" "$TOKEN"
  assert_eq "$HTTP_CODE" "200" "列出文档切片"
fi

# ============ 7. 检索 ============
step "7. 检索"

req POST /api/v1/rag/retrieve '{"query":"检索链路","kb_ids":['"$KB_ID"'],"top_k":3}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "向量检索返回 200"
assert_contains "$BODY" "hits" "响应含 hits 字段"

req POST /api/v1/rag/debug/retrieve '{"query":"检索链路","kb_ids":['"$KB_ID"'],"top_k":3}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "调试检索返回 200"
assert_contains "$BODY" "milvus_filter" "调试响应含 milvus_filter"

# 重排阶段必须真实执行过。
#
# ★ 这条断言的价值在于「防静默失效」：
#   若 mock 重排的词袋余弦达不到生产阈值 0.3，
#   rerank 会把全部候选判为不相关 → hits 为空 → 看起来测试通过，
#   实际检索链路已经废了。mock 模式阈值下调正是为此
#   （见 retrieve.Engine.rerankThreshold）。
assert_contains "$BODY" '"stage":"rerank"' "调试响应含 rerank 阶段（重排已真实执行）"
assert_contains "$BODY" 'RAG' "检索命中内容（证明重排后仍有结果，未被阈值全拦）"

req POST /api/v1/rag/retrieve '{"kb_ids":['"$KB_ID"']}' "$TOKEN"
assert_eq "$HTTP_CODE" "400" "缺 query 返回 400"

# ============ 8. 同步对话 ============
step "8. 同步对话"

req POST "/api/v1/agents/$AGENT_ID/chat" '{"content":"你好，请自我介绍","session_id":'"$SESSION_ID"'}' "$TOKEN"
assert_eq "$HTTP_CODE" "200" "同步对话返回 200"
assert_contains "$BODY" "content" "响应含 content"
USAGE_TOTAL="$(jsonq "$BODY" data.message.usage.total_tokens)"
[ -n "$USAGE_TOTAL" ] && ok "usage.total_tokens=$USAGE_TOTAL" || bad "响应缺 usage 统计"

# 空内容应被拒（400，不是 500 —— 参数问题不该报"服务内部错误"）
req POST "/api/v1/agents/$AGENT_ID/chat" '{"content":"   "}' "$TOKEN"
assert_eq "$HTTP_CODE" "400" "空内容返回 400"

# 不存在的 Agent → 404，且业务码要能穿过 Kitex 传回网关
req POST "/api/v1/agents/99999999/chat" '{"content":"hi"}' "$TOKEN"
assert_eq "$HTTP_CODE" "404" "不存在的 Agent 返回 404"
assert_eq "$(jsonq "$BODY" error.code)" "40401" "业务码正确传回（未被误判为 500）"

# ============ 9. SSE 流式 ============
step "9. SSE 流式对话"

# 令牌走 query —— EventSource 无法设置请求头
SSE_OUT="$TMP/sse.txt"
SSE_CODE="$(curl -sS -N -o "$SSE_OUT" -w '%{http_code}' --max-time 30 \
  -G "$BASE_URL/api/v1/agents/$AGENT_ID/chat/stream" \
  --data-urlencode "access_token=$TOKEN" \
  --data-urlencode "content=流式测试" \
  --data-urlencode "session_id=$SESSION_ID" 2>"$TMP/err")"
SSE_BODY="$(cat "$SSE_OUT" 2>/dev/null)"

assert_eq "$SSE_CODE" "200" "SSE 连接返回 200"

# 响应头由 -D 抓取
curl -sS -N -D "$TMP/hdr" -o /dev/null --max-time 30 \
  -G "$BASE_URL/api/v1/agents/$AGENT_ID/chat/stream" \
  --data-urlencode "access_token=$TOKEN" \
  --data-urlencode "content=流式测试2" >/dev/null 2>&1
HDR="$(cat "$TMP/hdr" 2>/dev/null)"
assert_contains "$HDR" "text/event-stream" "Content-Type 为 text/event-stream"
assert_contains "$HDR" "X-Accel-Buffering: no" "已禁用 nginx 缓冲"
assert_contains "$HDR" "Cache-Control: no-cache" "已禁用缓存"

# 事件序列：session → retrieval → delta* → done
assert_contains "$SSE_BODY" "event: session"    "收到 session 事件"
assert_contains "$SSE_BODY" "event: retrieval" "收到 retrieval 事件"
assert_contains "$SSE_BODY" "event: delta"     "收到 delta 事件"
assert_contains "$SSE_BODY" "event: done"      "收到 done 事件"
assert_contains "$SSE_BODY" "retry: 3000"     "首帧带重连间隔"

# 顺序校验
S_SEQ="$(printf '%s' "$SSE_BODY" | grep -o '^event: [a-z_]*' | sed 's/event: //' | head -20 | tr '\n' ' ')"
printf '  %s事件序列: %s%s\n' "$c_dim" "$S_SEQ" "$c_off"
case "$S_SEQ" in
  "session retrieval "*"done "*) ok "事件顺序正确（session → retrieval → … → done）" ;;
  *) bad "事件顺序异常：$S_SEQ" ;;
esac

# SSE 令牌校验：无令牌应 401
SSE_NOAUTH="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
  -G "$BASE_URL/api/v1/agents/$AGENT_ID/chat/stream" \
  --data-urlencode "content=hi" 2>/dev/null)"
assert_eq "$SSE_NOAUTH" "401" "SSE 无令牌返回 401"

# ============ 10. 越权与清理 ============
step "10. 越权防线与清理"

# 伪造 tenant/owner 参数不能越权 —— tenant 恒取令牌身份
req GET "/api/v1/agents?tenant=other&owner_uid=999" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "带伪造 tenant 参数仍返回 200（参数被忽略）"

# 分页边界不应 500
req GET "/api/v1/agents?page=-1&page_size=99999" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "非法分页参数被夹紧而非报错"

if [ -n "$DOC_ID" ]; then
  req DELETE "/api/v1/knowledge-bases/documents/$DOC_ID" "" "$TOKEN"
  assert_eq "$HTTP_CODE" "200" "删除文档"
fi
req DELETE "/api/v1/agents/$AGENT_ID" "" "$TOKEN"
assert_eq "$HTTP_CODE" "200" "删除 Agent"

req GET "/api/v1/agents/$AGENT_ID" "" "$TOKEN"
assert_eq "$HTTP_CODE" "404" "已删除的 Agent 返回 404"

# 资源不存在要报 404，不能是 500 —— 前端据此区分"重试"还是"别试了"
req GET "/api/v1/knowledge-bases/99999999" "" "$TOKEN"
assert_eq "$HTTP_CODE" "404" "不存在的知识库返回 404"
assert_eq "$(jsonq "$BODY" error.code)" "40420" "知识库专属错误码透传"

if [ -n "$KB_ID" ]; then
  req DELETE "/api/v1/knowledge-bases/$KB_ID" "" "$TOKEN"
  assert_eq "$HTTP_CODE" "200" "删除知识库"
fi

# ============ 汇总 ============
printf '\n%s%s%s\n' "$c_bold" "────────────────────────────────" "$c_off"
printf '  通过 %s%d%s   失败 %s%d%s\n' "$c_ok" "$PASS" "$c_off" \
  "$([ "$FAIL" -gt 0 ] && echo "$c_no" || echo "$c_ok")" "$FAIL" "$c_off"

if [ "$FAIL" -gt 0 ]; then
  printf '\n%s失败用例：%s\n' "$c_no" "$c_off"
  for f in "${FAILURES[@]}"; do printf '  - %s\n' "$f"; done
  exit 1
fi
printf '%s全部冒烟用例通过。%s\n' "$c_ok" "$c_off"
exit 0