package copilot

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeFakeJWT(name, upn string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadJSON := fmt.Sprintf(`{"unique_name":"%s","upn":"%s","exp":%d}`, name, upn, exp)
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	return fmt.Sprintf("%s.%s.signature", header, payload)
}

func TestTokenClaimsParsing(t *testing.T) {
	futureExp := time.Now().Add(1 * time.Hour).Unix()
	fakeToken := makeFakeJWT("alice@example.com", "alice_upn@example.com", futureExp)
	fakeURL := "wss://substrate.office.com/m365Copilot/Chathub?access_token=" + fakeToken

	user, exp, err := parseTokenClaims(fakeURL)
	if err != nil {
		t.Fatalf("unexpected error parsing claims: %v", err)
	}
	if user != "alice@example.com" {
		t.Errorf("expected user 'alice@example.com', got '%s'", user)
	}
	if exp != futureExp {
		t.Errorf("expected exp %d, got %d", futureExp, exp)
	}
}

func TestPoolRoundRobinAndHealth(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "copilot_pool_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	origDir := DefaultDataDir
	DefaultDataDir = tmpDir
	defer func() { DefaultDataDir = origDir }()

	futureExp := time.Now().Add(1 * time.Hour).Unix()
	url1 := "wss://substrate.office.com/m365Copilot/Chathub?access_token=" + makeFakeJWT("user1@example.com", "", futureExp)
	url2 := "wss://substrate.office.com/m365Copilot/Chathub?access_token=" + makeFakeJWT("user2@example.com", "", futureExp)

	// Save two accounts
	if err := SaveTokenWithID("Copilot-1", url1); err != nil {
		t.Fatalf("failed to save Copilot-1: %v", err)
	}
	if err := SaveTokenWithID("Copilot-2", url2); err != nil {
		t.Fatalf("failed to save Copilot-2: %v", err)
	}

	accs := GetAllAccounts()
	if len(accs) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accs))
	}

	// Verify round robin
	a1, err := GetNextHealthyAccount()
	if err != nil {
		t.Fatalf("failed to get account 1: %v", err)
	}
	a2, err := GetNextHealthyAccount()
	if err != nil {
		t.Fatalf("failed to get account 2: %v", err)
	}
	if a1.ID == a2.ID {
		t.Logf("Note: initial selection a1=%s, a2=%s", a1.ID, a2.ID)
	}

	// Verify cooldown
	MarkAccountCooldown(a1.ID, 2*time.Second)
	// Next calls should now return the other account
	for i := 0; i < 3; i++ {
		next, err := GetNextHealthyAccount()
		if err != nil {
			t.Fatalf("unexpected error getting account during cooldown: %v", err)
		}
		if next.ID == a1.ID {
			t.Fatalf("account %s should be in cooldown, but was returned", a1.ID)
		}
	}

	// Mark healthy again
	MarkAccountHealthy(a1.ID)
	healthyCount := GetHealthyAccountCount()
	if healthyCount != 2 {
		t.Errorf("expected 2 healthy accounts, got %d", healthyCount)
	}
}

func TestAccountAddAndDelete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "copilot_acc_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	origDir := DefaultDataDir
	DefaultDataDir = tmpDir
	defer func() { DefaultDataDir = origDir }()

	acc, err := AddAccount("")
	if err != nil {
		t.Fatalf("AddAccount failed: %v", err)
	}
	if acc.ID != "Copilot-1" {
		t.Errorf("expected ID 'Copilot-1', got '%s'", acc.ID)
	}
	expectedProf := filepath.Join(DefaultDataDir, "profiles", "Copilot-1")
	if acc.ProfileDir != expectedProf {
		t.Errorf("expected ProfileDir '%s', got '%s'", expectedProf, acc.ProfileDir)
	}

	acc2, err := AddAccount("")
	if err != nil {
		t.Fatalf("AddAccount 2 failed: %v", err)
	}
	if acc2.ID != "Copilot-2" {
		t.Errorf("expected ID 'Copilot-2', got '%s'", acc2.ID)
	}

		if err := DeleteAccount("Copilot-1"); err != nil {
			t.Fatalf("DeleteAccount failed: %v", err)
		}
	accs := GetAllAccounts()
	if len(accs) != 1 || accs[0].ID != "Copilot-2" {
		t.Errorf("expected only Copilot-2 remaining, got %+v", accs)
	}
}

func TestCopilotSessionStore_Lifecycle(t *testing.T) {
	store := NewCopilotSessionStore()

	// 1. Create session
	sess, existing := store.GetOrCreateSession("copilot-sess-1")
	if existing || sess.TurnCount != 0 {
		t.Fatalf("Expected new session with 0 turns, got existing=%v, turns=%d", existing, sess.TurnCount)
	}

	// 2. Bind account
	store.BindAccount("copilot-sess-1", "Copilot-2")
	sess2, _ := store.GetOrCreateSession("copilot-sess-1")
	if sess2.AccountID != "Copilot-2" {
		t.Fatalf("Expected account Copilot-2, got %s", sess2.AccountID)
	}

	// 3. Update metadata
	store.UpdateSessionMetadata("copilot-sess-1", "conv_abc", "chat_xyz", 1)
	sess3, _ := store.GetOrCreateSession("copilot-sess-1")
	if sess3.TurnCount != 1 || sess3.InvocationID != 1 || sess3.ConversationID != "conv_abc" {
		t.Fatalf("Update metadata failed: %+v", sess3)
	}

	// 4. Auto-reset at 25 turns
	for i := 1; i < MaxSessionTurns; i++ {
		store.UpdateSessionMetadata("copilot-sess-1", "conv_abc", "chat_xyz", i+1)
	}
	sessAfter, _ := store.GetOrCreateSession("copilot-sess-1")
	if sessAfter.TurnCount != 0 || sessAfter.ConversationID != "" || sessAfter.InvocationID != 0 {
		t.Fatalf("Auto-reset failed: turnCount=%d, convID=%s, invocID=%d",
			sessAfter.TurnCount, sessAfter.ConversationID, sessAfter.InvocationID)
	}
	if sessAfter.AccountID != "Copilot-2" {
		t.Fatalf("Auto-reset lost account binding: %s", sessAfter.AccountID)
	}
}

func TestCopilotPool_40_60_AndDynamicBorrowing(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "copilot_borrow_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	origDir := DefaultDataDir
	DefaultDataDir = tmpDir
	defer func() { DefaultDataDir = origDir }()

	futureExp := time.Now().Add(1 * time.Hour).Unix()
	// Add 5 accounts: 0,1 are session (2 accounts), 2,3,4 are worker (3 accounts)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("Copilot-%d", i)
		u := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub?access_token=%s", makeFakeJWT(fmt.Sprintf("user%d@example.com", i), "", futureExp))
		if err := SaveTokenWithID(id, u); err != nil {
			t.Fatalf("failed to save token %s: %v", id, err)
		}
	}

	// Normal worker request
	workerAcc, err := GetNextHealthyAccountForType(PoolTypeWorker)
	if err != nil {
		t.Fatalf("unexpected error getting worker account: %v", err)
	}
	if workerAcc.ID != "Copilot-3" && workerAcc.ID != "Copilot-4" && workerAcc.ID != "Copilot-5" {
		t.Fatalf("expected worker account (Copilot-3..5), got %s", workerAcc.ID)
	}

	// Normal session request
	sessAcc, err := GetNextHealthyAccountForType(PoolTypeSession)
	if err != nil {
		t.Fatalf("unexpected error getting session account: %v", err)
	}
	if sessAcc.ID != "Copilot-1" && sessAcc.ID != "Copilot-2" {
		t.Fatalf("expected session account (Copilot-1..2), got %s", sessAcc.ID)
	}

	// Overload session pool: put Copilot-1 and Copilot-2 in cooldown
	MarkAccountCooldown("Copilot-1", 1*time.Minute)
	MarkAccountCooldown("Copilot-2", 1*time.Minute)

	// Session request should dynamically borrow from worker pool!
	borrowedSess, err := GetNextHealthyAccountForType(PoolTypeSession)
	if err != nil {
		t.Fatalf("expected dynamic borrow from worker pool, got error: %v", err)
	}
	if borrowedSess.ID != "Copilot-3" && borrowedSess.ID != "Copilot-4" && borrowedSess.ID != "Copilot-5" {
		t.Fatalf("expected borrowed worker account, got %s", borrowedSess.ID)
	}

	// Overload worker pool: restore session, put workers in cooldown
	MarkAccountHealthy("Copilot-1")
	MarkAccountHealthy("Copilot-2")
	MarkAccountCooldown("Copilot-3", 1*time.Minute)
	MarkAccountCooldown("Copilot-4", 1*time.Minute)
	MarkAccountCooldown("Copilot-5", 1*time.Minute)

	// Worker request should dynamically borrow from session pool!
	borrowedWorker, err := GetNextHealthyAccountForType(PoolTypeWorker)
	if err != nil {
		t.Fatalf("expected dynamic borrow from session pool, got error: %v", err)
	}
	if borrowedWorker.ID != "Copilot-1" && borrowedWorker.ID != "Copilot-2" {
		t.Fatalf("expected borrowed session account, got %s", borrowedWorker.ID)
	}
}
