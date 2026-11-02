#!/usr/bin/env bash
#
# Raft 集群冒烟测试：健康检查、复制一致性、非 Leader 重定向、参数校验、
# follower 追赶、Leader 故障切换、少数派不可写。
#
# 前置条件：./scripts/cluster.sh start 已启动 7777/7778/7779。
# 测试结束后会自动把三个节点恢复为运行状态。
#
set -uo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
HOST=127.0.0.1
PORTS=(7777 7778 7779)
TOKEN=${RAFT_AUTH_TOKEN:-dev-raft-token}
BASE="http://$HOST"
CLUSTER="$ROOT_DIR/scripts/cluster.sh"
NS=selftest

PASS=0
FAIL=0

section() { echo; echo "== $* =="; }
ok()      { echo "  [PASS] $*"; PASS=$((PASS + 1)); }
bad()     { echo "  [FAIL] $*"; FAIL=$((FAIL + 1)); }

pid_of()  { cat "$ROOT_DIR/run/node-$1.pid" 2>/dev/null || true; }
alive()   { local pid; pid=$(pid_of "$1"); [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; }

api_code() {
    curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
        -H "Auth: $TOKEN" "$BASE:$1/api/v1/whoami" 2>/dev/null || true
}

up() { [ "$(api_code "$1")" = "200" ]; }

wait_up() {
    local port=$1 waited=0
    while [ "$waited" -lt 30 ]; do
        up "$port" && return 0
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

wait_down() {
    local port=$1 waited=0
    while [ "$waited" -lt 20 ]; do
        alive "$port" || return 0
        sleep 0.5
        waited=$((waited + 1))
    done
    return 1
}

# POST 一条配置，输出 "<http_code> <leader_header>"。
post_config() {
    local port=$1 ns=$2 key=$3 value=$4 headers code
    headers=$(mktemp)
    code=$(curl -s -D "$headers" -o /dev/null -w '%{http_code}' --max-time 10 \
        -H "Auth: $TOKEN" -H "Content-Type: application/json" \
        -d "{\"namespace\":\"$ns\",\"key\":\"$key\",\"value\":\"$value\",\"format\":\"text\"}" \
        "$BASE:$port/api/v1/configs" 2>/dev/null || true)
    local leader
    leader=$(tr -d '\r' <"$headers" | awk -F': ' 'tolower($1) == "x-raft-leader" {print $2}')
    rm -f "$headers"
    echo "$code ${leader##*:}"
}

# 通过实际写请求探测 leader（200 的是 leader，307 会告诉我们 leader 端口）。
find_leader() {
    local timeout=${1:-30}
    local deadline=$((SECONDS + timeout)) port code info
    while [ "$SECONDS" -lt "$deadline" ]; do
        for port in "${PORTS[@]}"; do
            up "$port" || continue
            read -r code info <<<"$(post_config "$port" "$NS" probe "probe-$SECONDS")"
            if [ "$code" = "200" ]; then
                echo "$port"
                return 0
            fi
            # Leader 刚挂掉时，follower 的 307 可能仍指向旧 Leader；
            # 只有目标端口还活着才认为它是当前 Leader。
            if [ "$code" = "307" ] && [ -n "$info" ]; then
                case " ${PORTS[*]} " in
                    *" $info "*)
                        if up "$info"; then
                            echo "$info"
                            return 0
                        fi
                        ;;
                esac
            fi
        done
        sleep 0.5
    done
    return 1
}

read_code() {
    curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
        -H "Auth: $TOKEN" "$BASE:$1/api/v1/configs/$2/$3" 2>/dev/null || true
}

read_value() {
    curl -sS --max-time 3 -H "Auth: $TOKEN" \
        "$BASE:$1/api/v1/configs/$2/$3" 2>/dev/null | sed -n 's/.*"value":"\([^"]*\)".*/\1/p'
}

# 等待节点读到预期值（apply 是异步的）。
wait_value() {
    local port=$1 ns=$2 key=$3 expected=$4 timeout=${5:-15} waited=0
    while [ "$waited" -lt "$timeout" ]; do
        [ "$(read_value "$port" "$ns" "$key")" = "$expected" ] && return 0
        sleep 0.5
        waited=$((waited + 1))
    done
    return 1
}

restart_all() {
    echo
    echo "== 恢复集群 =="
    RAFT_AUTH_TOKEN="$TOKEN" "$CLUSTER" start >/dev/null 2>&1 || true
    "$CLUSTER" status || true
}
trap restart_all EXIT

stop_node() {
    local pid
    pid=$(pid_of "$1")
    [ -n "$pid" ] || return 0
    kill -TERM "$pid" 2>/dev/null || true
    wait_down "$1" || kill -KILL "$pid" 2>/dev/null || true
    wait_down "$1"
}

other_ports() {
    local p
    for p in "${PORTS[@]}"; do
        [ "$p" = "$1" ] || echo "$p"
    done
}

echo "Raft 集群冒烟测试 (token: $TOKEN)"

# ---------------------------------------------------------------- 1. 健康检查
section "1. 三节点健康检查"
for p in "${PORTS[@]}"; do
    if wait_up "$p"; then ok "节点 $p /api/v1/whoami 正常"; else bad "节点 $p 不可用"; fi
done

# -------------------------------------------------------------- 2. 复制一致性
section "2. 写入复制一致性"
LEADER=$(find_leader 30)
if [ -n "${LEADER:-}" ]; then ok "当前 Leader: $LEADER"; else bad "30s 内未找到 Leader"; fi

VALUE="replicated-$(date +%s)"
code=$(post_config "$LEADER" "$NS" replica "$VALUE")
case "$code" in
    200*) ok "向 Leader $LEADER 写入成功" ;;
    *)    bad "向 Leader $LEADER 写入失败：$code" ;;
esac
for p in "${PORTS[@]}"; do
    if wait_value "$p" "$NS" replica "$VALUE"; then
        ok "节点 $p 已同步到 $VALUE"
    else
        bad "节点 $p 未同步到 $VALUE"
    fi
done

# -------------------------------------------------------- 3. 非 Leader 重定向
section "3. 非 Leader 写请求返回 307"
FOLLOWER=$(other_ports "$LEADER" | head -1)
code=$(post_config "$FOLLOWER" "$NS" redirect "should-redirect")
case "$code" in
    "307 $LEADER") ok "节点 $FOLLOWER 返回 307，指向 Leader $LEADER" ;;
    *)             bad "节点 $FOLLOWER 期望 307 指向 ${LEADER}，实际：$code" ;;
esac

# ------------------------------------------------------------ 4. 写前参数校验
section "4. 非法格式在进入日志前被拒绝"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Auth: $TOKEN" -H "Content-Type: application/json" \
    -d '{"namespace":"'"$NS"'","key":"bad","value":"{not-json","format":"json"}' \
    "$BASE:$LEADER/api/v1/configs" 2>/dev/null || true)
if [ "$code" = "400" ]; then ok "非法 JSON 返回 400，未写入日志"; else bad "期望 400，实际 $code"; fi

# -------------------------------------------------------- 5. follower 掉队追赶
section "5. follower 停机期间的写入，重启后自动追平"
stop_node "$FOLLOWER" && ok "已停止 follower $FOLLOWER"
VALUE2="catchup-$(date +%s)"
code=$(post_config "$LEADER" "$NS" catchup "$VALUE2")
case "$code" in
    200*) ok "2/3 多数派仍可写入（Leader ${LEADER}）" ;;
    *)    bad "多数派写入失败：$code" ;;
esac
if wait_value "$LEADER" "$NS" catchup "$VALUE2"; then
    ok "Leader 已应用新值"
else
    bad "Leader 未应用新值"
fi
RAFT_AUTH_TOKEN="$TOKEN" "$CLUSTER" start >/dev/null 2>&1
if wait_value "$FOLLOWER" "$NS" catchup "$VALUE2" 20; then
    ok "follower $FOLLOWER 重启后追平到 $VALUE2"
else
    bad "follower $FOLLOWER 重启后未追平"
fi

# ------------------------------------------------------------ 6. Leader 故障切换
section "6. Leader 故障后重新选举，旧 Leader 恢复后追平"
OLD_LEADER=$LEADER
stop_node "$OLD_LEADER" && ok "已停止 Leader $OLD_LEADER"
NEW_LEADER=$(find_leader 40)
if [ -n "${NEW_LEADER:-}" ] && [ "$NEW_LEADER" != "$OLD_LEADER" ]; then
    ok "新 Leader 选出：$NEW_LEADER"
else
    bad "未选出新 Leader"
fi
VALUE3="failover-$(date +%s)"
if [ -n "${NEW_LEADER:-}" ]; then
    code=$(post_config "$NEW_LEADER" "$NS" failover "$VALUE3")
    case "$code" in
        200*) ok "向新 Leader $NEW_LEADER 写入成功" ;;
        *)    bad "新 Leader 写入失败：$code" ;;
    esac
fi
RAFT_AUTH_TOKEN="$TOKEN" "$CLUSTER" start >/dev/null 2>&1
for p in "${PORTS[@]}"; do
    if wait_value "$p" "$NS" failover "$VALUE3" 20; then
        ok "节点 $p 最终同步到 $VALUE3"
    else
        bad "节点 $p 未同步到 $VALUE3"
    fi
done

# -------------------------------------------------------- 7. 少数派不可写入
section "7. 失去多数派时拒绝写入"
LEADER=$(find_leader 30)
if [ -n "${LEADER:-}" ]; then
    VICTIMS=$(other_ports "$LEADER")
    for p in $VICTIMS; do stop_node "$p"; done
    ok "停掉两个 follower，只剩 Leader $LEADER"
    code=$(post_config "$LEADER" "$NS" minority "must-fail")
    case "$code" in
        200*) bad "只有 1/3 节点时写入竟然成功（${code}）" ;;
        *)    ok "只有 1/3 节点时写入被拒绝（HTTP ${code}）" ;;
    esac
else
    bad "未找到 Leader，跳过"
fi

# ------------------------------------------------------------------- 结果
section "结果"
echo "  通过 $PASS 项，失败 $FAIL 项"
[ "$FAIL" -eq 0 ] && echo "  全部通过 ✅" || echo "  存在失败项 ❌"
exit $((FAIL > 0))
