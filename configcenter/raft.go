package configcenter

import (
	"fmt"

	"nexus/raft"
)

// NewRaftNode builds the Raft node for this configuration center. The center
// contributes exactly two things to the Raft library: the state-machine
// ApplyFunc and its RollbackFunc. Everything else — persisting and restoring
// the log, replicating entries, electing a leader, committing and applying in
// log order — is handled inside the Raft library.
//
// The apply callback additionally refreshes the node's peer list whenever a
// membership command is applied, so every node (not just the one that received
// the HTTP request) tracks the cluster membership without any extra wiring in
// the caller.
func NewNode(store *Store, localIP string, peers []string) (*raft.RaftNode, error) {
	var node *raft.RaftNode
	node, err := raft.New(raft.Config{
		LocalIP: localIP,
		Peers:   peers,
		Storage: store,
		Apply: func(cmd raft.Command) error {
			if err := store.ApplyCommand(cmd); err != nil {
				return err
			}
			return syncRaftPeers(store, node, cmd)
		},
		Rollback: store.RollbackCommand,
	})
	if err != nil {
		return nil, fmt.Errorf("build raft node: %w", err)
	}
	return node, nil
}

// syncRaftPeers pushes the current node registry to the Raft node after a
// membership command, so peer changes take effect on every node that applies
// the command.
func syncRaftPeers(store *Store, node *raft.RaftNode, cmd raft.Command) error {
	if node == nil {
		return nil
	}
	if cmd.OpType != OpNodeRegister && cmd.OpType != OpNodeDelete {
		return nil
	}
	nodes, err := store.ListNodes()
	if err != nil {
		return err
	}
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addrs = append(addrs, n.Address())
	}
	node.SetNodes(addrs)
	return nil
}

// RollbackCommand is the compensation half of the Raft state machine. Every
// ApplyCommand runs inside a database transaction, so a failed apply leaves no
// partial write to undo; the callback therefore only reports the command. It is
// registered explicitly so the configuration center owns the RollbackFunc
// contract and can add real compensation here if a future command stops being
// atomic.
func (s *Store) RollbackCommand(cmd raft.Command) error {
	fmt.Printf("raft: 回滚命令 %s\n", cmd.OpType)
	return nil
}
