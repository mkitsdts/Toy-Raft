package raft

import (
	"math/rand"
	"time"
)

// 生成随机数时间间隔
func RandomDuration() time.Duration {
	// Convert duration difference to milliseconds for rand.Intn
	durationDiffMs := int(MAX_DURATION-MIN_DURATION) / int(time.Millisecond)
	// Generate random milliseconds and convert back to duration
	return MIN_DURATION + time.Duration(rand.Intn(durationDiffMs))*time.Millisecond
}
