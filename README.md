# Raft

该 package 用以实现节点间数据库日志同步：所有节点复制同一份日志，按相同顺序
执行（apply）到状态机（配置表），最终达成一致。

## 写入链路

配置中心里所有会改变状态的操作都作为一条命令进入 Raft 日志，在各节点按相同
顺序执行（apply）到状态机（SQLite）：配置、命名空间、角色、节点成员。

```
客户端写入 (configs / namespaces / roles / nodes)
        │
        ▼
Leader 校验（如 JSON/YAML 格式），非法请求在进入日志前就被拒绝
        │
        ▼
封装成 Command{OpType, Value} → LogEntry{Term, Comm}
        │
        ▼
追加到 Leader 日志并持久化（raft_log_entries 表）
        │
        ▼
复制给多数节点（AppendEntries）
        │
   多数确认，提交（commit）
        │
        ▼
每个节点按序 apply 到状态机：Store.ApplyCommand
        → configurations / namespaces / roles / node_configs 表
```

- 非 Leader 收到写请求会返回 `307 Temporary Redirect`，`Location` 指向当前
  Leader；客户端可据此重试。
- 日志里保存的是命令本身；业务数据保存在各自的表里，两者通过
  `LogIndex`/`Command` 对应。角色 token 等非确定性值由 Leader 生成后写入
  命令负载，保证各节点 apply 结果一致。
- 节点重启时用 `LoadLogEntries` 恢复日志，已落库的状态机不再重放。
- 新增节点前会先探测并把日志追平（`RaftNode.ProbePeer`），不可达的地址直接
  拒绝，不会被提交进 peer 列表（避免集群依赖一个已经挂掉的节点）。

## 命令类型

| OpType | 说明 |
|---|---|
| `config.put` / `config.delete` | 配置的写入与删除（会隐式创建命名空间） |
| `namespace.create` / `namespace.delete` | 命名空间；删除会级联删除其中配置与角色授权 |
| `role.create` / `role.update` / `role.delete` / `role.rotate` | 角色 |
| `node.register` / `node.delete` | Raft 集群成员 |

## 支持的请求

开启一个 http 服务器，监听其他节点发送的 http 请求：

- heartbeat        调用节点的 `HandleHeartbeat` 方法（无日志的 AppendEntries，
  同时承担落后节点追赶与提交索引传播）
- append_logentry  调用节点的 `HandleAppendEntries` 方法
- elect            调用节点的 `HandleElection` 方法
- 客户端写请求     由 `configcenter.Server` 调用 `RaftNode.Apply`，由 Raft 负责
  复制、提交，并通过 `SetApplyFunc` 注册的状态机函数落库

## 关键接口

- `LogStorage`：日志持久化后端（本项目用 SQLite 实现，见 `configcenter/logstore.go`）
- `ApplyFunc`：状态机，日志提交后按序执行（见 `configcenter/state.go`）
- `RaftNode.Apply(cmd)`：客户端写入口，Leader 上同步完成“复制→提交→应用”

## 测试

```
go test ./...
```
