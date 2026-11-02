#!/usr/bin/env bash
#
# 本地三节点 Raft 集群启动脚本（127.0.0.1:7777 / 7778 / 7779）
#
# 用法：
#   ./scripts/cluster.sh start     构建并启动三个节点，等待 leader 选出
#   ./scripts/cluster.sh stop      优雅停止全部节点
#   ./scripts/cluster.sh restart   重启
#   ./scripts/cluster.sh status    查看进程、接口、leader 和节点列表
#   ./scripts/cluster.sh logs [p]  跟踪日志（p 可选：7777/7778/7779）
#   ./scripts/cluster.sh demo      写入一条配置并验证其他节点已同步
#   ./scripts/cluster.sh clean     停止并清空 data/logs/run（慎用）
#
# 环境变量：
#   RAFT_AUTH_TOKEN  管理员 token（三个节点必须一致，默认 dev-raft-token）
#   RAFT_DATA_DIR    数据目录（默认 <repo>/data）
#
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
HOST=127.0.0.1
PORTS=(7777 7778 7779)
TOKEN=${RAFT_AUTH_TOKEN:-dev-raft-token}

DATA_DIR=${RAFT_DATA_DIR:-"$ROOT_DIR/data"}
LOG_DIR="$ROOT_DIR/logs"
PID_DIR="$ROOT_DIR/run"
BIN="$ROOT_DIR/bin/nexus"

SEEDS="$HOST:7777,$HOST:7778,$HOST:7779"
READY_TIMEOUT=${READY_TIMEOUT:-60}   # 秒，等待端口就绪
LEADER_TIMEOUT=${LEADER_TIMEOUT:-60} # 秒，等待 leader 选出

pid_file() { echo "$PID_DIR/node-$1.pid"; }
log_file() { echo "$LOG_DIR/node-$1.log"; }
db_file()  { echo "$DATA_DIR/node-$1.db"; }
base_url() { echo "http://$HOST:$1"; }

pid_alive() {
    local pid=$1
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

node_pid() {
    local f
    f=$(pid_file "$1")
    [ -f "$f" ] && cat "$f" || true
}

port_busy() {
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

api_ok() {
    curl -fsS --max-time 2 -H "Auth: $TOKEN" \
        "$(base_url "$1")/api/v1/whoami" >/dev/null 2>&1
}

leader_port() {
    local port pid
    for port in "${PORTS[@]}"; do
        pid=$(node_pid "$port")
        pid_alive "$pid" || continue
        if grep -q "当选 IP" "$(log_file "$port")" 2>/dev/null; then
            echo "$port"
            return 0
        fi
    done
    return 1
}

build() {
    echo "==> 构建 $BIN"
    mkdir -p "$(dirname "$BIN")"
    (cd "$ROOT_DIR" && go build -o "$BIN" ./cmd)
}

start_one() {
    local port=$1
    local pid
    pid=$(node_pid "$port")

    if pid_alive "$pid"; then
        echo "==> 节点 $port 已在运行（pid ${pid}），跳过"
        return 0
    fi
    if port_busy "$port"; then
        echo "!! 端口 $port 已被其他进程占用：" >&2
        lsof -nP -iTCP:"$port" -sTCP:LISTEN >&2 || true
        return 1
    fi

    echo "==> 启动节点 ${port}（db: $(db_file "$port")）"
    RAFT_NODE_IPS="$SEEDS" \
    RAFT_AUTH_TOKEN="$TOKEN" \
    nohup "$BIN" run \
        --host "$HOST" \
        --port "$port" \
        --db-driver sqlite3 \
        --db-dsn "$(db_file "$port")" \
        >"$(log_file "$port")" 2>&1 &
    echo $! >"$(pid_file "$port")"
}

wait_ready() {
    local port=$1 waited=0
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if api_ok "$port"; then
            echo "    节点 $port 已就绪"
            return 0
        fi
        local pid
        pid=$(node_pid "$port")
        if ! pid_alive "$pid"; then
            echo "!! 节点 $port 启动失败（进程已退出），最后日志：" >&2
            tail -n 30 "$(log_file "$port")" >&2 || true
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "!! 等待节点 $port 就绪超时（${READY_TIMEOUT}s），最后日志：" >&2
    tail -n 30 "$(log_file "$port")" >&2 || true
    return 1
}

wait_leader() {
    local waited=0 port
    while [ "$waited" -lt "$LEADER_TIMEOUT" ]; do
        if port=$(leader_port); then
            echo "==> Leader: $HOST:$port"
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "!! ${LEADER_TIMEOUT}s 内没有节点当选 leader，请查看日志：./scripts/cluster.sh logs" >&2
    return 1
}

cmd_start() {
    mkdir -p "$DATA_DIR" "$LOG_DIR" "$PID_DIR"
    build

    local port
    for port in "${PORTS[@]}"; do
        start_one "$port"
    done
    for port in "${PORTS[@]}"; do
        wait_ready "$port"
    done

    wait_leader
    echo
    cmd_status
}

cmd_stop() {
    local port pid waited rc=0
    for port in "${PORTS[@]}"; do
        pid=$(node_pid "$port")
        if ! pid_alive "$pid"; then
            echo "==> 节点 $port 未运行"
            rm -f "$(pid_file "$port")"
            continue
        fi
        echo "==> 停止节点 ${port}（pid ${pid}）"
        kill -TERM "$pid" 2>/dev/null || true
        waited=0
        while pid_alive "$pid" && [ "$waited" -lt 15 ]; do
            sleep 1
            waited=$((waited + 1))
        done
        if pid_alive "$pid"; then
            echo "    优雅退出超时，强制结束"
            kill -KILL "$pid" 2>/dev/null || true
            rc=1
        fi
        rm -f "$(pid_file "$port")"
    done
    return $rc
}

cmd_status() {
    local port pid state leader=""
    leader=$(leader_port || true)

    printf '%-6s %-8s %-10s %-8s %s\n' PORT PID 接口 LEADER DB
    for port in "${PORTS[@]}"; do
        pid=$(node_pid "$port")
        if pid_alive "$pid"; then
            if api_ok "$port"; then state="正常"; else state="无响应"; fi
        else
            pid="-"
            state="已停止"
        fi
        local is_leader="-"
        [ "$leader" = "$port" ] && is_leader="★"
        printf '%-6s %-8s %-10s %-8s %s\n' \
            "$port" "$pid" "$state" "$is_leader" "$(db_file "$port")"
    done

    if [ -n "$leader" ]; then
        echo
        echo "集群节点（通过 $HOST:$leader 读取）："
        curl -fsS --max-time 3 -H "Auth: $TOKEN" \
            "$(base_url "$leader")/api/v1/nodes" 2>/dev/null || true
        echo
    fi
}

cmd_logs() {
    local target=${1:-}
    if [ -n "$target" ]; then
        exec tail -n 100 -f "$(log_file "$target")"
    fi
    local files=() p
    for p in "${PORTS[@]}"; do
        files+=("$(log_file "$p")")
    done
    exec tail -n 40 -f "${files[@]}"
}

# 等待 follower 应用日志（提交后 apply 是异步的）并读到预期值，最多约 5 秒。
fetch_config() {
    local port=$1 ns=$2 key=$3 expected=$4 waited=0 value
    while [ "$waited" -lt 20 ]; do
        if value=$(curl -fsS --max-time 2 -H "Auth: $TOKEN" \
            "$(base_url "$port")/api/v1/configs/$ns/$key" 2>/dev/null) \
            && [ "${value#*\"value\":\"$expected\"}" != "$value" ]; then
            echo "$value"
            return 0
        fi
        sleep 0.25
        waited=$((waited + 1))
    done
    return 1
}

cmd_demo() {
    local leader
    leader=$(wait_leader) || return 1
    leader=${leader##*:}
    local ns=demo key=hello expected
    expected="raft-$(date +%s)"

    echo "==> 向 leader($leader) 写入配置 $ns/$key = $expected"
    curl -fsS --max-time 10 -X POST \
        -H "Auth: $TOKEN" -H "Content-Type: application/json" \
        -d "{\"namespace\":\"$ns\",\"key\":\"$key\",\"value\":\"$expected\",\"format\":\"text\"}" \
        "$(base_url "$leader")/api/v1/configs"
    echo

    local port value failed=0
    for port in "${PORTS[@]}"; do
        if value=$(fetch_config "$port" "$ns" "$key" "$expected"); then
            echo "    节点 $port 已同步：$value"
        else
            echo "    节点 $port 未同步到 $expected"
            failed=1
        fi
    done
    return $failed
}

cmd_clean() {
    cmd_stop || true
    echo "==> 删除 $DATA_DIR $LOG_DIR $PID_DIR"
    rm -rf "$DATA_DIR" "$LOG_DIR" "$PID_DIR"
}

case "${1:-start}" in
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    restart) cmd_stop || true; cmd_start ;;
    status)  cmd_status ;;
    logs)    shift; cmd_logs "${1:-}" ;;
    demo)    cmd_demo ;;
    clean)   cmd_clean ;;
    *)
        echo "用法: $0 {start|stop|restart|status|logs [port]|demo|clean}" >&2
        exit 1
        ;;
esac
