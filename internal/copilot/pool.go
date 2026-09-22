package copilot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	PoolTypeSession = "session"
	PoolTypeWorker  = "worker"
	PoolTypeAuto    = "auto"
)

var (
	DefaultDataDir = "data/copilot"
	poolMu         sync.RWMutex
	roundRobinIdx  uint64
)

type accountHealth struct {
	cooldownUntil time.Time
	failCount     int
}

var healthTracker = make(map[string]*accountHealth)

type CopilotAccount struct {
	ID                 string `json:"id"`
	User               string `json:"user"`
	TokenURL           string `json:"token_url"`
	ProfileDir         string `json:"profile_dir"`
	RemainingSeconds   int64  `json:"remaining_seconds"`
	RemainingFormatted string `json:"remaining_formatted"`
	Healthy            bool   `json:"healthy"`
	FailCount          int    `json:"fail_count"`
	CooldownUntil      string `json:"cooldown_until,omitempty"`
	Active             bool   `json:"active"`
	LastUpdated        int64  `json:"last_updated"`
	PoolType           string `json:"pool_type,omitempty"`       // "session" or "worker"
	ConfiguredRole     string `json:"configured_role,omitempty"` // "session", "worker", or "auto"
}

func init() {
	_ = os.MkdirAll(DefaultDataDir, 0755)
}

func parseTokenClaims(urlStr string) (user string, exp int64, err error) {
	if urlStr == "" {
		return "", 0, fmt.Errorf("empty url")
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", 0, err
	}
	token := u.Query().Get("access_token")
	if token == "" {
		return "", 0, fmt.Errorf("no access_token param")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", 0, fmt.Errorf("invalid jwt format")
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", 0, err
		}
	}
	var claims struct {
		Exp  int64  `json:"exp"`
		Upn  string `json:"upn"`
		Name string `json:"unique_name"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", 0, err
	}
	user = claims.Name
	if user == "" {
		user = claims.Upn
	}
	return user, claims.Exp, nil
}

func ensureMigratedLocked() {
	legacyProfile := filepath.Join(DefaultDataDir, "profile")
	targetProfile := filepath.Join(DefaultDataDir, "profiles", "Copilot-1")
	if fi, err := os.Stat(legacyProfile); err == nil && fi.IsDir() {
		if _, err := os.Stat(targetProfile); os.IsNotExist(err) {
			_ = os.MkdirAll(filepath.Dir(targetProfile), 0755)
			_ = os.Rename(legacyProfile, targetProfile)
		}
	}
}

func loadAccountsLocked() map[string]CopilotAccount {
	ensureMigratedLocked()
	accPath := filepath.Join(DefaultDataDir, "copilot_accounts.json")
	res := make(map[string]CopilotAccount)
	if data, err := os.ReadFile(accPath); err == nil {
		_ = json.Unmarshal(data, &res)
	}

	// Normalize profiles and IDs
	modified := false
	for k, acc := range res {
		if acc.ProfileDir == "" {
			acc.ProfileDir = filepath.Join(DefaultDataDir, "profiles", acc.ID)
			res[k] = acc
			modified = true
		}
	}

	// Backward compatibility: import from ws_token_url.txt if empty
	tokenPath := filepath.Join(DefaultDataDir, "ws_token_url.txt")
	if len(res) == 0 {
		if data, err := os.ReadFile(tokenPath); err == nil {
			uStr := strings.TrimSpace(string(data))
			if uStr != "" {
				usr, exp, err := parseTokenClaims(uStr)
				if err == nil {
					id := "Copilot-1"
					res[id] = CopilotAccount{
						ID:          id,
						User:        usr,
						TokenURL:    uStr,
						ProfileDir:  filepath.Join(DefaultDataDir, "profiles", id),
						LastUpdated: exp - 4800,
						Healthy:     true,
					}
					modified = true
				}
			}
		}
	}

	if modified {
		_ = saveAccountsLocked(res)
	}
	return res
}

func saveAccountsLocked(accs map[string]CopilotAccount) error {
	_ = os.MkdirAll(DefaultDataDir, 0755)
	accPath := filepath.Join(DefaultDataDir, "copilot_accounts.json")
	data, err := json.MarshalIndent(accs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(accPath, data, 0644)
}

func getHealthLocked(id string) *accountHealth {
	h, ok := healthTracker[id]
	if !ok {
		h = &accountHealth{}
		healthTracker[id] = h
	}
	return h
}

func isAccountHealthyLocked(id string) bool {
	h, ok := healthTracker[id]
	if !ok {
		return true
	}
	return time.Now().After(h.cooldownUntil)
}

func MarkAccountCooldown(id string, duration time.Duration) {
	poolMu.Lock()
	defer poolMu.Unlock()
	h := getHealthLocked(id)
	h.cooldownUntil = time.Now().Add(duration)
	h.failCount++
}

func MarkAccountHealthy(id string) {
	poolMu.Lock()
	defer poolMu.Unlock()
	h := getHealthLocked(id)
	h.cooldownUntil = time.Time{}
	h.failCount = 0
}

func GenerateNextAccountID() string {
	poolMu.RLock()
	defer poolMu.RUnlock()
	accs := loadAccountsLocked()
	maxIdx := 0
	for _, acc := range accs {
		var idx int
		if _, err := fmt.Sscanf(acc.ID, "Copilot-%d", &idx); err == nil {
			if idx > maxIdx {
				maxIdx = idx
			}
		}
	}
	return fmt.Sprintf("Copilot-%d", maxIdx+1)
}

func AddAccount(id string) (*CopilotAccount, error) {
	poolMu.Lock()
	defer poolMu.Unlock()

	accs := loadAccountsLocked()
	if id == "" {
		maxIdx := 0
		for _, acc := range accs {
			var idx int
			if _, err := fmt.Sscanf(acc.ID, "Copilot-%d", &idx); err == nil {
				if idx > maxIdx {
					maxIdx = idx
				}
			}
		}
		id = fmt.Sprintf("Copilot-%d", maxIdx+1)
	}

	if _, exists := accs[id]; exists {
		return nil, fmt.Errorf("account %s already exists", id)
	}

	profDir := filepath.Join(DefaultDataDir, "profiles", id)
	_ = os.MkdirAll(profDir, 0755)

	acc := CopilotAccount{
		ID:          id,
		User:        "Chưa đăng nhập",
		ProfileDir:  profDir,
		Healthy:     true,
		LastUpdated: time.Now().Unix(),
	}
	accs[id] = acc
	if err := saveAccountsLocked(accs); err != nil {
		return nil, err
	}
	return &acc, nil
}

func DeleteAccount(id string) error {
	poolMu.Lock()
	defer poolMu.Unlock()

	accs := loadAccountsLocked()
	acc, exists := accs[id]
	if !exists {
		return fmt.Errorf("account %s not found", id)
	}

	delete(accs, id)
	delete(healthTracker, id)
	_ = saveAccountsLocked(accs)

	if acc.ProfileDir != "" {
		_ = os.RemoveAll(acc.ProfileDir)
	}

	// Update active ws_token_url.txt
	now := time.Now().Unix()
	var nextURL string
	for _, a := range accs {
		_, exp, err := parseTokenClaims(a.TokenURL)
		if err == nil && exp > now {
			nextURL = a.TokenURL
			break
		}
	}
	tokenPath := filepath.Join(DefaultDataDir, "ws_token_url.txt")
	if nextURL != "" {
		_ = os.WriteFile(tokenPath, []byte(nextURL), 0644)
		cachedWsURL = nextURL
	} else {
		_ = os.Remove(tokenPath)
		cachedWsURL = ""
	}
	return nil
}

func SaveToken(urlStr string) error {
	return SaveTokenWithID("", urlStr)
}

func SaveTokenWithID(id, urlStr string) error {
	poolMu.Lock()
	defer poolMu.Unlock()

	urlStr = strings.TrimSpace(urlStr)
	user, exp, err := parseTokenClaims(urlStr)
	if err != nil {
		return fmt.Errorf("invalid token URL: %w", err)
	}

	accs := loadAccountsLocked()
	if id == "" {
		// Find matching account by user or id
		for existingID, acc := range accs {
			if acc.User == user || existingID == id {
				id = existingID
				break
			}
		}
		if id == "" {
			// Find first account with empty token
			for existingID, acc := range accs {
				if acc.TokenURL == "" {
					id = existingID
					break
				}
			}
		}
		if id == "" {
			maxIdx := 0
			for _, acc := range accs {
				var idx int
				if _, err := fmt.Sscanf(acc.ID, "Copilot-%d", &idx); err == nil {
					if idx > maxIdx {
						maxIdx = idx
					}
				}
			}
			id = fmt.Sprintf("Copilot-%d", maxIdx+1)
		}
	}

	profDir := filepath.Join(DefaultDataDir, "profiles", id)
	if existing, ok := accs[id]; ok && existing.ProfileDir != "" {
		profDir = existing.ProfileDir
	}
	_ = os.MkdirAll(profDir, 0755)

	accs[id] = CopilotAccount{
		ID:          id,
		User:        user,
		TokenURL:    urlStr,
		ProfileDir:  profDir,
		Healthy:     true,
		LastUpdated: time.Now().Unix(),
	}
	_ = saveAccountsLocked(accs)

	// Update cachedWsURL and ws_token_url.txt
	cachedWsURL = urlStr
	tokenPath := filepath.Join(DefaultDataDir, "ws_token_url.txt")
	_ = os.WriteFile(tokenPath, []byte(urlStr), 0644)

	// Reset health
	h := getHealthLocked(id)
	h.failCount = 0
	h.cooldownUntil = time.Time{}
	_ = exp
	return nil
}

func GetAllAccounts() []CopilotAccount {
	poolMu.RLock()
	defer poolMu.RUnlock()

	accs := loadAccountsLocked()
	var list []CopilotAccount
	now := time.Now().Unix()

	sortedIDs := make([]string, 0, len(accs))
	for id := range accs {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)
	total := len(sortedIDs)

	for i, id := range sortedIDs {
		acc := accs[id]
		h := getHealthLocked(acc.ID)
		acc.FailCount = h.failCount
		acc.Healthy = isAccountHealthyLocked(acc.ID)
		if !h.cooldownUntil.IsZero() && time.Now().Before(h.cooldownUntil) {
			acc.CooldownUntil = h.cooldownUntil.Format("15:04:05")
		}

		role := GetCopilotEffectivePoolRole(i, total, acc.PoolType)
		cfg := acc.PoolType
		if cfg == "" {
			cfg = PoolTypeAuto
		}
		acc.PoolType = role
		acc.ConfiguredRole = cfg

		if acc.TokenURL != "" {
			user, exp, err := parseTokenClaims(acc.TokenURL)
			if err == nil {
				if user != "" {
					acc.User = user
				}
				rem := exp - now
				acc.RemainingSeconds = rem
				if rem > 0 {
					acc.RemainingFormatted = fmt.Sprintf("%d phút %ds", rem/60, rem%60)
				} else {
					acc.RemainingFormatted = "Hết hạn"
					acc.Healthy = false
				}
			} else {
				acc.RemainingFormatted = "Token lỗi"
				acc.Healthy = false
			}
		} else {
			acc.RemainingFormatted = "Chưa có token"
			acc.Healthy = false
		}
		acc.Active = (acc.TokenURL != "" && acc.TokenURL == cachedWsURL)
		list = append(list, acc)
	}
	return list
}

func GetHealthyAccountCount() int {
	poolMu.RLock()
	defer poolMu.RUnlock()

	accs := loadAccountsLocked()
	now := time.Now().Unix()
	count := 0
	for _, acc := range accs {
		if acc.TokenURL == "" {
			continue
		}
		if !isAccountHealthyLocked(acc.ID) {
			continue
		}
		_, exp, err := parseTokenClaims(acc.TokenURL)
		if err == nil && exp-now > 60 {
			count++
		}
	}
	return count
}

func GetCopilotEffectivePoolRole(idx, total int, configuredRole string) string {
	switch strings.ToLower(strings.TrimSpace(configuredRole)) {
	case PoolTypeSession:
		return PoolTypeSession
	case PoolTypeWorker:
		return PoolTypeWorker
	default:
		if total <= 1 {
			return PoolTypeWorker
		}
		sessionCount := (total*4 + 5) / 10
		if sessionCount < 1 {
			sessionCount = 1
		}
		if sessionCount >= total {
			sessionCount = total - 1
		}
		if idx < sessionCount {
			return PoolTypeSession
		}
		return PoolTypeWorker
	}
}

func SetCopilotAccountPoolType(id, poolType string) error {
	poolMu.Lock()
	defer poolMu.Unlock()
	accs := loadAccountsLocked()
	acc, exists := accs[id]
	if !exists {
		return fmt.Errorf("account %s not found", id)
	}
	acc.PoolType = poolType
	accs[id] = acc
	return saveAccountsLocked(accs)
}

func GetCopilotAccountByID(id string) (*CopilotAccount, error) {
	poolMu.RLock()
	defer poolMu.RUnlock()
	accs := loadAccountsLocked()
	acc, exists := accs[id]
	if !exists {
		return nil, fmt.Errorf("account %s not found", id)
	}
	if !isAccountHealthyLocked(acc.ID) {
		return nil, fmt.Errorf("account %s is in cooldown", id)
	}
	now := time.Now().Unix()
	user, exp, err := parseTokenClaims(acc.TokenURL)
	if err != nil || exp-now <= 60 {
		return nil, fmt.Errorf("account %s token expired or invalid", id)
	}
	if user != "" {
		acc.User = user
	}
	acc.RemainingSeconds = exp - now
	acc.Healthy = true
	return &acc, nil
}

func GetNextHealthyAccount() (*CopilotAccount, error) {
	return GetNextHealthyAccountForType(PoolTypeWorker)
}

func GetNextHealthyAccountForType(requestedType string) (*CopilotAccount, error) {
	if requestedType == "" {
		requestedType = PoolTypeWorker
	}
	requestedType = strings.ToLower(strings.TrimSpace(requestedType))
	if requestedType != PoolTypeSession && requestedType != PoolTypeWorker {
		requestedType = PoolTypeWorker
	}

	poolMu.RLock()
	defer poolMu.RUnlock()

	accs := loadAccountsLocked()
	if len(accs) == 0 {
		return nil, fmt.Errorf("no copilot accounts configured")
	}

	sortedIDs := make([]string, 0, len(accs))
	for id := range accs {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	total := len(sortedIDs)
	now := time.Now().Unix()

	var primaryCandidates []CopilotAccount
	var oppositeCandidates []CopilotAccount

	oppositeType := PoolTypeWorker
	if requestedType == PoolTypeWorker {
		oppositeType = PoolTypeSession
	}

	for i, id := range sortedIDs {
		acc := accs[id]
		if acc.TokenURL == "" {
			continue
		}
		if !isAccountHealthyLocked(acc.ID) {
			continue
		}
		user, exp, err := parseTokenClaims(acc.TokenURL)
		if err != nil || exp-now <= 60 {
			continue
		}
		if user != "" {
			acc.User = user
		}
		acc.RemainingSeconds = exp - now
		acc.Healthy = true

		role := GetCopilotEffectivePoolRole(i, total, acc.PoolType)
		acc.PoolType = role

		if role == requestedType {
			primaryCandidates = append(primaryCandidates, acc)
		} else {
			oppositeCandidates = append(oppositeCandidates, acc)
		}
	}

	// 1. Primary candidates available
	if len(primaryCandidates) > 0 {
		idx := atomic.AddUint64(&roundRobinIdx, 1) - 1
		selected := primaryCandidates[int(idx)%len(primaryCandidates)]
		return &selected, nil
	}

	// 2. Overload detected! Dynamic Borrowing: borrow up to 2 accounts from opposite pool
	if len(oppositeCandidates) > 0 {
		borrowLimit := 2
		if len(oppositeCandidates) < borrowLimit {
			borrowLimit = len(oppositeCandidates)
		}
		borrowedCandidates := oppositeCandidates[:borrowLimit]
		idx := atomic.AddUint64(&roundRobinIdx, 1) - 1
		selected := borrowedCandidates[int(idx)%len(borrowedCandidates)]
		log.Printf("[CopilotPool] Dynamic Borrowing: %s pool overloaded/empty, borrowed account %s from %s pool (borrow pool size: %d)\n",
			requestedType, selected.ID, oppositeType, borrowLimit)
		return &selected, nil
	}

	return nil, fmt.Errorf("all copilot accounts in %s pool (and borrow pool) are exhausted, expired, or in cooldown", requestedType)
}

func GetActiveWsURL() (string, error) {
	acc, err := GetNextHealthyAccount()
	if err != nil {
		return "", err
	}
	cachedWsURL = acc.TokenURL
	return acc.TokenURL, nil
}

func GetTokenInfo() map[string]interface{} {
	accs := GetAllAccounts()
	activeAccountUser := "Chưa cấu hình"
	activeRemFormatted := "Hết hạn"
	activeRemSec := int64(0)
	isValid := false

	for _, acc := range accs {
		if acc.Active {
			activeAccountUser = acc.User
			activeRemFormatted = acc.RemainingFormatted
			activeRemSec = acc.RemainingSeconds
			isValid = (acc.RemainingSeconds > 0 && acc.Healthy)
			break
		}
	}

	if !isValid && len(accs) > 0 {
		for _, acc := range accs {
			if acc.RemainingSeconds > 0 && acc.Healthy {
				activeAccountUser = acc.User
				activeRemFormatted = acc.RemainingFormatted
				activeRemSec = acc.RemainingSeconds
				isValid = true
				break
			}
		}
	}

	return map[string]interface{}{
		"valid":               isValid,
		"remaining_seconds":   activeRemSec,
		"remaining_formatted": activeRemFormatted,
		"user":                activeAccountUser,
		"mode":                "Zero-Browser Pure SignalR",
		"is_logging_in":       autoLoginRunning.Load(),
		"is_refreshing":       autoRefreshActive.Load(),
		"accounts":            accs,
	}
}
