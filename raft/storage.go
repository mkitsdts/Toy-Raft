package raft

// LogStorage persists Raft log entries. It decouples the Raft algorithm from
// the concrete storage backend (SQLite in this project) so no JSON file is
// needed.
type LogStorage interface {
	// AppendLogEntries persists entries whose absolute Raft index starts at
	// startIndex. Persisting an index that already exists overwrites it, so
	// calls are idempotent.
	AppendLogEntries(startIndex int, entries []LogEntry) error
	// LoadLogEntries returns every persisted entry ordered by ascending index.
	LoadLogEntries() ([]LogEntry, error)
	// TruncateLogEntries removes every persisted entry whose absolute Raft
	// index is >= fromIndex. It is called when a conflicting suffix is dropped
	// because the leader's log diverged, so those entries cannot resurface
	// after a restart.
	TruncateLogEntries(fromIndex int) error
}
