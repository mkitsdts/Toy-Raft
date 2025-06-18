package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// 发送心跳
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

			appendResp := AppendEntriesResp{}
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

func (rn *RaftNode) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	// 解析数据
	var reqInfo AppendEntriesReq
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&reqInfo); err != nil {
		return
	}
	rn.mux.RLock()
	fmt.Println("处理心跳请求...")
	// 检查任期
	if reqInfo.Term < rn.Term {
		// 拒绝心跳
		fmt.Println("拒绝心跳，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		var respInfo AppendEntriesResp
		respInfo.Term = rn.Term
		rn.mux.RUnlock()
		respInfo.Success = false
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		return
	} else if reqInfo.Term > rn.Term {
		// 更新自身状态
		rn.mux.RUnlock()
		rn.mux.Lock()
		rn.Term = reqInfo.Term
		rn.state = FOLLOWER
		select {
		case rn.stateChan <- struct{}{}:
			fmt.Println("状态变化通知成功")
		default:
			fmt.Println("状态变化通知失败")
		}
		rn.LeaderIP = reqInfo.LeaderIP
		rn.VotedFor = ""
		rn.VoteCount = 0
		rn.mux.Unlock()
		// 返回心跳
		var respInfo AppendEntriesResp
		respInfo.Term = rn.Term
		respInfo.Success = true
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("心跳成功，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	}
	// 检查日志一致性
	if reqInfo.PrevLogIndex < len(rn.Log) {
		// 本地数据比leader节点更新 拒绝心跳
		fmt.Println("本地数据比 leader 节点新， 拒绝心跳")
		var respInfo AppendEntriesResp
		respInfo.Term = rn.Term
		rn.mux.RUnlock()
		respInfo.Success = false
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		return
	}
	rn.mux.RUnlock()
	rn.mux.Lock()
	// 更新自身状态
	rn.Term = reqInfo.Term
	rn.state = FOLLOWER
	rn.LeaderIP = reqInfo.LeaderIP
	rn.VotedFor = ""
	rn.VoteCount = 0

	if reqInfo.LeaderCommit > rn.CommitIndex {
		rn.CommitIndex = min(reqInfo.LeaderCommit, len(rn.Log)-1)
	}
	rn.mux.Unlock()
	// 返回心跳
	var respInfo AppendEntriesResp
	respInfo.Term = rn.Term
	respInfo.Success = true
	respBody, _ := json.Marshal(respInfo)
	w.Write(respBody)
	select {
	case rn.stateChan <- struct{}{}:
		fmt.Println("状态变化通知成功")
	default:
		fmt.Println("状态变化通知失败")
	}
	fmt.Println("心跳成功，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
}
