package raft

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	FOLLOWER  uint8 = 0
	CANDIDATE uint8 = 1
	LEADER    uint8 = 2
)

var ErrNotLeader = errors.New("not raft leader")

type ApplyFunc func(Command) error

// RaftNode is a small Raft node used by the configuration center. All mutable
// application data is represented by Command values and is applied only after
// a majority has acknowledged its log entry.
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
	stateChan   chan struct{}
	stopCh      chan struct{}
	stopOnce    sync.Once
	apply       ApplyFunc
	storage     LogStorage
}

func InitRaftNode(localIP string, nodeIPs []string) *RaftNode {
	n := &RaftNode{
		state: FOLLOWER, Term: TERM_MIN, LocalIP: localIP, NodeIP: nodeIPs,
		CommitIndex: -1, LastApplied: -1, stateChan: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		Log:    make([]LogEntry, 0),
	}
	// A one-node cluster has a majority immediately.
	if len(nodeIPs) == 0 {
		n.state = LEADER
		n.LeaderIP = localIP
	}
	return n
}

func (rn *RaftNode) SetApplyFunc(fn ApplyFunc) { rn.mux.Lock(); rn.apply = fn; rn.mux.Unlock() }

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
	return map[string]any{"state": states[rn.state], "term": rn.Term, "leader": rn.LeaderIP, "local_ip": rn.LocalIP, "commit_index": rn.CommitIndex}
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

func (rn *RaftNode) Run() {
	if len(rn.NodeIP) == 0 {
		return
	}
	go func() {
		for {
			rn.mux.RLock()
			state := rn.state
			rn.mux.RUnlock()
			if state == LEADER {
				rn.SendHeartbeat()
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
