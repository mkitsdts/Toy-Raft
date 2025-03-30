package raft

import "time"

const (
	FOLLOWER  uint8 = 0
	CANDIDATE uint8 = 1
	LEADER    uint8 = 2

	TERM_MIN = 0

	CONNECT_MAX_TIME  = 5 * time.Second
	HEARTBEAT_TIMEOUT = 1000 * time.Millisecond
	HEARTBEAT_PERIOD  = 500 * time.Millisecond

	DEFAULTPORT = "11451"

	LOG_PATH         = "log.json"
	RAFT_CONFIG_PATH = "raft.json"
)

const (
	MIN_DURATION = 2 * time.Second
	MAX_DURATION = 3 * time.Second
)

type Command struct {
	OpType string `json:"op_type"`
	SQL    string `json:"sql"`
	Value  []byte `json:"value"`
	DBName string `json:"db_name"`
}

type LogEntry struct {
	Term int     `json:"term"`
	Comm Command `json:"command"` // 日志条目
}

type ElectionReq struct {
	Term         int    `json:"term"`
	CandidateIP  string `json:"candidate_ip"`
	LastLogIndex int    `json:"last_log_index"`
	LastLogTerm  int    `json:"last_log_term"`
}

type ElectionResp struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"vote_granted"`
}

type AppendEntriesReq struct {
	Term         int        `json:"term"`
	LeaderIP     string     `json:"leader_ip"`
	PrevLogIndex int        `json:"prev_log_index"`
	PrevLogTerm  int        `json:"prev_log_term"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leader_commit"`
	LastApplied  int        `json:"last_applied"`
}

type AppendEntriesResp struct {
	Term    int  `json:"term"`
	Success bool `json:"success"`
}
