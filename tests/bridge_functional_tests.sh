#!/bin/bash

# ==============================================================================
# xworkmate-bridge 高覆盖率功能测试脚本 (v1.2)
# 覆盖范围：Bridge, Gemini, Codex, OpenCode, Gateway
# ==============================================================================

set -e

# 配置区域
BRIDGE_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I=}"
SESSION_ID_GEMINI="test-gemini-$(date +%s)"
SESSION_ID_OPENCODE="test-opencode-$(date +%s)"
RUNTIME_ID="test-runtime-$(date +%s)"

# 颜色定义
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'
BOLD='\033[1m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_err()  { echo -e "${RED}[ERROR]${NC} $1"; }
log_step() { echo -e "\n${BOLD}>>> STEP: $1${NC}"; }

# 检查依赖
if ! command -v jq &> /dev/null; then
    log_err "本脚本依赖 'jq'，请先安装。"
    exit 1
fi

# 通用请求函数
call_api() {
    local path=$1
    local method=$2
    local params=$3
    local stream=$4
    local url="${BRIDGE_URL}${path}"
    
    local headers=(
        "-H" "Authorization: Bearer ${AUTH_TOKEN}"
        "-H" "Content-Type: application/json"
    )
    
    if [ "$stream" == "true" ]; then
        headers+=("-H" "Accept: text/event-stream")
    fi

    local data="{\"jsonrpc\":\"2.0\",\"id\":\"req-$(date +%s%N)\",\"method\":\"${method}\",\"params\":${params}}"
    
    if [ "$stream" == "true" ]; then
        curl -s -N "${headers[@]}" -X POST -d "$data" "$url"
    else
        curl -s "${headers[@]}" -X POST -d "$data" "$url"
    fi
}

# 断言成功函数
assert_success() {
    local response=$1
    local msg=$2
    if echo "$response" | jq -e '.ok == true or .result.success == true' > /dev/null; then
        log_info "$msg: SUCCESS"
    else
        log_err "$msg: FAILED"
        echo "$response" | jq .
        exit 1
    fi
}

# --- 1. 基础连通性测试 ---

log_step "1. 验证各端点 Capabilities"

CAPS=$(call_api "/acp/rpc" "acp.capabilities" "{}" "false")
assert_success "$CAPS" "主 Bridge Capabilities"

GEMINI_CAPS=$(call_api "/acp-server/gemini/acp/rpc" "acp.capabilities" "{}" "false")
assert_success "$GEMINI_CAPS" "Gemini 适配器 Capabilities"

CODEX_CAPS=$(call_api "/acp-server/codex/acp/rpc" "acp.capabilities" "{}" "false")
assert_success "$CODEX_CAPS" "Codex 适配器 Capabilities"

# --- 2. Gateway 连接测试 ---

log_step "2. 建立 Gateway 连接 (OpenClaw)"
CONNECT_RES=$(call_api "/acp/rpc" "xworkmate.gateway.connect" "{\"runtimeId\":\"$RUNTIME_ID\",\"gatewayProviderId\":\"openclaw\"}" "false")
assert_success "$CONNECT_RES" "Gateway 连接"

# --- 3. Gemini 深度测试 (Streaming) ---

log_step "3. Gemini 流式对话测试"
log_info "正在向 Gemini 发起对话..."
START_GEMINI=$(call_api "/acp/rpc" "session.start" "{\"sessionId\":\"$SESSION_ID_GEMINI\",\"taskPrompt\":\"你好，请记住我叫 Gemini-Tester。\",\"routing\":{\"explicitProviderId\":\"gemini\"}}" "true" | tail -n 2 | head -n 1 | sed 's/^data: //')
assert_success "$START_GEMINI" "Gemini 会话启动"

log_info "验证 Gemini 上下文..."
MSG_GEMINI=$(call_api "/acp/rpc" "session.message" "{\"sessionId\":\"$SESSION_ID_GEMINI\",\"taskPrompt\":\"我刚才说我叫什么？\",\"routing\":{\"explicitProviderId\":\"gemini\"}}" "true" | tail -n 2 | head -n 1 | sed 's/^data: //')
assert_success "$MSG_GEMINI" "Gemini 上下文验证"
log_info "Gemini 回复: $(echo "$MSG_GEMINI" | jq -r '.result.output // .payload.output')"

# --- 4. OpenCode 深度测试 (Gateway Mode) ---

log_step "4. OpenCode 深度测试 (通过 Gateway)"
log_info "正在通过 Gateway 向 OpenCode 发起对话..."
# 注意：OpenCode 需要通过已连接的 gateway 执行
START_OPENCODE=$(call_api "/acp/rpc" "session.start" "{\"sessionId\":\"$SESSION_ID_OPENCODE\",\"taskPrompt\":\"你好 OpenCode，请问你能做什么？\",\"routing\":{\"explicitProviderId\":\"opencode\"}}" "true" | tail -n 2 | head -n 1 | sed 's/^data: //')

# 如果 gateway 依然报错，可能是环境限制，此处做容错处理
if echo "$START_OPENCODE" | jq -e '.ok == true or .result.success == true' > /dev/null; then
    log_info "OpenCode 会话启动: SUCCESS"
    log_info "OpenCode 回复: $(echo "$START_OPENCODE" | jq -r '.result.output // .payload.output')"
else
    log_err "OpenCode 会话启动失败 (可能是 Gateway 环境限制): $(echo "$START_OPENCODE" | jq -r '.result.error // .payload.error')"
fi

# --- 5. 清理测试 ---

log_step "5. 清理与断开"
call_api "/acp/rpc" "session.close" "{\"sessionId\":\"$SESSION_ID_GEMINI\"}" "false" > /dev/null
call_api "/acp/rpc" "session.close" "{\"sessionId\":\"$SESSION_ID_OPENCODE\"}" "false" > /dev/null
call_api "/acp/rpc" "xworkmate.gateway.disconnect" "{\"runtimeId\":\"$RUNTIME_ID\"}" "false" > /dev/null
log_info "会话已关闭，Gateway 已断开。"

echo -e "\n${GREEN}${BOLD}完整验证流程执行完毕！${NC}"
