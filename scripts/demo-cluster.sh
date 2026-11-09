#!/usr/bin/env bash
#
# 演示脚本：一条命令后台拉起 5 个节点的 Raft 集群。
#
#   * 每个节点一个全新的 SQLite 数据库（5 个 db 文件）
#   * 5 个进程的输出写进同一个共享日志文件（每行带 [端口] 前缀）
#   * 每个节点都启动 Web 界面，浏览器直接打开 http://127.0.0.1:<port> 登录即可
#
# 用法：
#   ./scripts/demo-cluster.sh start    构建 → 清空旧 db → 后台启动 5 个节点 → 等 leader
#   ./scripts/demo-cluster.sh stop     优雅停止全部节点
#   ./scripts/demo-cluster.sh status   查看进程 / 接口 / leader
#   ./scripts/demo-cluster.sh logs     跟踪共享日志（一个文件看 5 个节点）
#   ./scripts/demo-cluster.sh clean    停止并删除 demo 目录（含 db 和日志）
#
# 环境变量：
#   DEMO_NODES       节点数量（默认 5）
#   DEMO_BASE_PORT   起始端口（默认 8777，即 8777-8781）
#   DEMO_HOST        监听/通告地址（默认 127.0.0.1）
#   RAFT_AUTH_TOKEN  管理员 token（所有节点一致，默认 dev-raft-token）
#
set -uo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN="$ROOT_DIR/bin/nexus"

DEMO_DIR="$ROOT_DIR/demo"
DATA_DIR="$DEMO_DIR/data"
PID_DIR="$DEMO_DIR/run"
FIFO_DIR="$DEMO_DIR/fifo"
LOG="$DEMO_DIR/demo.log"   # 五个节点共享的日志文件

NODES=${DEMO_NODES:-5}
BASE_PORT=${DEMO_BASE_PORT:-8777}
HOST=${DEMO_HOST:-127.0.0.1}
TOKEN=${RAFT_AUTH_TOKEN:-dev-raft-token}
READY_TIMEOUT=${READY_TIMEOUT:-60}
LEADER_TIMEOUT=${LEADER_TIMEOUT:-60}

PORTS=()
for ((i = 0; i < NODES; i++)); do
    PORTS+=($((BASE_PORT + i)))
done

SEEDS=""
for p in "${PORTS[@]}"; do
    [ -n "$SEEDS" ] && SEEDS+=","
    SEEDS+="$HOST:$p"
done

pid_file()    { echo "$PID_DIR/node-$1.pid"; }
reader_file() { echo "$PID_DIR/reader-$1.pid"; }
db_file()     { echo "$DATA_DIR/node-$1.db"; }
fifo_file()   { echo "$FIFO_DIR/node-$1.fifo"; }
base_url()    { echo "http://$HOST:$1"; }

pid_alive() {
    local pid=$1
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

node_pid() {
    local f
    f=$(pid_file "$1")
    [ -f "$f" ] && cat "$f" || true
}

port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

api_ok() {
    curl -fsS --max-time 2 -H "Auth: $TOKEN" \
        "$(base_url "$1")/api/v1/whoami" >/dev/null 2>&1
}

build() {
    echo "==> 构建 $BIN"
    mkdir -p "$(dirname "$BIN")"
    (cd "$ROOT_DIR" && go build -o "$BIN" ./cmd) || {
        echo "!! 构建失败" >&2
        return 1
    }
}

start_one() {
    local port=$1 fifo pid
    fifo=$(fifo_file "$port")
    rm -f "$fifo"
    mkfifo "$fifo"

    # 读端：给每行加上 [port] 前缀后追加到共享日志。
    # stdin/stdout/stderr 全部重定向，避免后台进程继承调用方的 stdout 导致管道不结束。
    (awk -v p="[$port] " '{ print p $0; fflush() }' \
        </dev/null <"$fifo" >>"$LOG" 2>&1) &
    echo $! >"$(reader_file "$port")"

    RAFT_NODE_IPS="$SEEDS" \
    RAFT_AUTH_TOKEN="$TOKEN" \
    nohup "$BIN" run \
        --host "$HOST" \
        --port "$port" \
        --db-driver sqlite3 \
        --db-dsn "$(db_file "$port")" \
        >"$fifo" 2>&1 </dev/null &
    pid=$!
    echo "$pid" >"$(pid_file "$port")"
    echo "==> 启动节点 ${port}（pid ${pid}，db: $(db_file "$port")）"
}

wait_ready() {
    local port=$1 waited=0 pid
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if api_ok "$port"; then
            return 0
        fi
        pid=$(node_pid "$port")
        if ! pid_alive "$pid"; then
            echo "!! 节点 $port 启动失败（进程已退出），最后日志：" >&2
            grep -h "^\[$port\] " "$LOG" 2>/dev/null | tail -n 20 >&2
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "!! 等待节点 $port 就绪超时（${READY_TIMEOUT}s），最后日志：" >&2
    grep -h "^\[$port\] " "$LOG" 2>/dev/null | tail -n 20 >&2
    return 1
}

# 从共享日志里读最后一次选举成功的端口（形如 "[8778] 选举成功... 当选 IP: ..."）。
leader_port() {
    local line port
    line=$(grep -h '当选 IP' "$LOG" 2>/dev/null | tail -n 1)
    [ -n "$line" ] || return 1
    port=$(printf '%s' "$line" | sed -E -n 's/^\[([0-9]+)\].*/\1/p')
    [ -n "$port" ] || return 1
    pid_alive "$(node_pid "$port")" || return 1
    api_ok "$port" || return 1
    echo "$port"
}

wait_leader() {
    local waited=0 port
    while [ "$waited" -lt "$LEADER_TIMEOUT" ]; do
        if port=$(leader_port); then
            echo "$port"
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

cmd_stop() {
    local port pid waited rc=0
    for port in "${PORTS[@]}"; do
        pid=$(node_pid "$port")
        if pid_alive "$pid"; then
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
        else
            echo "==> 节点 $port 未运行"
        fi
        rm -f "$(pid_file "$port")"

        # 日志前缀过滤器：节点退出后 FIFO 关闭，sed 会自己结束，这里兜底清理。
        local reader
        reader=$(cat "$(reader_file "$port")" 2>/dev/null || true)
        pid_alive "$reader" && kill -TERM "$reader" 2>/dev/null || true
        rm -f "$(reader_file "$port")" "$(fifo_file "$port")"
    done
    return $rc
}

cmd_start() {
    local port

    # 端口预检：端口被别的进程占着就直接报错，避免拉起半个集群。
    for port in "${PORTS[@]}"; do
        if port_busy "$port" && ! pid_alive "$(node_pid "$port")"; then
            echo "!! 端口 $port 已被其他进程占用：" >&2
            lsof -nP -iTCP:"$port" -sTCP:LISTEN >&2 || true
            echo "   可用 DEMO_BASE_PORT=8777 之类换个起始端口。" >&2
            return 1
        fi
    done

    cmd_stop >/dev/null 2>&1 || true

    build || return 1

    echo "==> 新建 $NODES 个数据库并清空旧数据：$DATA_DIR"
    rm -rf "$DATA_DIR" "$FIFO_DIR" "$PID_DIR"
    mkdir -p "$DATA_DIR" "$FIFO_DIR" "$PID_DIR"
    : >"$LOG"

    for port in "${PORTS[@]}"; do
        start_one "$port"
    done
    for port in "${PORTS[@]}"; do
        wait_ready "$port" || { cmd_stop; return 1; }
        echo "    节点 $port 已就绪"
    done

    local leader
    if ! leader=$(wait_leader); then
        echo "!! ${LEADER_TIMEOUT}s 内没有节点当选 leader，日志：$LOG" >&2
        return 1
    fi
    echo "==> Leader: $HOST:$leader"
    echo

    cmd_status
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

    echo
    echo "Web 界面（每个节点一个，登录同一个 token 即可对比数据）："
    for port in "${PORTS[@]}"; do
        echo "    http://$HOST:$port"
    done
    echo "admin token: $TOKEN"
    echo "共享日志:    tail -f $LOG"
}

cmd_logs() {
    [ -f "$LOG" ] || { echo "!! 还没有日志，先运行 $0 start" >&2; return 1; }
    exec tail -n 100 -F "$LOG"
}

cmd_clean() {
    cmd_stop || true
    echo "==> 删除 $DEMO_DIR"
    rm -rf "$DEMO_DIR"
}

case "${1:-start}" in
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    restart) cmd_stop || true; cmd_start ;;
    status)  cmd_status ;;
    logs)    cmd_logs ;;
    clean)   cmd_clean ;;
    *)
        echo "用法: $0 {start|stop|restart|status|logs|clean}" >&2
        exit 1
        ;;
esac
