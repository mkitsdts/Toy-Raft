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
}
