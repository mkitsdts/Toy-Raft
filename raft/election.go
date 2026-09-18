package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// StartElection transitions this node to candidate and requests votes from all
// peers. A candidate that wins a majority of the cluster (including itself)
// becomes leader.
func (rn *RaftNode) StartElection() {
	fmt.Println("准备选举...")

	rn.mux.Lock()
	if rn.state != CANDIDATE {
		rn.mux.Unlock()
		return
	}
	rn.VoteCount = 1 // vote for ourselves
	rn.LeaderIP = ""
	rn.VotedFor = rn.LocalIP
	rn.Term++
	term := rn.Term
	lastIndex, lastTerm := rn.lastLogIndexTermLocked()
	rn.mux.Unlock()

	peers := rn.peerList()
	if len(peers) == 0 {
		rn.mux.Lock()
		rn.becomeLeaderLocked()
		rn.mux.Unlock()
		rn.notify()
		return
	}

	need := (len(peers)+1)/2 + 1
	fmt.Println("正在向其他节点发送选举请求...")

	for _, node := range peers {
		go func(targetNode string) {
			reqInfo := ElectionReq{
				Term:         term,
				CandidateIP:  rn.LocalIP,
				LastLogIndex: lastIndex,
				LastLogTerm:  lastTerm,
			}
			reqBody, _ := json.Marshal(reqInfo)
			req, err := http.NewRequest("POST", peerURL(targetNode, "/election"), bytes.NewBuffer(reqBody))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: CONNECT_MAX_TIME}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			var electionResp ElectionResp
			if err := json.NewDecoder(resp.Body).Decode(&electionResp); err != nil {
				return
			}

			rn.mux.Lock()
			defer rn.mux.Unlock()
			// Ignore replies from an election we already lost or an older term.
			if rn.state != CANDIDATE || rn.Term != term {
				return
			}
			if electionResp.Term > rn.Term {
				rn.Term = electionResp.Term
				rn.state = FOLLOWER
				rn.VoteCount = 0
				rn.LeaderIP = ""
				rn.VotedFor = ""
				rn.notify()
				return
			}
			if !electionResp.VoteGranted {
				fmt.Println("未获得 IP: ", targetNode, " 选票")
				return
			}
			rn.VoteCount++
			fmt.Println("收到投票，IP:", targetNode, "当前票数：", rn.VoteCount, "需要：", need)
			if int(rn.VoteCount) >= need {
				fmt.Println("选举成功，当前任期：", rn.Term, "当选 IP: ", rn.LocalIP)
				rn.becomeLeaderLocked()
				rn.notify()
			}
		}(node)
	}
}

// HandleElection processes a RequestVote RPC.
func (rn *RaftNode) HandleElection(w http.ResponseWriter, r *http.Request) {
	var req ElectionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return
	}
	resp := rn.handleVote(req)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleVote applies the Raft voting rules and returns the response.
func (rn *RaftNode) handleVote(req ElectionReq) ElectionResp {
	rn.mux.Lock()
	defer rn.mux.Unlock()

	// 1. Reject stale terms.
	if req.Term < rn.Term {
		fmt.Println("任期不同，拒绝投票，当前任期：", rn.Term, "请求任期：", req.Term)
		return ElectionResp{Term: rn.Term, VoteGranted: false}
	}

	// 2. A newer term means we step down and clear our vote.
	if req.Term > rn.Term {
		rn.Term = req.Term
		rn.state = FOLLOWER
		rn.LeaderIP = ""
		rn.VotedFor = ""
		rn.VoteCount = 0
	}

	// 3. One vote per term.
	if rn.VotedFor != "" && rn.VotedFor != req.CandidateIP {
		fmt.Println("本轮已投票给", rn.VotedFor, "，拒绝投票")
		return ElectionResp{Term: rn.Term, VoteGranted: false}
	}

	// 4. The candidate's log must be at least as up to date as ours.
	lastIndex, lastTerm := rn.lastLogIndexTermLocked()
	upToDate := req.LastLogTerm > lastTerm ||
		(req.LastLogTerm == lastTerm && req.LastLogIndex >= lastIndex)
	if !upToDate {
		fmt.Println("日志不够新，拒绝投票，本地：", lastIndex, lastTerm, "候选：", req.LastLogIndex, req.LastLogTerm)
		return ElectionResp{Term: rn.Term, VoteGranted: false}
	}

	rn.VotedFor = req.CandidateIP
	rn.LeaderIP = ""
	fmt.Println("投票成功，候选节点：", req.CandidateIP, "当前任期：", rn.Term)
	rn.notify()
	return ElectionResp{Term: rn.Term, VoteGranted: true}
}
