package stats

import (
	"sync"
	"time"
)

type TrafficStats struct {
	mu            sync.RWMutex
	TotalRequests int64  `json:"total_requests"`
	TotalTokens   int64  `json:"total_tokens"`
	LastRequestAt string `json:"last_request_at"`
}

var STATS TrafficStats

func UpdateStats(tokens int64) {
	STATS.mu.Lock()
	defer STATS.mu.Unlock()
	STATS.TotalRequests++
	STATS.TotalTokens += tokens
	STATS.LastRequestAt = time.Now().Format("2006-01-02 15:04:05")
}

func GetStats() (int64, int64, string) {
	STATS.mu.RLock()
	defer STATS.mu.RUnlock()
	return STATS.TotalRequests, STATS.TotalTokens, STATS.LastRequestAt
}
