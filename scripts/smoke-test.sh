#!/usr/bin/env bash
# GoSeek Windows 版离线冒烟测试（在 macOS/Linux 上运行）。
#
# 不需要真实 API key 或中转：脚本在本地启动 mock-provider（假的 OpenAI-compatible
# 流式供应商），再启动被测的 goseek serve，向它提交一条消息，验证完整链路：
#
#   会话创建 → 模型请求（流式 SSE）→ 工具调用（write_file 真实写盘）→
#   观察回填 → 第二次补全 → 文本回复
#
# 用法：
#   scripts/smoke-test.sh                    # 需要本机有 go 工具链（自动构建）
#   GOSEEK_BIN=/path/goseek MOCK_BIN=/path/mock-provider scripts/smoke-test.sh
#
# 环境变量：
#   SERVE_PORT  被测服务端口（默认 18765，刻意避开 goseek 默认的 8765，
#               这样你正在跑的 GoSeek 不会被打扰）
#   MOCK_PORT   mock 供应商端口（默认 19876）
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
SERVE_PORT=${SERVE_PORT:-18765}
MOCK_PORT=${MOCK_PORT:-19876}
BASE_URL="http://127.0.0.1:${SERVE_PORT}"

WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/goseek-smoke.XXXXXX")
mkdir -p "$WORKDIR/workspace" "$WORKDIR/data" "$WORKDIR/config"
SERVER_PID=""
MOCK_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

command -v curl >/dev/null || { echo "需要 curl"; exit 1; }
command -v python3 >/dev/null || { echo "需要 python3"; exit 1; }

echo "==> 准备二进制"
GOSEEK_BIN=${GOSEEK_BIN:-}
MOCK_BIN=${MOCK_BIN:-}
if [ -z "$GOSEEK_BIN" ]; then
  command -v go >/dev/null || { echo "没有 go 工具链：请先用 Makefile 构建并用 GOSEEK_BIN 指定"; exit 1; }
  (cd "$REPO_ROOT/backend" && go build -o "$WORKDIR/goseek" ./cmd/goseek)
  GOSEEK_BIN="$WORKDIR/goseek"
fi
if [ -z "$MOCK_BIN" ]; then
  command -v go >/dev/null || { echo "没有 go 工具链：请用 MOCK_BIN 指定 mock-provider"; exit 1; }
  (cd "$REPO_ROOT/backend" && go build -o "$WORKDIR/mock-provider" ./cmd/mock-provider)
  MOCK_BIN="$WORKDIR/mock-provider"
fi

echo "==> 启动 mock 供应商（127.0.0.1:$MOCK_PORT）"
MOCK_PORT=$MOCK_PORT "$MOCK_BIN" >"$WORKDIR/mock.log" 2>&1 &
MOCK_PID=$!

echo "==> 启动被测服务（127.0.0.1:$SERVE_PORT，数据目录 $WORKDIR/data）"
XDG_DATA_HOME="$WORKDIR/data" XDG_CONFIG_HOME="$WORKDIR/config" \
GOSEEK_API_KEY=smoke-test-not-a-real-key \
GOSEEK_BASE_URL="http://127.0.0.1:$MOCK_PORT" \
GOSEEK_MODEL=mock-model \
GOSEEK_CONTEXT_WINDOW=128000 \
"$GOSEEK_BIN" serve --addr "127.0.0.1:$SERVE_PORT" >"$WORKDIR/serve.log" 2>&1 &
SERVER_PID=$!

wait_http() {
  local url=$1 name=$2
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null "$url"; then return 0; fi
    sleep 0.2
  done
  echo "!! $name 未就绪：$url"
  echo "--- serve.log ---"; tail -20 "$WORKDIR/serve.log" || true
  echo "--- mock.log ---"; tail -10 "$WORKDIR/mock.log" || true
  exit 1
}
wait_http "$BASE_URL/api/v1/sessions" "被测服务"
wait_http "http://127.0.0.1:$MOCK_PORT/chat/completions" "mock 供应商"

echo "==> 创建会话并发送测试消息"
SESSION_ID=$(curl -sf -X POST "$BASE_URL/api/v1/sessions" -H 'Content-Type: application/json' \
  -d "{\"workspace\":\"$WORKDIR/workspace\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
curl -sf -X POST "$BASE_URL/api/v1/sessions/$SESSION_ID/turns" -H 'Content-Type: application/json' \
  -d '{"content":"把测试结果写入 result.txt"}' >/dev/null
echo "    会话 $SESSION_ID"

echo "==> 等待本轮完成（工具调用 + 回复）"
FINISHED=""
for _ in $(seq 1 100); do
  STATE=$(curl -sf "$BASE_URL/api/v1/sessions/$SESSION_ID" | python3 -c '
import json,sys
s=json.load(sys.stdin)
a=[m for m in s["messages"] if m["role"]=="assistant"]
print("DONE" if len(a)>=2 and (a[-1].get("content") or "").strip() else "WAIT")') || STATE=WAIT
  if [ "$STATE" = "DONE" ]; then FINISHED=1; break; fi
  sleep 0.3
done
if [ -z "$FINISHED" ]; then
  echo "!! 超时：本轮没有完成"
  tail -20 "$WORKDIR/serve.log" || true
  exit 1
fi

echo "==> 断言：写盘内容、消息序列、最终回复"
python3 - "$WORKDIR/workspace/result.txt" "$BASE_URL/api/v1/sessions/$SESSION_ID" <<'PY'
import json, sys, urllib.request
path, url = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as f:
    written = f.read()
assert "冒烟测试通过" in written, "result.txt 内容不对: %r" % written
snap = json.load(urllib.request.urlopen(url))
roles = [m["role"] for m in snap["messages"]]
assert roles == ["user", "assistant", "tool", "assistant"], "消息序列不对: %r" % roles
last = [m for m in snap["messages"] if m["role"] == "assistant"][-1]
assert "测试完成" in (last.get("content") or ""), "最终回复不对"
print("    write_file 真实写盘 ✓  观察回填 ✓  文本回复 ✓")
PY

echo ""
echo "PASS: GoSeek 冒烟测试全部通过（127.0.0.1:$SERVE_PORT 链路完整）"
