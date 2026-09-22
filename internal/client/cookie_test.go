package client

import (
	"testing"
	"time"
)

func TestGetNextHealthyCookie_PrioritizesAuthenticated(t *testing.T) {
	pool.mu.Lock()
	origAccounts := pool.Accounts
	origIndex := pool.index
	origHealth := pool.health

	pool.health = make(map[string]*accountHealth)
	pool.Accounts = []AccountCookie{
		{ID: "Guest-1", Cookie: "SAPISID=123; guest=true"},
		{ID: "Auth-1", Cookie: "SAPISID=456; auth=true"},
	}
	pool.index = 0

	// Mock tokenCache
	tokenCache.mu.Lock()
	if tokenCache.Caches == nil {
		tokenCache.Caches = make(map[string]TokenEntry)
	}
	now := time.Now()
	tokenCache.Caches["SAPISID=123; guest=true"] = TokenEntry{IsAuth: false, XsrfToken: "guest-token", Ts: now}
	tokenCache.Caches["SAPISID=456; auth=true"] = TokenEntry{IsAuth: true, XsrfToken: "auth-token", Ts: now}
	tokenCache.mu.Unlock()

	pool.mu.Unlock()

	defer func() {
		pool.mu.Lock()
		pool.Accounts = origAccounts
		pool.index = origIndex
		pool.health = origHealth
		pool.mu.Unlock()
	}()

	// Multiple calls should consistently pick Auth-1, not Guest-1
		for i := 0; i < 5; i++ {
			_, _, id, _ := GetNextHealthyCookie()
			if id != "Auth-1" {
				t.Fatalf("Iteration %d: GetNextHealthyCookie chose %s instead of authenticated account Auth-1", i, id)
			}
		}
}

func TestGetEffectivePoolRole_40_60(t *testing.T) {
	// For 10 accounts: 0..3 are session (4), 4..9 are worker (6)
	total := 10
	sessionCount := 0
	workerCount := 0
	for i := 0; i < total; i++ {
		role := GetEffectivePoolRole(i, total, "")
		if role == PoolTypeSession {
			sessionCount++
		} else if role == PoolTypeWorker {
			workerCount++
		}
	}
	if sessionCount != 4 || workerCount != 6 {
		t.Fatalf("Expected 4 session and 6 worker for total=10, got session=%d, worker=%d", sessionCount, workerCount)
	}

	// For 9 accounts: (9*4+5)/10 = 4 session, 5 worker
	total = 9
	sessionCount = 0
	workerCount = 0
	for i := 0; i < total; i++ {
		role := GetEffectivePoolRole(i, total, "")
		if role == PoolTypeSession {
			sessionCount++
		} else if role == PoolTypeWorker {
			workerCount++
		}
	}
	if sessionCount != 4 || workerCount != 5 {
		t.Fatalf("Expected 4 session and 5 worker for total=9, got session=%d, worker=%d", sessionCount, workerCount)
	}

	// For 6 accounts: (6*4+5)/10 = 2 session, 4 worker
	total = 6
	sessionCount = 0
	workerCount = 0
	for i := 0; i < total; i++ {
		role := GetEffectivePoolRole(i, total, "")
		if role == PoolTypeSession {
			sessionCount++
		} else if role == PoolTypeWorker {
			workerCount++
		}
	}
	if sessionCount != 2 || workerCount != 4 {
		t.Fatalf("Expected 2 session and 4 worker for total=6, got session=%d, worker=%d", sessionCount, workerCount)
	}

	// Manual override
	if role := GetEffectivePoolRole(0, 10, "worker"); role != PoolTypeWorker {
		t.Fatalf("Expected manual override 'worker', got %s", role)
	}
	if role := GetEffectivePoolRole(9, 10, "session"); role != PoolTypeSession {
		t.Fatalf("Expected manual override 'session', got %s", role)
	}
}

func TestDynamicBorrowing_WhenOverloaded(t *testing.T) {
	pool.mu.Lock()
	origAccounts := pool.Accounts
	origIndex := pool.index
	origHealth := pool.health

	pool.health = make(map[string]*accountHealth)
	// 5 accounts: 0,1 are session (2), 2,3,4 are worker (3)
	pool.Accounts = []AccountCookie{
		{ID: "Session-1", Cookie: "cookie-s1"},
		{ID: "Session-2", Cookie: "cookie-s2"},
		{ID: "Worker-1", Cookie: "cookie-w1"},
		{ID: "Worker-2", Cookie: "cookie-w2"},
		{ID: "Worker-3", Cookie: "cookie-w3"},
	}
	pool.index = 0
	pool.mu.Unlock()

	defer func() {
		pool.mu.Lock()
		pool.Accounts = origAccounts
		pool.index = origIndex
		pool.health = origHealth
		pool.mu.Unlock()
	}()

	// Normal case: Worker request gets Worker account
	_, _, idWorker, _ := GetNextCookieForType(PoolTypeWorker)
	if idWorker != "Worker-1" && idWorker != "Worker-2" && idWorker != "Worker-3" {
		t.Fatalf("Expected worker account, got %s", idWorker)
	}

	// Normal case: Session request gets Session account
	_, _, idSession, _ := GetNextCookieForType(PoolTypeSession)
	if idSession != "Session-1" && idSession != "Session-2" {
		t.Fatalf("Expected session account, got %s", idSession)
	}

	// Case 2: Overload - All Session accounts in cooldown
	MarkCooldown("Session-1", 1*time.Minute)
	MarkCooldown("Session-2", 1*time.Minute)

	// Session request should dynamically borrow from Worker pool!
	_, _, borrowedId, _ := GetNextCookieForType(PoolTypeSession)
	if borrowedId != "Worker-1" && borrowedId != "Worker-2" && borrowedId != "Worker-3" {
		t.Fatalf("Expected session request to borrow from worker pool, got %s", borrowedId)
	}

	// Case 3: Overload - All Worker accounts in cooldown, Session accounts restored
	MarkHealthy("Session-1")
	MarkHealthy("Session-2")
	MarkCooldown("Worker-1", 1*time.Minute)
	MarkCooldown("Worker-2", 1*time.Minute)
	MarkCooldown("Worker-3", 1*time.Minute)

	// Worker request should dynamically borrow from Session pool!
	_, _, borrowedWorkerId, _ := GetNextCookieForType(PoolTypeWorker)
	if borrowedWorkerId != "Session-1" && borrowedWorkerId != "Session-2" {
		t.Fatalf("Expected worker request to borrow from session pool, got %s", borrowedWorkerId)
	}
}
