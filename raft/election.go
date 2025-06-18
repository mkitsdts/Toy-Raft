package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

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
