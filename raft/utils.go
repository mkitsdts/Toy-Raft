package raft

import (
	"fmt"
	"math/rand"
	"time"
)

const (
	MIN_DURATION = 2 * time.Second
	MAX_DURATION = 3 * time.Second
)

// 生成随机数时间间隔
func RandomDuration() time.Duration {
	// Convert duration difference to milliseconds for rand.Intn
	durationDiffMs := int(MAX_DURATION-MIN_DURATION) / int(time.Millisecond)
	// Generate random milliseconds and convert back to duration
	return MIN_DURATION + time.Duration(rand.Intn(durationDiffMs))*time.Millisecond
}

// 保存日志到数据库：只要有日志就整份写入，不关心提交/应用进度。
// 存储层按索引 upsert，所以重复写入是幂等的。
// 调用方需保证已持有（或无需持有）rn.mux，本函数自身不加锁。
func PersisLog(rn *RaftNode) {
	if rn.storage == nil {
		fmt.Println("未配置日志存储，跳过持久化")
		return
	}
	if len(rn.Log) == 0 {
		fmt.Println("没有需要保存的日志")
		return
	}

	entries := make([]LogEntry, len(rn.Log))
	copy(entries, rn.Log)

	fmt.Println("正在写入日志至数据库...")
	if err := rn.storage.AppendLogEntries(0, entries); err != nil {
		fmt.Println("写入日志失败：", err)
		return
	}
	fmt.Println("写入日志成功，当前日志数量：", len(entries))
}
