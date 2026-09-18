package raft

import (
	"errors"
	"testing"
)

// newFollower builds a follower with a known term/log so appendEntries and
// handleVote can be exercised without any network.
func newFollower(term int, log []LogEntry, commit int) *RaftNode {
	n := InitRaftNode("127.0.0.1", []string{"127.0.0.2:11451"})
	n.mux.Lock()
	n.state = FOLLOWER
	n.Term = term
	n.Log = log
	n.CommitIndex = commit
	n.LastApplied = commit
	n.mux.Unlock()
	return n
}

func cmd(op string) Command { return Command{OpType: op} }

func TestAppendEntriesCommitsAndApplies(t *testing.T) {
	n := newFollower(1, nil, -1)
	var applied []string
	n.SetApplyFunc(func(c Command) error {
		applied = append(applied, c.OpType)
		return nil
	})

	resp := n.appendEntries(AppendEntriesReq{
		Term:         1,
		LeaderIP:     "127.0.0.2",
		PrevLogIndex: -1,
		Entries:      []LogEntry{{Term: 1, Comm: cmd("a")}, {Term: 1, Comm: cmd("b")}},
		LeaderCommit: 1,
	})

	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	if len(n.Log) != 2 || n.CommitIndex != 1 {
		t.Fatalf("log=%d commit=%d, want log=2 commit=1", len(n.Log), n.CommitIndex)
	}
	if len(applied) != 2 || applied[0] != "a" || applied[1] != "b" {
		t.Fatalf("applied = %v, want [a b]", applied)
	}
}

func TestAppendEntriesRejectsStaleTerm(t *testing.T) {
	n := newFollower(5, nil, -1)
	resp := n.appendEntries(AppendEntriesReq{Term: 4, LeaderIP: "x"})
	if resp.Success || resp.Term != 5 {
		t.Fatalf("stale term accepted: %+v", resp)
	}
}

func TestAppendEntriesRejectsMissingPrev(t *testing.T) {
	n := newFollower(1, nil, -1)
	resp := n.appendEntries(AppendEntriesReq{Term: 1, LeaderIP: "x", PrevLogIndex: 0, PrevLogTerm: 1})
	if resp.Success {
		t.Fatal("expected rejection when prev index is missing")
	}
}

func TestAppendEntriesRejectsTermMismatch(t *testing.T) {
	n := newFollower(1, []LogEntry{{Term: 1, Comm: cmd("a")}}, 0)
	resp := n.appendEntries(AppendEntriesReq{
		Term: 1, LeaderIP: "x", PrevLogIndex: 0, PrevLogTerm: 9,
		Entries: []LogEntry{{Term: 1, Comm: cmd("b")}},
	})
	if resp.Success {
		t.Fatal("expected rejection on prev term mismatch")
	}
	if len(n.Log) != 1 || n.Log[0].Comm.OpType != "a" {
		t.Fatalf("log changed on rejected request: %+v", n.Log)
	}
}

func TestAppendEntriesTruncatesConflict(t *testing.T) {
	n := newFollower(1, []LogEntry{
		{Term: 1, Comm: cmd("a")},
		{Term: 1, Comm: cmd("b")},
	}, 1)
	resp := n.appendEntries(AppendEntriesReq{
		Term: 1, LeaderIP: "x", PrevLogIndex: 0, PrevLogTerm: 1,
		Entries: []LogEntry{{Term: 2, Comm: cmd("c")}},
	})
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	if len(n.Log) != 2 || n.Log[1].Comm.OpType != "c" || n.Log[1].Term != 2 {
		t.Fatalf("conflicting suffix not replaced: %+v", n.Log)
	}
}

func TestAppendEntriesIsIdempotent(t *testing.T) {
	n := newFollower(1, []LogEntry{{Term: 1, Comm: cmd("a")}}, 0)
	req := AppendEntriesReq{
		Term: 1, LeaderIP: "x", PrevLogIndex: 0, PrevLogTerm: 1,
		Entries: []LogEntry{{Term: 1, Comm: cmd("b")}},
	}
	n.appendEntries(req)
	n.appendEntries(req) // replayed request must not duplicate
	if len(n.Log) != 2 {
		t.Fatalf("log duplicated: %+v", n.Log)
	}
}

func TestHandleVote(t *testing.T) {
	n := newFollower(1, []LogEntry{{Term: 1, Comm: cmd("a")}}, 0)

	// Stale term: rejected.
	if resp := n.handleVote(ElectionReq{Term: 0, CandidateIP: "x", LastLogIndex: 99, LastLogTerm: 99}); resp.VoteGranted {
		t.Fatal("granted vote for a stale term")
	}
	// Stale log: rejected (candidate log term is older than ours).
	if resp := n.handleVote(ElectionReq{Term: 1, CandidateIP: "x", LastLogIndex: 0, LastLogTerm: 0}); resp.VoteGranted {
		t.Fatal("granted vote to a candidate with a stale log")
	}
	// The first same-term vote is granted.
	if resp := n.handleVote(ElectionReq{Term: 2, CandidateIP: "x", LastLogIndex: 1, LastLogTerm: 1}); !resp.VoteGranted {
		t.Fatalf("expected vote granted, got %+v", resp)
	}
	// A different candidate in the same term is rejected.
	if resp := n.handleVote(ElectionReq{Term: 2, CandidateIP: "y", LastLogIndex: 5, LastLogTerm: 5}); resp.VoteGranted {
		t.Fatal("granted a second vote in the same term")
	}
	// A log that is behind must never win a vote.
	n2 := newFollower(1, []LogEntry{{Term: 2, Comm: cmd("a")}}, 0)
	if resp := n2.handleVote(ElectionReq{Term: 2, CandidateIP: "x", LastLogIndex: 0, LastLogTerm: 1}); resp.VoteGranted {
		t.Fatal("granted vote to a candidate with a stale log")
	}
}

func TestApplyRejectsWhenNotLeader(t *testing.T) {
	n := newFollower(1, nil, -1)
	n.mux.Lock()
	n.LeaderIP = "127.0.0.9"
	n.mux.Unlock()

	_, err := n.Apply(cmd("x"))
	var notLeader *NotLeaderError
	if !errors.As(err, &notLeader) {
		t.Fatalf("error = %v, want *NotLeaderError", err)
	}
	if notLeader.Leader != "127.0.0.9" {
		t.Fatalf("leader = %q, want 127.0.0.9", notLeader.Leader)
	}
}

func TestApplyWithoutQuorumDoesNotCommit(t *testing.T) {
	// Two-node cluster whose only peer is unreachable (connection refused):
	// there is no majority, so the entry must not be committed or applied.
	n := InitRaftNode("127.0.0.1", []string{"127.0.0.1:1"})
	n.mux.Lock()
	n.becomeLeaderLocked()
	n.mux.Unlock()

	applied := 0
	n.SetApplyFunc(func(Command) error { applied++; return nil })

	if _, err := n.Apply(cmd("x")); err == nil {
		t.Fatal("expected quorum error")
	}
	if applied != 0 {
		t.Fatalf("entry applied without quorum: applied=%d", applied)
	}
	if n.CommitIndex != -1 {
		t.Fatalf("commit index advanced without quorum: %d", n.CommitIndex)
	}
}
