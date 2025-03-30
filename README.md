# Raft

该package用以实现节点间数据库日志同步

## 使用方法

开启一个http服务器，监听其他节点发送的http请求，分为heartbeat,append_logentry,elect,commit共4种请求

对于不同请求调用不同方法，其中commit_logentry需要额外实现

- heartbeat             调用节点的 HandleHeartbeat 方法

- append_logentry       调用节点的 HandleAppendEntries 方法

- elect                 调用节点的 HandleElection 方法

- commit                调用额外写的方法，需要实现 根据日志完成数据库的更改