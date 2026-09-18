package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// postAppendEntries sends an AppendEntries/heartbeat request to a peer.
func postAppendEntries(peer, path string, req AppendEntriesReq) (AppendEntriesResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return AppendEntriesResp{}, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, peerURL(peer, path), bytes.NewBuffer(body))
	if err != nil {
		return AppendEntriesResp{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: CONNECT_MAX_TIME}
	resp, err := client.Do(httpReq)
	if err != nil {
		return AppendEntriesResp{}, err
	}
	defer resp.Body.Close()

	var out AppendEntriesResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AppendEntriesResp{}, err
	}
	return out, nil
}

// HandleAppendEntries receives a log replication request from the leader.
func (rn *RaftNode) HandleAppendEntries(w http.ResponseWriter, r *http.Request) {
	var req AppendEntriesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return
	}
	resp := rn.appendEntries(req)
	if resp.Success {
		rn.notify() // valid leader contact resets our election timer
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// appendEntries is the shared Raft AppendEntries/heartbeat handler. It brings
// the local log in line with the leader's request, advances the commit index,
// persists the log and applies newly committed entries. Entries may be empty
// (a heartbeat). It must not be called with rn.mux held.
func (rn *RaftNode) appendEntries(req AppendEntriesReq) AppendEntriesResp {
	resp, changed := rn.appendEntriesLocked(req)
	if changed {
		rn.persist()
		rn.drainApply()
	}
	return resp
}

// appendEntriesLocked performs the log mutation and commit-index update while
// holding rn.mux. It returns the response and whether the local log changed
// (so the caller can persist outside the lock).
func (rn *RaftNode) appendEntriesLocked(req AppendEntriesReq) (AppendEntriesResp, bool) {
	rn.mux.Lock()
	defer rn.mux.Unlock()

	fmt.Println("处理 AppendEntries 请求，任期:", req.Term)

	// 1. Reject requests from a stale term.
	if req.Term < rn.Term {
		fmt.Println("拒绝日志，当前任期：", rn.Term, "请求任期：", req.Term)
		return AppendEntriesResp{Term: rn.Term, Success: false}, false
	}

	// 2. A valid request with a newer term makes us a follower.
	if req.Term > rn.Term || rn.state != FOLLOWER {
		rn.state = FOLLOWER
	}
	if req.Term > rn.Term {
		rn.Term = req.Term
		rn.VotedFor = ""
		rn.VoteCount = 0
	}
	rn.LeaderIP = req.LeaderIP

	// 3. Consistency check: the entry preceding the new ones must match.
	if req.PrevLogIndex >= 0 {
		if req.PrevLogIndex >= len(rn.Log) {
			fmt.Println("本地日志不足，拒绝日志，需要索引：", req.PrevLogIndex, "本地最大索引：", len(rn.Log)-1)
			return AppendEntriesResp{Term: rn.Term, Success: false}, false
		}
		if rn.Log[req.PrevLogIndex].Term != req.PrevLogTerm {
			fmt.Println("本地日志任期不一致，本地：", rn.Log[req.PrevLogIndex].Term, "leader：", req.PrevLogTerm)
			return AppendEntriesResp{Term: rn.Term, Success: false}, false
		}
	}

	// 4. Append new entries, truncating any conflicting suffix.
	changed := false
	for i, entry := range req.Entries {
		idx := req.PrevLogIndex + 1 + i
		if idx < len(rn.Log) {
			if rn.Log[idx].Term == entry.Term {
				continue // already present
			}
			rn.Log = rn.Log[:idx] // conflict: drop the rest
		}
		rn.Log = append(rn.Log, req.Entries[i:]...)
		changed = true
		break
	}

	// 5. Advance the commit index; drainApply (called by the caller) applies
	// everything newly committed.
	if req.LeaderCommit > rn.CommitIndex {
		last := len(rn.Log) - 1
		if req.LeaderCommit < last {
			rn.CommitIndex = req.LeaderCommit
		} else {
			rn.CommitIndex = last
		}
		changed = true
	}

	return AppendEntriesResp{Term: rn.Term, Success: true}, changed
}
