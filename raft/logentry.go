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
	resp, result := rn.appendEntriesLocked(req)
	if result.truncatedFrom >= 0 {
		rn.truncateLog(result.truncatedFrom)
	}
	if result.changed {
		rn.persist()
	}
	// Undo already applied entries the conflicting suffix removed.
	rn.rollbackEntries(result.discarded)
	if result.changed {
		rn.drainApply()
	}
	return resp
}

// appendResult reports what appendEntriesLocked did to the local log so the
// caller can persist, truncate and roll back outside the lock.
type appendResult struct {
	// changed is true when the log or commit index was modified.
	changed bool
	// truncatedFrom is the first index dropped because it conflicted with the
	// leader's log, or -1 when nothing was truncated.
	truncatedFrom int
	// discarded holds the truncated entries that had already been applied.
	discarded []LogEntry
}

// appendEntriesLocked performs the log mutation and commit-index update while
// holding rn.mux.
func (rn *RaftNode) appendEntriesLocked(req AppendEntriesReq) (AppendEntriesResp, appendResult) {
	rn.mux.Lock()
	defer rn.mux.Unlock()

	fmt.Println("处理 AppendEntries 请求，任期:", req.Term)

	// 1. Reject requests from a stale term.
	if req.Term < rn.Term {
		fmt.Println("拒绝日志，当前任期：", rn.Term, "请求任期：", req.Term)
		return AppendEntriesResp{Term: rn.Term, Success: false}, appendResult{truncatedFrom: -1}
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
			return AppendEntriesResp{Term: rn.Term, Success: false}, appendResult{truncatedFrom: -1}
		}
		if rn.Log[req.PrevLogIndex].Term != req.PrevLogTerm {
			fmt.Println("本地日志任期不一致，本地：", rn.Log[req.PrevLogIndex].Term, "leader：", req.PrevLogTerm)
			return AppendEntriesResp{Term: rn.Term, Success: false}, appendResult{truncatedFrom: -1}
		}
	}

	// 4. Append new entries, truncating any conflicting suffix.
	result := appendResult{truncatedFrom: -1}
	for i, entry := range req.Entries {
		idx := req.PrevLogIndex + 1 + i
		if idx < len(rn.Log) {
			if rn.Log[idx].Term == entry.Term {
				continue // already present
			}
			// Conflict: drop the rest of our suffix. Remember the entries that
			// were already applied so the state machine can undo them once the
			// lock is released.
			result.truncatedFrom = idx
			for j := idx; j < len(rn.Log); j++ {
				if j <= rn.LastApplied {
					result.discarded = append(result.discarded, rn.Log[j])
				}
			}
			rn.Log = rn.Log[:idx]
			if rn.LastApplied >= idx {
				rn.LastApplied = idx - 1
			}
			if rn.CommitIndex >= idx {
				rn.CommitIndex = idx - 1
			}
		}
		rn.Log = append(rn.Log, req.Entries[i:]...)
		result.changed = true
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
		result.changed = true
	}

	return AppendEntriesResp{Term: rn.Term, Success: true}, result
}

// truncateLog deletes the persisted entries a conflicting suffix replaced.
func (rn *RaftNode) truncateLog(fromIndex int) {
	rn.mux.RLock()
	storage := rn.storage
	rn.mux.RUnlock()
	if storage == nil {
		return
	}
	if err := storage.TruncateLogEntries(fromIndex); err != nil {
		fmt.Println("raft: 截断日志失败:", err)
	}
}
