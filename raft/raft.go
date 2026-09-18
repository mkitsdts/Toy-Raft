package raft

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	FOLLOWER  uint8 = 0
	CANDIDATE uint8 = 1
	LEADER    uint8 = 2
)

// NotLeaderError is returned by Apply when this node is not the leader. It
// carries the address of the leader (when known) so HTTP handlers can redirect
// the write to the right node.
type NotLeaderError struct {
	Leader string
}

func (e *NotLeaderError) Error() string {
	if e.Leader == "" {
		return "raft: not leader (leader unknown)"
	}
	return "raft: not leader, current leader is " + e.Leader
}

// ApplyFunc is the state machine. It is invoked once per committed log entry,
// in log order, on every node. It must be deterministic: replaying the same
// command must produce the same result.
type ApplyFunc func(Command) error

// RollbackFunc compensates a command whose effect must be undone:
//   - ApplyFunc returned an error, so the state machine may hold a partial
//     write that the callback has to revert, or
//   - an already applied log entry was discarded because it conflicted with the
//     leader's log.
//
// Entries that were never applied are never passed to it, so it does not need
// to guess whether the command took effect; it must, however, be idempotent.
type RollbackFunc func(Command) error

// Config bundles everything required to build a running Raft node.
type Config struct {
	// LocalIP is this node's address, with or without a port.
	LocalIP string
	// Peers are the other cluster members' addresses. An empty list makes this
	// node a single-node leader immediately.
	Peers []string
	// Storage persists the replicated log. It may be nil for an in-memory node.
	Storage LogStorage
	// Apply is the state machine; Rollback compensates a failed or discarded
	// apply. Both may be nil.
	Apply    ApplyFunc
	Rollback RollbackFunc
}

// New builds a Raft node from cfg. It restores any persisted log and registers
// the state-machine callbacks. Once built, the caller only has to expose the
// RPC routes (RegisterRoutes) and start the background loops (Run); the Raft
// library takes care of replication, commitment, applying and rollback.
func New(cfg Config) (*RaftNode, error) {
	n := InitRaftNode(cfg.LocalIP, cfg.Peers)
	if cfg.Apply != nil {
		n.SetApplyFunc(cfg.Apply)
	}
	if cfg.Rollback != nil {
		n.SetRollbackFunc(cfg.Rollback)
	}
	if cfg.Storage != nil {
		if err := n.SetStorage(cfg.Storage); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// RaftNode is a small Raft node used by the configuration center. All mutable
// application data is represented by Command values and is applied only after
// a majority has acknowledged its log entry.
type RaftNode struct {
	mux sync.RWMutex
	// writeMux serializes client writes, commit application and peer
	// replication so log indexes never interleave.
	writeMux sync.Mutex
	// applyMux serializes applying committed entries to the state machine. The
	// state machine runs without mux held, so it may safely call back into the
	// node (e.g. to update the peer list after a membership command).
	applyMux    sync.Mutex
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
	// nextIndex tracks, per peer, the next log index to replicate. It is
	// rebuilt whenever this node becomes leader.
	nextIndex map[string]int
	stateChan chan struct{}
	stopCh    chan struct{}
	stopOnce  sync.Once
	apply     ApplyFunc
	rollback  RollbackFunc
	storage   LogStorage
}

func InitRaftNode(localIP string, nodeIPs []string) *RaftNode {
	n := &RaftNode{
		state: FOLLOWER, Term: TERM_MIN, LocalIP: strings.TrimSpace(localIP),
		CommitIndex: -1, LastApplied: -1, stateChan: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		Log:       make([]LogEntry, 0),
		nextIndex: make(map[string]int),
	}
	// SetNodes also promotes an empty cluster to a single-node leader, which is
	// the majority immediately.
	n.SetNodes(nodeIPs)
	return n
}

// SetNodes replaces the peer list at runtime. Entries may be "host" or
// "host:port"; the local node itself and duplicates are ignored. Registered
// nodes added through the config center take effect without a restart.
func (rn *RaftNode) SetNodes(addrs []string) {
	self := PeerAddress(rn.LocalIP)
	nodes := make([]string, 0, len(addrs))
	seen := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		normalized := PeerAddress(addr)
		if normalized == "" || normalized == self {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		nodes = append(nodes, normalized)
	}

	rn.mux.Lock()
	rn.NodeIP = nodes
	if rn.nextIndex == nil {
		rn.nextIndex = make(map[string]int)
	}
	for _, peer := range nodes {
		if _, ok := rn.nextIndex[peer]; !ok {
			rn.nextIndex[peer] = len(rn.Log)
		}
	}
	if len(nodes) == 0 && rn.state != LEADER {
		rn.state = LEADER
		rn.LeaderIP = rn.LocalIP
	}
	rn.mux.Unlock()
	rn.notify()
}

// Nodes returns a snapshot of the current peer list.
func (rn *RaftNode) Nodes() []string { return rn.peerList() }

// peerList returns a copy of NodeIP under the read lock so callers can iterate
// safely while the list is being updated.
func (rn *RaftNode) peerList() []string {
	rn.mux.RLock()
	defer rn.mux.RUnlock()
	return rn.peerListLocked()
}

// peerListLocked is peerList for callers that already hold rn.mux.
func (rn *RaftNode) peerListLocked() []string {
	out := make([]string, len(rn.NodeIP))
	copy(out, rn.NodeIP)
	return out
}

// lastLogIndexTermLocked returns the index and term of the last log entry, or
// (-1, 0) for an empty log. Callers must hold rn.mux.
func (rn *RaftNode) lastLogIndexTermLocked() (int, int) {
	if len(rn.Log) == 0 {
		return -1, 0
	}
	return len(rn.Log) - 1, rn.Log[len(rn.Log)-1].Term
}

func (rn *RaftNode) SetApplyFunc(fn ApplyFunc) { rn.mux.Lock(); rn.apply = fn; rn.mux.Unlock() }

// SetRollbackFunc registers the compensation function invoked when a command
// cannot be applied cleanly or when an applied log entry is discarded.
func (rn *RaftNode) SetRollbackFunc(fn RollbackFunc) {
	rn.mux.Lock()
	rn.rollback = fn
	rn.mux.Unlock()
}

// rollbackFunc snapshots the registered callback under the read lock.
func (rn *RaftNode) rollbackFunc() RollbackFunc {
	rn.mux.RLock()
	defer rn.mux.RUnlock()
	return rn.rollback
}

// SetStorage attaches the persistence backend and restores the persisted log
// into memory. It must be called before Run so the election/heartbeat loop
// observes the recovered log.
func (rn *RaftNode) SetStorage(s LogStorage) error {
	entries, err := s.LoadLogEntries()
	if err != nil {
		return err
	}
	rn.mux.Lock()
	defer rn.mux.Unlock()
	rn.storage = s
	if len(entries) > 0 {
		rn.Log = entries
		// Persisted entries are treated as committed. The application state
		// machine (the configurations table) is persisted as well, so there is
		// no need to replay on startup.
		rn.CommitIndex = len(entries) - 1
		rn.LastApplied = len(entries) - 1
	}
	if rn.nextIndex == nil {
		rn.nextIndex = make(map[string]int)
	}
	for _, peer := range rn.NodeIP {
		if _, ok := rn.nextIndex[peer]; !ok {
			rn.nextIndex[peer] = len(rn.Log)
		}
	}
	return nil
}

func (rn *RaftNode) IsLeader() bool {
	rn.mux.RLock()
	defer rn.mux.RUnlock()
	return rn.state == LEADER
}
func (rn *RaftNode) Leader() string { rn.mux.RLock(); defer rn.mux.RUnlock(); return rn.LeaderIP }
func (rn *RaftNode) Status() map[string]any {
	rn.mux.RLock()
	defer rn.mux.RUnlock()
	states := []string{"follower", "candidate", "leader"}
	return map[string]any{
		"state": states[rn.state], "term": rn.Term, "leader": rn.LeaderIP,
		"local_ip": rn.LocalIP, "commit_index": rn.CommitIndex,
		"last_applied": rn.LastApplied, "last_log_index": len(rn.Log) - 1,
	}
}

func (rn *RaftNode) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /logentry", rn.HandleAppendEntries)
	mux.HandleFunc("POST /election", rn.HandleElection)
	mux.HandleFunc("POST /heartbeat", rn.HandleHeartbeat)
}

func (rn *RaftNode) notify() {
	select {
	case rn.stateChan <- struct{}{}:
	default:
	}
}

// becomeLeaderLocked promotes this node and (re)builds the replication
// progress for every peer. Callers must hold rn.mux.
func (rn *RaftNode) becomeLeaderLocked() {
	rn.state = LEADER
	rn.LeaderIP = rn.LocalIP
	rn.VoteCount = 0
	rn.nextIndex = make(map[string]int, len(rn.NodeIP))
	for _, peer := range rn.NodeIP {
		rn.nextIndex[peer] = len(rn.Log)
	}
}

// stepDown abandons leadership when a higher term is observed.
func (rn *RaftNode) stepDown(term int) {
	rn.mux.Lock()
	if term > rn.Term {
		rn.Term = term
		rn.state = FOLLOWER
		rn.LeaderIP = ""
		rn.VotedFor = ""
		rn.VoteCount = 0
	}
	rn.mux.Unlock()
	rn.notify()
}

// Apply proposes a client command. On the leader it appends the command to the
// log, replicates it to a majority, commits it and applies it to the state
// machine before returning. A non-leader returns *NotLeaderError. Apply is
// serialized with a dedicated mutex so concurrent clients cannot interleave
// log indexes.
func (rn *RaftNode) Apply(cmd Command) (int, error) {
	rn.writeMux.Lock()
	defer rn.writeMux.Unlock()

	rn.mux.Lock()
	if rn.state != LEADER {
		leader := rn.LeaderIP
		rn.mux.Unlock()
		return -1, &NotLeaderError{Leader: leader}
	}
	rn.Log = append(rn.Log, LogEntry{Term: rn.Term, Comm: cmd})
	index := len(rn.Log) - 1
	peers := rn.peerListLocked()
	rn.mux.Unlock()

	// Persist before replicating so a crash cannot lose an acknowledged entry.
	rn.persist()

	if len(peers) > 0 {
		need := (len(peers)+1)/2 + 1
		acked := rn.replicateQuorum(peers, index, need)
		if len(acked)+1 < need {
			return index, fmt.Errorf("raft: quorum not reached for entry %d (%d/%d nodes)", index, len(acked)+1, len(peers)+1)
		}
		rn.commitTo(index)
		// Propagate the new commit index to the peers that are up so they apply
		// the entry promptly. Unreachable peers catch up via heartbeat.
		for _, peer := range acked {
			go rn.replicateTo(peer, index)
		}
		return index, nil
	}

	rn.commitTo(index)
	return index, nil
}

// replicateQuorum replicates up to target to every peer concurrently and
// returns the peers that acknowledged. It returns as soon as the caller plus
// the acknowledged peers form a majority (need), so a single slow or
// unreachable peer cannot stall a write that already has quorum.
func (rn *RaftNode) replicateQuorum(peers []string, target, need int) []string {
	type result struct {
		peer string
		ok   bool
	}
	ch := make(chan result, len(peers))
	for _, peer := range peers {
		go func(p string) { ch <- result{p, rn.replicateTo(p, target)} }(peer)
	}

	acked := make([]string, 0, len(peers))
	for range peers {
		r := <-ch
		if r.ok {
			acked = append(acked, r.peer)
			if len(acked)+1 >= need {
				break // remaining goroutines still finish and drain into ch
			}
		}
	}
	return acked
}

// replicateTo sends log entries up to target to one peer, backing off on
// consistency mismatches until the follower accepts. It returns true when the
// peer's log is up to date with target.
func (rn *RaftNode) replicateTo(peer string, target int) bool {
	for attempt := 0; attempt <= 256; attempt++ {
		rn.mux.Lock()
		if rn.state != LEADER {
			rn.mux.Unlock()
			return false
		}
		next, ok := rn.nextIndex[peer]
		if !ok || next < 0 {
			next = 0
		}
		if next > len(rn.Log) {
			next = len(rn.Log)
		}
		end := target + 1
		if end > len(rn.Log) {
			end = len(rn.Log)
		}
		if next > end {
			next = end
		}
		prevIndex := next - 1
		prevTerm := 0
		if prevIndex >= 0 && prevIndex < len(rn.Log) {
			prevTerm = rn.Log[prevIndex].Term
		}
		entries := make([]LogEntry, end-next)
		copy(entries, rn.Log[next:end])
		req := AppendEntriesReq{
			Term:         rn.Term,
			LeaderIP:     rn.LocalIP,
			PrevLogIndex: prevIndex,
			PrevLogTerm:  prevTerm,
			Entries:      entries,
			LeaderCommit: rn.CommitIndex,
		}
		term := rn.Term
		wasBottom := next == 0
		rn.mux.Unlock()

		resp, err := postAppendEntries(peer, "/logentry", req)
		if err != nil {
			return false
		}
		if resp.Term > term {
			rn.stepDown(resp.Term)
			return false
		}
		if resp.Success {
			rn.mux.Lock()
			if end > rn.nextIndex[peer] {
				rn.nextIndex[peer] = end
			}
			rn.mux.Unlock()
			return true
		}
		// The follower rejected on a consistency mismatch: back off one entry
		// and try again.
		rn.mux.Lock()
		if rn.nextIndex[peer] > 0 {
			rn.nextIndex[peer]--
		}
		rn.mux.Unlock()
		if wasBottom {
			return false
		}
	}
	return false
}

// ProbePeer brings an as-yet-unconfigured peer up to date with the leader's
// log and reports whether it was reachable. It is used before adding a node to
// the cluster so an unreachable node is never committed into the peer set
// (which would otherwise make the cluster require a vote it cannot get). The
// peer is not added to the configuration here; that happens when the
// node.register command is applied.
func (rn *RaftNode) ProbePeer(addr string) bool {
	peer := PeerAddress(addr)
	if peer == "" || peer == PeerAddress(rn.LocalIP) {
		return true
	}

	rn.mux.Lock()
	if rn.state != LEADER {
		rn.mux.Unlock()
		return false
	}
	if rn.nextIndex == nil {
		rn.nextIndex = make(map[string]int)
	}
	if _, ok := rn.nextIndex[peer]; !ok {
		rn.nextIndex[peer] = 0 // send the whole log so the new node catches up
	}
	target := len(rn.Log) - 1
	rn.mux.Unlock()

	fmt.Println("正在探测新节点:", peer)
	return rn.replicateTo(peer, target)
}

// commitTo advances the commit index to index (when newer) and applies every
// newly committed entry to the state machine.
func (rn *RaftNode) commitTo(index int) {
	rn.mux.Lock()
	if index > rn.CommitIndex {
		rn.CommitIndex = index
	}
	rn.mux.Unlock()
	rn.drainApply()
	rn.persist()
}

// drainApply applies every committed-but-not-yet-applied entry, in order and
// exactly once. It is safe to call concurrently: applyMux serializes callers and
// LastApplied is advanced under mux before the state machine runs, so an entry is
// never applied twice. The state machine is invoked with mux released.
func (rn *RaftNode) drainApply() {
	rn.applyMux.Lock()
	defer rn.applyMux.Unlock()

	for {
		rn.mux.Lock()
		if rn.LastApplied >= rn.CommitIndex || rn.LastApplied+1 >= len(rn.Log) {
			rn.mux.Unlock()
			return
		}
		rn.LastApplied++
		index := rn.LastApplied
		entry := rn.Log[index]
		apply := rn.apply
		rn.mux.Unlock()

		if apply == nil {
			continue
		}
		if err := apply(entry.Comm); err != nil {
			fmt.Printf("raft: apply log[%d] failed: %v\n", index, err)
			rn.rollbackOne(entry.Comm)
		}
	}
}

// rollbackOne runs the RollbackFunc for a single command. Callers must hold
// applyMux, which also serializes rollback against in-flight applies.
func (rn *RaftNode) rollbackOne(cmd Command) {
	rollback := rn.rollbackFunc()
	if rollback == nil {
		return
	}
	if err := rollback(cmd); err != nil {
		fmt.Printf("raft: rollback command %q failed: %v\n", cmd.OpType, err)
	}
}

// rollbackEntries undoes a suffix of already applied log entries, newest first,
// so the state machine ends up consistent with the truncated log.
func (rn *RaftNode) rollbackEntries(entries []LogEntry) {
	if len(entries) == 0 {
		return
	}
	rn.applyMux.Lock()
	defer rn.applyMux.Unlock()
	for i := len(entries) - 1; i >= 0; i-- {
		rn.rollbackOne(entries[i].Comm)
	}
}

// persist writes the whole in-memory log to the storage backend. The backend
// upserts by index, so repeated writes are idempotent.
func (rn *RaftNode) persist() {
	rn.mux.RLock()
	storage := rn.storage
	entries := make([]LogEntry, len(rn.Log))
	copy(entries, rn.Log)
	rn.mux.RUnlock()

	if storage == nil || len(entries) == 0 {
		return
	}
	if err := storage.AppendLogEntries(0, entries); err != nil {
		fmt.Println("raft: persist log failed:", err)
	}
}

func (rn *RaftNode) Run() {
	go func() {
		for {
			rn.mux.RLock()
			state := rn.state
			rn.mux.RUnlock()
			if state == LEADER {
				// A single-node (or not-yet-configured) cluster has no peers to
				// heartbeat; it still stays in the loop so nodes registered later
				// are picked up.
				if len(rn.peerList()) > 0 {
					rn.SendHeartbeat()
				}
				select {
				case <-rn.stopCh:
					return
				case <-time.After(HEARTBEAT_PERIOD):
				}
				continue
			}
			timer := time.NewTimer(RandomDuration())
			select {
			case <-timer.C:
				rn.mux.Lock()
				rn.state = CANDIDATE
				rn.mux.Unlock()
				rn.StartElection()
			case <-rn.stateChan:
				timer.Stop()
			case <-rn.stopCh:
				timer.Stop()
				return
			}
		}
	}()
}

// Stop asks the background election/heartbeat loop to exit. It is safe to call
// multiple times.
func (rn *RaftNode) Stop() {
	rn.stopOnce.Do(func() { close(rn.stopCh) })
}

func (rn *RaftNode) Start() {
	mux := http.NewServeMux()
	rn.RegisterRoutes(mux)
	rn.Run()
	fmt.Println("Raft 节点启动完成")
	if err := http.ListenAndServe(":"+DEFAULTPORT, mux); err != nil {
		panic(err)
	}
}
