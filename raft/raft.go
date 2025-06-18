package raft

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	FOLLOWER  uint8 = 0
	CANDIDATE uint8 = 1
	LEADER    uint8 = 2
)

const (
	LOG_PATH         = "log.json"
	RAFT_CONFIG_PATH = "raft.json"
)

type RaftNode struct {
	mux         sync.RWMutex
	state       uint8
	Term        int      // 当前任期
	VoteCount   uint16   // 获得的选票数
	VotedFor    string   // 投票给的节点 IP
	CommitIndex int      // 已知的最大的已经被提交的日志条目的索引值
	LastApplied int      // 最后被应用到状态机的日志条目索引值
	LocalIP     string   `json:"local_ip"`
	NodeIP      []string `json:"node_ip"`
	LeaderIP    string   `json:"leader_ip"`
	Log         []LogEntry
	stateChan   chan struct{} // 添加状态变化通知通道
}

func InitRaftNode() *RaftNode {
	// 读取配置文件
	file, err := os.Open(RAFT_CONFIG_PATH)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)

	type Config struct {
		LocalIP string   `json:"local_ip"`
		NodeIP  []string `json:"node_ip"`
	}
	config := Config{}
	node := RaftNode{}
	err = decoder.Decode(&config)
	if err != nil {
		panic(err)
	}
	node.state = FOLLOWER
	node.Term = TERM_MIN
	node.VoteCount = 0
	node.LeaderIP = ""
	node.VotedFor = ""
	node.LocalIP = config.LocalIP
	node.NodeIP = config.NodeIP
	node.CommitIndex = 0
	node.LastApplied = 0
	node.mux = sync.RWMutex{}
	node.stateChan = make(chan struct{}, 1)
	node.Log = make([]LogEntry, 0)
	return &node
}

func (rn *RaftNode) Start() {
	go func() {
		for {
			rn.mux.RLock()
			currentState := rn.state
			rn.mux.RUnlock()

			switch currentState {
			case FOLLOWER:
				// 使用select同时等待超时和状态变化
				select {
				case <-time.After(HEARTBEAT_TIMEOUT):
					// 心跳超时，变为候选人
					rn.state = CANDIDATE
					fmt.Println("心跳超时，变为候选人...")
				case <-rn.stateChan:
					// 状态已变化，继续下一次循环
					fmt.Println("收到状态变化通知")
				}

			case CANDIDATE:
				// 开始选举
				fmt.Println("开始新一轮选举...")
				rn.StartElection()

				// 使用select等待选举结果或状态变化
				select {
				case <-time.After(RandomDuration()):
					// 选举超时，重新开始选举
					fmt.Println("选举超时，准备重新选举...")
				case <-rn.stateChan:
					// 状态已变化(可能成为Leader或回到Follower)
					fmt.Println("选举过程中收到状态变化通知")
				}

			case LEADER:
				// 作为Leader发送心跳
				rn.SendHeartbeat()
				time.Sleep(HEARTBEAT_PERIOD)

				// 检查是否仍然是Leader
				rn.mux.RLock()
				stillLeader := rn.state == LEADER
				rn.mux.RUnlock()

				if !stillLeader {
					// 不再是Leader，发送通知以更新循环
					select {
					case rn.stateChan <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	http.HandleFunc("/logentry", rn.HandleAppendEntries)
	http.HandleFunc("/election", rn.HandleElection)
	http.HandleFunc("/heartbeat", rn.HandleHeartbeat)
	fmt.Println("Raft 节点启动完成")
	// 启动 HTTP 服务器
	http.ListenAndServe(fmt.Sprintf(":%s", DEFAULTPORT), nil)
}
