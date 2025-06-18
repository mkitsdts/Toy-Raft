package raft

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
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

// 暂时不能保存大量数据
// 保存日志到磁盘
func PersisLog(rn *RaftNode) {

	if rn.CommitIndex-rn.LastApplied <= 0 {
		fmt.Println("没有需要保存的日志")
		return
	}

	// 判断文件是否存在
	_, err := os.Stat(LOG_PATH)
	if err != nil {
		// 创建文件
		file, err := os.Create(LOG_PATH)
		if err != nil {
			return
		}
		defer file.Close()
	}
	// 打开文件
	file, err := os.OpenFile(LOG_PATH, os.O_WRONLY, 0666)
	if err != nil {
		return
	}
	defer file.Close()

	var logs []LogEntry
	// 读取日志
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&logs)
	if err != nil {
		return
	}
	fmt.Println("读取日志成功，当前日志数量：", len(logs))
	fmt.Println("正在写入日志至磁盘...")
	// 写入日志
	for i := rn.LastApplied; i < rn.CommitIndex; i++ {
		logs = append(logs, rn.Log[i])
	}
	fmt.Println("写入日志成功，当前日志数量：", len(logs))
	// 保存日志
	encoder := json.NewEncoder(file)
	err = encoder.Encode(logs)
	if err != nil {
		return
	}
}
