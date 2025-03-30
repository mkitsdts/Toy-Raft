package raft

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 处理选举请求
func (rn *RaftNode) HandleElection(w http.ResponseWriter, r *http.Request) {
	// 解析数据
	var reqInfo ElectionReq
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&reqInfo)
	if err != nil {
		return
	}
	// 处理选举
	fmt.Println("处理选举请求...")
	if rn.VotedFor != reqInfo.CandidateIP && rn.VotedFor != "" {
		// 拒绝投票
		respInfo := ElectionResp{
			Term:        rn.Term,
			VoteGranted: false,
		}
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("投票节点不一样，拒绝投票，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	} else if rn.VotedFor == reqInfo.CandidateIP {
		respInfo := ElectionResp{
			Term:        rn.Term,
			VoteGranted: true,
		}
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("同意投票，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	}

	if reqInfo.Term < rn.Term {
		// 拒绝投票
		respInfo := ElectionResp{
			Term:        rn.Term,
			VoteGranted: false,
		}
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("任期不同，拒绝投票，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	} else if reqInfo.Term > rn.Term {
		// 更新自身状态
		rn.Term = reqInfo.Term
		rn.state = FOLLOWER
		rn.LeaderIP = ""
		rn.VotedFor = reqInfo.CandidateIP
		rn.VoteCount = 0
		// 返回投票
		respInfo := ElectionResp{
			Term:        rn.Term,
			VoteGranted: true,
		}
		respBody, _ := json.Marshal(respInfo)
		w.Write(respBody)
		fmt.Println("投票成功，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
		return
	} else {
		if rn.state == CANDIDATE {
			if len(rn.Log)-1 > reqInfo.LastLogIndex {
				// 拒绝投票
				respInfo := ElectionResp{
					Term:        rn.Term,
					VoteGranted: false,
				}
				respBody, _ := json.Marshal(respInfo)
				w.Write(respBody)
				fmt.Println("日志不够新，拒绝投票，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
				return
			} else {
				// 投票
				respInfo := ElectionResp{
					Term:        rn.Term,
					VoteGranted: true,
				}
				rn.Term = reqInfo.Term
				rn.state = FOLLOWER
				rn.VotedFor = reqInfo.CandidateIP
				respBody, _ := json.Marshal(respInfo)
				w.Write(respBody)
				rn.LeaderIP = reqInfo.CandidateIP
				fmt.Println("投票成功，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
				return
			}
		}
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
	rn.PersisLog()
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
		rn.mux.RLock()
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
	select {
	case rn.stateChan <- struct{}{}:
		fmt.Println("状态变化通知成功")
	default:
		fmt.Println("状态变化通知失败")
	}
	rn.mux.Unlock()

	if reqInfo.LeaderCommit > rn.CommitIndex {
		rn.CommitIndex = min(reqInfo.LeaderCommit, len(rn.Log)-1)
	}
	// 返回心跳
	var respInfo AppendEntriesResp
	respInfo.Term = rn.Term
	respInfo.Success = true
	respBody, _ := json.Marshal(respInfo)
	w.Write(respBody)
	fmt.Println("心跳成功，当前任期：", rn.Term, "请求任期：", reqInfo.Term)
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
					rn.mux.Lock()
					rn.state = CANDIDATE
					rn.mux.Unlock()
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
}
