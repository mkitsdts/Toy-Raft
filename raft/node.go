package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
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
	var config Config
	var node RaftNode
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

// 开始选举
func (rn *RaftNode) StartElection() {
	fmt.Println("准备选举...")

	rn.mux.Lock()
	if rn.state != CANDIDATE {
		rn.mux.Unlock()
		return
	}
	rn.VoteCount = 1
	rn.LeaderIP = ""
	rn.VotedFor = ""
	rn.Term++
	rn.mux.Unlock()

	fmt.Println("正在向其他节点发送选举请求...")

	for _, node := range rn.NodeIP {
		// 发送选举请求
		go func(targetNode string) {
			// 生成选举请求
			var prevLogIndex int
			var prevLogTerm int
			// 检测数组越界
			if len(rn.Log) == 0 {
				prevLogIndex = 0
				prevLogTerm = 0
			} else {
				prevLogIndex = len(rn.Log) - 1
				prevLogTerm = rn.Log[len(rn.Log)-1].Term
			}

			reqInfo := ElectionReq{
				Term:         rn.Term,
				CandidateIP:  rn.LocalIP,
				LastLogIndex: prevLogIndex,
				LastLogTerm:  prevLogTerm,
			}
			reqBody, _ := json.Marshal(reqInfo)
			req, err := http.NewRequest("POST", "http://"+node+":"+DEFAULTPORT+"/election", bytes.NewBuffer(reqBody))
			fmt.Println("正在向 http://" + node + ":" + DEFAULTPORT + "/election")
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")

			httpClient := &http.Client{
				Timeout: CONNECT_MAX_TIME,
			}
			fmt.Println("正在向 IP: ", node, "发送选举请求...")

			// 发送选举请求
			resp, err := httpClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			var electionResp ElectionResp
			decoder := json.NewDecoder(resp.Body)
			err = decoder.Decode(&electionResp)
			if err != nil {
				return
			}
			fmt.Println("收到 IP: ", node, "的选举")
			// 处理选举结果
			rn.mux.Lock()
			fmt.Println("当前节点 IP: ", rn.LocalIP, "当前票数：", rn.VoteCount)
			if electionResp.Term > rn.Term {
				fmt.Println("选举失败，当前任期：", rn.Term, "请求任期：", electionResp.Term)
				rn.Term = electionResp.Term
				rn.state = FOLLOWER
				rn.VoteCount = 0
				select {
				case rn.stateChan <- struct{}{}:
					fmt.Println("状态变化通知成功")
				default:
					fmt.Println("状态变化通知失败")
				}
				rn.mux.Unlock()
				return
			} else if electionResp.VoteGranted {
				fmt.Println("收到投票， IP: ", node)
				rn.VoteCount++
				if rn.VoteCount > uint16(len(rn.NodeIP)/2) {
					fmt.Println("选举成功，当前任期：", rn.Term, "请求任期：", electionResp.Term, "当选 IP: ", rn.LocalIP)
					rn.VoteCount = 0
					rn.state = LEADER
					rn.LeaderIP = rn.LocalIP
					select {
					case rn.stateChan <- struct{}{}:
						fmt.Println("状态变化通知成功")
					default:
						fmt.Println("状态变化通知失败")
					}
					rn.mux.Unlock()
				} else {
					fmt.Println("未获得 IP: ", node, " 选票，当前票数：", rn.VoteCount)
					rn.mux.Unlock()
					return
				}
			} else {
				fmt.Println("未获得 IP: ", node, " 选票，当前票数：", rn.VoteCount)
				rn.mux.Unlock()
			}
		}(node)
	}
}

// 发送信号
func (rn *RaftNode) SendHeartbeat() {
	for _, node := range rn.NodeIP {
		go func(targetNode string) {
			// 生成包
			fmt.Println("正在向 IP: ", node, "发送心跳...")

			var prevLogIndex int
			var prevLogTerm int
			// 检测数组越界
			if len(rn.Log) == 0 {
				prevLogIndex = 0
				prevLogTerm = 0
			} else {
				prevLogIndex = len(rn.Log) - 1
				prevLogTerm = rn.Log[len(rn.Log)-1].Term
			}

			reqInfo := AppendEntriesReq{
				Term:         rn.Term,
				LeaderIP:     rn.LocalIP,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      make([]LogEntry, 0),
				LeaderCommit: 0,
			}
			reqBody, _ := json.Marshal(reqInfo)
			// 发送心跳
			req, err := http.NewRequest("POST", "http://"+node+":"+DEFAULTPORT+"/heartbeat", bytes.NewBuffer(reqBody))
			if err != nil {
				return
			}
			httpClient := &http.Client{
				Timeout: CONNECT_MAX_TIME,
			}
			fmt.Println("正在等待 IP: ", node, "的响应...")
			resp, err := httpClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			// 根据返回数据判断是否掉线

			var appendResp AppendEntriesResp
			decoder := json.NewDecoder(resp.Body)
			err = decoder.Decode(&appendResp)
			if err != nil {
				return
			}
			if appendResp.Term > rn.Term {
				rn.mux.Lock()
				fmt.Println("节点掉线，当前任期：", rn.Term, "请求任期：", appendResp.Term)
				rn.state = FOLLOWER
				rn.Term = appendResp.Term
				select {
				case rn.stateChan <- struct{}{}:
					fmt.Println("状态变化通知成功")
				default:
					fmt.Println("状态变化通知失败")
				}
				rn.mux.Unlock()
			}
		}(node)
	}
}

// 发送心跳
func (rn *RaftNode) SendLogEntry(logs []LogEntry) {
	for _, node := range rn.NodeIP {
		go func(targetNode string) {
			// 生成包
			fmt.Println("正在向 IP: ", node, "发送日志...")
			reqInfo := AppendEntriesReq{
				Term:         rn.Term,
				LeaderIP:     rn.LocalIP,
				PrevLogIndex: len(rn.Log) - 1,
				PrevLogTerm:  rn.Log[len(rn.Log)-1].Term,
				Entries:      logs,
				LeaderCommit: rn.CommitIndex,
			}
			reqBody, _ := json.Marshal(reqInfo)
			// 发送日志
			req, err := http.NewRequest("POST", "http://"+node+":"+DEFAULTPORT+"/logentry", bytes.NewBuffer(reqBody))
			if err != nil {
				return
			}
			httpClient := &http.Client{
				Timeout: CONNECT_MAX_TIME,
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			// 根据返回数据判断是否掉线
			var appendResp AppendEntriesResp
			decoder := json.NewDecoder(resp.Body)
			err = decoder.Decode(&appendResp)
			if err != nil {
				return
			}
			if appendResp.Term > rn.Term {
				fmt.Println("节点掉线，当前任期：", rn.Term, "请求任期：", appendResp.Term)
				rn.state = FOLLOWER
				rn.Term = appendResp.Term
			} else if !appendResp.Success {
				fmt.Println("未收到响应, IP: ", node, "正在重新发送请求...")
				rn.SendLogEntry(logs)
			}
		}(node)
	}
}

// 暂时不能保存大量数据
// 保存日志到磁盘
func (rn *RaftNode) PersisLog() {

	if rn.CommitIndex-rn.LastApplied <= 0 {
		fmt.Println("没有需要保存的日志")
		return
	}

	// 判断文件是否存在
	_, err := os.Stat(LOG_PATH)
	if err != nil {
		// 创建文件
		file, err := os.Create(LOG_PATH)
		if err != nil {
			return
		}
		defer file.Close()
	}
	// 打开文件
	file, err := os.OpenFile(LOG_PATH, os.O_WRONLY, 0666)
	if err != nil {
		return
	}
	defer file.Close()

	var logs []LogEntry
	// 读取日志
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&logs)
	if err != nil {
		return
	}
	fmt.Println("读取日志成功，当前日志数量：", len(logs))
	fmt.Println("正在写入日志至磁盘...")
	// 写入日志
	for i := rn.LastApplied; i < rn.CommitIndex; i++ {
		logs = append(logs, rn.Log[i])
	}
	fmt.Println("写入日志成功，当前日志数量：", len(logs))
	// 保存日志
	encoder := json.NewEncoder(file)
	err = encoder.Encode(logs)
	if err != nil {
		return
	}
}
