package raft

import (
	"math/rand"
	"net"
	"strings"
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

// PeerAddress normalizes a peer node address. Addresses that already contain a
// port ("host:port", "[::1]:port") are returned unchanged; otherwise the
// default Raft port is appended.
func PeerAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	addr = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	return net.JoinHostPort(addr, DEFAULTPORT)
}

// peerURL builds the request URL for a peer endpoint, tolerating addresses
// without an explicit port.
func peerURL(addr, path string) string {
	return "http://" + PeerAddress(addr) + path
}
