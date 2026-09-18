package raft

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// SendHeartbeat is called periodically by the leader. Besides asserting
// leadership it also replicates any log entries a follower is still missing
// and propagates the current commit index, so a restarted follower catches up
// without waiting for the next client write. Peers are contacted concurrently
// so one unreachable node cannot delay heartbeats to the rest of the cluster.
func (rn *RaftNode) SendHeartbeat() {
	peers := rn.peerList()
	if len(peers) == 0 {
		return
	}

	rn.mux.RLock()
	if rn.state != LEADER {
		rn.mux.RUnlock()
		return
	}
	lastIndex := len(rn.Log) - 1
	rn.mux.RUnlock()

	for _, peer := range peers {
		go func(target string) {
			fmt.Println("正在向 IP: ", target, "发送心跳...")
			rn.replicateTo(target, lastIndex)
		}(peer)
	}
}

// HandleHeartbeat accepts a leader heartbeat (an AppendEntries request with no
// entries). It shares the exact same logic as log replication: it validates
// the term and consistency, updates the leader and commit index, and applies
// newly committed entries.
func (rn *RaftNode) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req AppendEntriesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return
	}
	resp := rn.appendEntries(req)
	if resp.Success {
		rn.notify()
	}
	_ = json.NewEncoder(w).Encode(resp)
}
