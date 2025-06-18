package main

import (
	"fmt"
	"raft/raft"
)

func main() {
	fmt.Println("初始化 Raft 节点...")
	node := raft.InitRaftNode()
	fmt.Println("Raft 节点初始化完成，节点 IP :", node.LocalIP)

	node.Start()

}
