package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// 发送日志
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

// 处理日志
func (rn *RaftNode) HandleAppendEntries(w http.ResponseWriter, r *http.Request) {
	// 解析数据
	var reqInfo AppendEntriesReq
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&reqInfo); err != nil {
		return
	}

	rn.mux.Lock()
	fmt.Println("处理日志请求...")
	// 处理日志
	if reqInfo.Term < rn.Term {
		// 拒绝日志
		var respInfo AppendEntriesResp
		respInfo.Term = rn.Term
		rn.mux.Unlock()
		respInfo.Success = false
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("拒绝日志，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	}
	// 检查日志一致性
	if rn.Log[reqInfo.PrevLogIndex].Term != reqInfo.PrevLogTerm {
		// 拒绝日志
		var respInfo AppendEntriesResp
		respInfo.Term = rn.Term
		rn.mux.Unlock()
		respInfo.Success = false
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("本地日志：", rn.Log[reqInfo.PrevLogIndex].Term, "请求日志：", reqInfo.PrevLogTerm)
		return
	}
	// 更新日志
	rn.Log = append(rn.Log, reqInfo.Entries...)
	// 更新提交索引
	if reqInfo.LeaderCommit > rn.Log[len(rn.Log)-1].Term {
		rn.Log[len(rn.Log)-1].Term = reqInfo.LeaderCommit
	}
	// 返回日志
	var respInfo AppendEntriesResp
	respInfo.Term = rn.Term
	rn.mux.Unlock()
	respInfo.Success = true
	respBody, _ := json.Marshal(respInfo)
	w.Write(respBody)
	PersisLog(rn)
}

func (rn *RaftNode) HandleClientAppendEntries(w http.ResponseWriter, r *http.Request) {
	rn.mux.RLock()
	isLeader := rn.state == LEADER
	leaderIP := rn.LeaderIP
	rn.mux.RUnlock()

	if !isLeader {
		// 重定向请求
		response := map[string]string{
			"success": "false",
			"error":   "not leader",
			"leader":  leaderIP,
		}
		jsonResp, _ := json.Marshal(response)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTemporaryRedirect)
		w.Write(jsonResp)
		return
	}

	// 解析数据
	var reqInfo AppendEntriesReq
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&reqInfo); err != nil {
		return
	}
	// 处理日志
	rn.mux.Lock()
	fmt.Println("处理客户端日志请求...")
	// 更新日志
	rn.Log = append(rn.Log, reqInfo.Entries...)
	// 更新提交索引
	if reqInfo.LeaderCommit > rn.CommitIndex {
		rn.CommitIndex = min(reqInfo.LeaderCommit, len(rn.Log)-1)
	}
	// 返回日志
	var respInfo AppendEntriesResp
	respInfo.Term = rn.Term
	respInfo.Success = true
	respBody, _ := json.Marshal(respInfo)
	w.Write(respBody)
	PersisLog(rn)
	rn.mux.Unlock()
}
