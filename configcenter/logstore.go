package configcenter

import (
	"time"

	"gorm.io/gorm/clause"

	"nexus/raft"
)

// RaftLogEntry is the SQLite representation of a Raft log entry. LogIndex is
// the entry's absolute position in the Raft log and is used as the primary key
// so re-persisting the same entry is idempotent. The "index" name is avoided
// because it is an SQL keyword.
type RaftLogEntry struct {
	LogIndex  int    `gorm:"primaryKey;autoIncrement:false;column:log_index"`
	Term      int    `gorm:"not null"`
	OpType    string `gorm:"size:64"`
	SQL       string `gorm:"type:text"`
	Value     []byte `gorm:"type:blob"`
	DBName    string `gorm:"size:255"`
	CreatedAt time.Time
}

// AppendLogEntries implements raft.LogStorage by upserting the given entries
// into the raft_log_entries table.
func (s *Store) AppendLogEntries(startIndex int, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	records := make([]RaftLogEntry, 0, len(entries))
	for i, entry := range entries {
		records = append(records, RaftLogEntry{
			LogIndex: startIndex + i,
			Term:     entry.Term,
			OpType:   entry.Comm.OpType,
			SQL:      entry.Comm.SQL,
			Value:    entry.Comm.Value,
			DBName:   entry.Comm.DBName,
		})
	}
	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&records).Error
}

// LoadLogEntries implements raft.LogStorage, restoring the log on startup.
func (s *Store) LoadLogEntries() ([]raft.LogEntry, error) {
	var records []RaftLogEntry
	if err := s.db.Order("log_index asc").Find(&records).Error; err != nil {
		return nil, err
	}
	entries := make([]raft.LogEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, raft.LogEntry{
			Term: record.Term,
			Comm: raft.Command{
				OpType: record.OpType,
				SQL:    record.SQL,
				Value:  record.Value,
				DBName: record.DBName,
			},
		})
	}
	return entries, nil
}
