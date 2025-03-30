package main

import (
	"fmt"
	"net/http"
	"raft/raft"
)

func main() {
	fmt.Println("初始化 Raft 节点...")
	node := raft.InitRaftNode()
	fmt.Println("Raft 节点初始化完成，节点 IP :", node.LocalIP)
	node.Start()
	fmt.Println("Raft 节点启动完成")

	// 启动 HTTP 服务器
	http.HandleFunc("/logentry", node.HandleAppendEntries)
	http.HandleFunc("/election", node.HandleElection)
	http.HandleFunc("/heartbeat", node.HandleHeartbeat)

	// 启动 HTTP 服务器
	http.ListenAndServe(fmt.Sprintf(":%s", raft.DEFAULTPORT), nil)
}
