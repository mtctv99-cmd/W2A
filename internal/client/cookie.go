package client

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"geminigo/internal/config"
)

const (
	PoolTypeSession = "session"
	PoolTypeWorker  = "worker"
	PoolTypeAuto    = "auto"
)

type AccountCookie struct {
	ID           string `json:"id"`
	Cookie       string `json:"cookie"`
	Sapisid      string `json:"sapisid"`
	ProfileDir   string `json:"profile_dir,omitempty"`
	GoogleAPIKey string `json:"google_api_key,omitempty"`
	PoolType     string `json:"pool_type,omitempty"` // "session", "worker", or "" (auto 40/60)
}

type accountHealth struct {
	cooldownUntil   time.Time
	failCount        int
	rotateAuthError  bool
}

type CookiePool struct {
	mu       sync.RWMutex
	Accounts []AccountCookie `json:"accounts"`
	index    uint64
	health   map[string]*accountHealth
}

var pool CookiePool

const (
	cooldownShort  = 10 * time.Second
	cooldownMedium = 30 * time.Second
	cooldownLong   = 2 * time.Minute
	maxFailCount   = 3
)

func init() {
	pool.health = make(map[string]*accountHealth)
}

func getHealth(id string) *accountHealth {
	h, ok := pool.health[id]
	if !ok {
		h = &accountHealth{}
		pool.health[id] = h
	}
	return h
}

func MarkCooldown(id string, duration time.Duration) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	h := getHealth(id)
	h.cooldownUntil = time.Now().Add(duration)
	h.failCount++
	log.Printf("[Pool] Account %s cooldown %v (fail #%d)\n", id, duration, h.failCount)
}

func MarkHealthy(id string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	h := getHealth(id)
	h.failCount = 0
	h.cooldownUntil = time.Time{}
	h.rotateAuthError = false
}

func MarkRotateAuthError(id string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	h := getHealth(id)
	h.rotateAuthError = true
}

func ClearRotateAuthError(id string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if h, ok := pool.health[id]; ok {
		h.rotateAuthError = false
	}
}

func isHealthy(id string) bool {
	h, ok := pool.health[id]
	if !ok {
		return true
	}
	if time.Now().Before(h.cooldownUntil) {
		return false
	}
	return true
}

func LoadPool() {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	cookieFile := config.CONFIG.CookieFile
	if cookieFile == "" {
		cookieFile = "data/cookies.json"
	}

	data, err := os.ReadFile(cookieFile)
	if err != nil {
		pool.Accounts = nil
		return
	}

	var accs []AccountCookie
	if err := json.Unmarshal(data, &accs); err == nil {
		if len(accs) > 0 {
			for i := range accs {
				if accs[i].ProfileDir == "" {
					accs[i].ProfileDir = fmt.Sprintf("data/profiles/%s", accs[i].ID)
				}
			}
			pool.Accounts = accs
			for _, acc := range accs {
				h := getHealth(acc.ID)
				h.failCount = 0
				h.rotateAuthError = false
			}
		} else {
			pool.Accounts = nil
		}
		if len(pool.Accounts) > 0 {
			StartCookieRotation()
		}
		return
	}

	content := strings.TrimSpace(string(data))
	if content != "" && !strings.HasPrefix(content, "[") {
		var legacy struct {
			Cookie  string `json:"cookie"`
			Sapisid string `json:"sapisid"`
		}
		cookieStr := content
		sapisid := ""
		if err := json.Unmarshal(data, &legacy); err == nil {
			cookieStr = legacy.Cookie
			sapisid = legacy.Sapisid
		}
		if sapisid == "" {
			for _, part := range strings.Split(cookieStr, "; ") {
				if strings.HasPrefix(part, "SAPISID=") {
					sapisid = strings.TrimPrefix(part, "SAPISID=")
					break
				}
			}
		}
		pool.Accounts = []AccountCookie{
			{ID: "Account-1", Cookie: cookieStr, Sapisid: sapisid, ProfileDir: "data/profiles/Account-1"},
		}
	}

	if len(pool.Accounts) > 0 {
		StartCookieRotation()
	}
}

var rotationOnce sync.Once

var AutoRefreshCallback func()

func SetAutoRefreshCallback(fn func()) {
	AutoRefreshCallback = fn
}

func StartCookieRotation() {
	rotationOnce.Do(func() {
		go func() {
			for {
				time.Sleep(1 * time.Hour)
				if AutoRefreshCallback != nil {
					log.Println("[AutoRefresh] Running hourly automatic profile and cookie refresh...")
					AutoRefreshCallback()
				}
			}
		}()
	})
}

func SavePool() error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return savePoolLocked()
}

func savePoolLocked() error {
	data, err := json.MarshalIndent(pool.Accounts, "", "  ")
	if err != nil {
		return err
	}

	cookieFile := config.CONFIG.CookieFile
	if cookieFile == "" {
		cookieFile = "data/cookies.json"
	}

	dir := filepath.Dir(cookieFile)
	_ = os.MkdirAll(dir, 0755)

	tmpFile := cookieFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, cookieFile)
}

func ExtractSapisid(rawCookie string) string {
	for _, part := range strings.Split(rawCookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "SAPISID=") {
			return strings.TrimPrefix(part, "SAPISID=")
		}
	}
	return ""
}

func generateNextAccountIDLocked() string {
	maxIdx := 0
	for _, acc := range pool.Accounts {
		var idx int
		if _, err := fmt.Sscanf(acc.ID, "Account-%d", &idx); err == nil {
			if idx > maxIdx {
				maxIdx = idx
			}
		}
	}
	return fmt.Sprintf("Account-%d", maxIdx+1)
}

func GenerateNextAccountID() string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return generateNextAccountIDLocked()
}

func AddAccountCookie(rawCookie string) error {
	return AddAccountCookieWithID("", rawCookie, "")
}

func AddAccountCookieWithID(accountID, rawCookie, profileDir string) error {
	sapisid := ExtractSapisid(rawCookie)
	if sapisid == "" {
		return fmt.Errorf("invalid cookie string: missing SAPISID parameter")
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	if accountID == "" {
		for _, acc := range pool.Accounts {
			if acc.Sapisid == sapisid {
				accountID = acc.ID
				break
			}
		}
	}

	if accountID == "" {
		accountID = generateNextAccountIDLocked()
	}

	if profileDir == "" {
		profileDir = fmt.Sprintf("data/profiles/%s", accountID)
	}

	_ = os.MkdirAll(profileDir, 0755)

	found := false
	for i, acc := range pool.Accounts {
		if acc.ID == accountID || (acc.Sapisid == sapisid && accountID == acc.ID) {
			pool.Accounts[i].Cookie = rawCookie
			pool.Accounts[i].Sapisid = sapisid
			pool.Accounts[i].ProfileDir = profileDir
			found = true
			break
		}
	}

	if !found {
		pool.Accounts = append(pool.Accounts, AccountCookie{
			ID:         accountID,
			Cookie:     rawCookie,
			Sapisid:    sapisid,
			ProfileDir: profileDir,
		})
	}

	return savePoolLocked()
}

func SaveAccountGoogleAPIKey(id, apiKey string) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	for i, acc := range pool.Accounts {
		if acc.ID == id {
			pool.Accounts[i].GoogleAPIKey = apiKey
			return savePoolLocked()
		}
	}
	return fmt.Errorf("account %s not found", id)
}

func DeleteAccountCookie(id string) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	found := false
	var targetProfileDir string
	var updated []AccountCookie
	for _, acc := range pool.Accounts {
		if acc.ID == id {
			found = true
			targetProfileDir = acc.ProfileDir
			if targetProfileDir == "" {
				targetProfileDir = fmt.Sprintf("data/profiles/%s", id)
			}
		} else {
			updated = append(updated, acc)
		}
	}

	if !found {
		return fmt.Errorf("account not found")
	}

	// Delete profile directory from disk
	if targetProfileDir != "" {
		_ = os.RemoveAll(targetProfileDir)
	}

	pool.Accounts = updated
	delete(pool.health, id)
	return savePoolLocked()
}

func MergeCookieString(originalCookie string, newCookies []*http.Cookie) string {
	if len(newCookies) == 0 {
		return originalCookie
	}
	cookieMap := make(map[string]string)
	var order []string
	for _, part := range strings.Split(originalCookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx > 0 {
			name := strings.TrimSpace(part[:idx])
			val := strings.TrimSpace(part[idx+1:])
			if _, exists := cookieMap[name]; !exists {
				order = append(order, name)
			}
			cookieMap[name] = val
		}
	}
	for _, c := range newCookies {
		if c == nil || c.Name == "" || c.Value == "" {
			continue
		}
		if _, exists := cookieMap[c.Name]; !exists {
			order = append(order, c.Name)
		}
		cookieMap[c.Name] = c.Value
	}
	var parts []string
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s=%s", name, cookieMap[name]))
	}
	return strings.Join(parts, "; ")
}

func UpdateCookieInPool(oldCookie, newCookie string) {
	if newCookie == "" || oldCookie == newCookie {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for i, acc := range pool.Accounts {
		if acc.Cookie == oldCookie {
			pool.Accounts[i].Cookie = newCookie
			sapisid := ExtractSapisid(newCookie)
			if sapisid != "" {
				pool.Accounts[i].Sapisid = sapisid
			}
			_ = savePoolLocked()
			break
		}
	}
}

func GetHealthyAccountCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	count := 0
	for _, acc := range pool.Accounts {
		if isHealthy(acc.ID) {
			count++
		}
	}
	return count
}

func GetAuthenticatedAccountCount() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	count := 0
	for _, acc := range pool.Accounts {
		if isHealthy(acc.ID) && IsAuthenticated(acc.Cookie) {
			count++
		}
	}
	return count
}

func GetAccounts() []AccountCookie {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	return pool.Accounts
}

func GetAccountByID(id string) (AccountCookie, bool) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, acc := range pool.Accounts {
		if acc.ID == id {
			return acc, true
		}
	}
	return AccountCookie{}, false
}

type AccountStatus struct {
	ID              string `json:"id"`
	Healthy         bool   `json:"healthy"`
	CooldownUntil   string `json:"cooldown_until,omitempty"`
	FailCount       int    `json:"fail_count"`
	RotateAuthError bool   `json:"rotate_auth_error"`
	PoolType        string `json:"pool_type"`
	ConfiguredRole  string `json:"configured_role"`
}

func GetAccountStatuses() []AccountStatus {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	total := len(pool.Accounts)
	var statuses []AccountStatus
	for i, acc := range pool.Accounts {
		h, ok := pool.health[acc.ID]
		role := GetEffectivePoolRole(i, total, acc.PoolType)
		cfg := acc.PoolType
		if cfg == "" {
			cfg = PoolTypeAuto
		}
		status := AccountStatus{
			ID:             acc.ID,
			Healthy:        true,
			FailCount:      0,
			PoolType:       role,
			ConfiguredRole: cfg,
		}
		if ok {
			status.FailCount = h.failCount
			status.RotateAuthError = h.rotateAuthError
			if time.Now().Before(h.cooldownUntil) {
				status.Healthy = false
				status.CooldownUntil = h.cooldownUntil.Format("15:04:05")
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// GetEffectivePoolRole calculates whether an account is Session (40%) or Worker (60%).
// If explicitly configured to "session" or "worker", it respects the manual setting.
// If empty or "auto", it splits automatically: the first 40% are Session, and the rest 60% are Worker.
func GetEffectivePoolRole(idx, total int, configuredRole string) string {
	switch strings.ToLower(strings.TrimSpace(configuredRole)) {
	case PoolTypeSession:
		return PoolTypeSession
	case PoolTypeWorker:
		return PoolTypeWorker
	default:
		if total <= 1 {
			return PoolTypeWorker
		}
		// 40% session, 60% worker
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

func SetAccountPoolType(accountID string, poolType string) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	for i, acc := range pool.Accounts {
		if acc.ID == accountID {
			pool.Accounts[i].PoolType = poolType
			return savePoolLocked()
		}
	}
	return fmt.Errorf("account %s not found", accountID)
}

func GetCookieByAccountID(accountID string) (string, string, string, string, bool) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, acc := range pool.Accounts {
		if acc.ID == accountID {
			pDir := acc.ProfileDir
			if pDir == "" {
				pDir = fmt.Sprintf("data/profiles/%s", acc.ID)
			}
			return acc.Cookie, acc.Sapisid, acc.ID, pDir, isHealthy(acc.ID)
		}
	}
	return "", "", "", "", false
}

func GetNextHealthyCookie() (string, string, string, string) {
	return GetNextCookieForType(PoolTypeWorker)
}

func GetNextHealthyCookieForVision() (string, string, string, string) {
	return GetNextCookieForType(PoolTypeWorker)
}

func GetNextCookieForType(requestedType string) (string, string, string, string) {
	if requestedType == "" {
		requestedType = PoolTypeWorker
	}
	requestedType = strings.ToLower(strings.TrimSpace(requestedType))
	if requestedType != PoolTypeSession && requestedType != PoolTypeWorker {
		requestedType = PoolTypeWorker
	}

	pool.mu.RLock()
	total := len(pool.Accounts)
	if total == 0 {
		pool.mu.RUnlock()
		return "", "", "", ""
	}

	start := int(atomic.AddUint64(&pool.index, 1)-1) % total

	primaryCandidates := make([]AccountCookie, 0, total)
	oppositeCandidates := make([]AccountCookie, 0, total)

	oppositeType := PoolTypeWorker
	if requestedType == PoolTypeWorker {
		oppositeType = PoolTypeSession
	}

	for i := 0; i < total; i++ {
		idx := (start + i) % total
		acc := pool.Accounts[idx]
		if isHealthy(acc.ID) {
			role := GetEffectivePoolRole(idx, total, acc.PoolType)
			if role == requestedType {
				primaryCandidates = append(primaryCandidates, acc)
			} else {
				oppositeCandidates = append(oppositeCandidates, acc)
			}
		}
	}
	pool.mu.RUnlock()

	// 1. Choose from primary candidates if available
	if len(primaryCandidates) > 0 {
		return pickBestAccount(primaryCandidates)
	}

	// 2. Overload detected! Dynamic Borrowing: borrow up to 2 accounts from opposite pool
	if len(oppositeCandidates) > 0 {
		borrowLimit := 2
		if len(oppositeCandidates) < borrowLimit {
			borrowLimit = len(oppositeCandidates)
		}
		borrowedCandidates := oppositeCandidates[:borrowLimit]
		cookie, sapisid, id, pDir := pickBestAccount(borrowedCandidates)
		log.Printf("[Pool] Dynamic Borrowing: %s pool overloaded/exhausted, borrowed account %s from %s pool (borrow pool size: %d)\n",
			requestedType, id, oppositeType, borrowLimit)
		return cookie, sapisid, id, pDir
	}

	return "", "", "", ""
}

func pickBestAccount(candidates []AccountCookie) (string, string, string, string) {
	if len(candidates) == 0 {
		return "", "", "", ""
	}

	// Priority 1: Authenticated account
	for _, acc := range candidates {
		if IsAuthenticated(acc.Cookie) {
			pDir := acc.ProfileDir
			if pDir == "" {
				pDir = fmt.Sprintf("data/profiles/%s", acc.ID)
			}
			return acc.Cookie, acc.Sapisid, acc.ID, pDir
		}
	}

	// Priority 2: Fallback to first candidate
	first := candidates[0]
	pDir := first.ProfileDir
	if pDir == "" {
		pDir = fmt.Sprintf("data/profiles/%s", first.ID)
	}
	return first.Cookie, first.Sapisid, first.ID, pDir
}

func GetAnyHealthyGoogleAPIKey() string {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, acc := range pool.Accounts {
		if acc.GoogleAPIKey != "" {
			return acc.GoogleAPIKey
		}
	}
	return ""
}

func GetNextCookie() (string, string) {
	c, s, _, _ := GetNextHealthyCookie()
	return c, s
}

func MakeSapisidHash(sapisid string) string {
	ts := time.Now().Unix()
	h := sha1.Sum([]byte(fmt.Sprintf("%d %s https://gemini.google.com", ts, sapisid)))
	return fmt.Sprintf("SAPISIDHASH %d_%x", ts, h)
}

func RotateCookies(cookieStr string) error {
	// Real session refresh is handled automatically via headless Chrome CDP in browser.RefreshAccountFromProfile
	return nil
}

func AccountPrefix() string {
	if config.CONFIG.AuthUser == "" {
		return ""
	}
	return "/u/" + config.CONFIG.AuthUser
}

func BuildHeadersWithCookie(cookieStr, sapisid string) http.Header {
	prefix := AccountPrefix()
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", "https://gemini.google.com")
	h.Set("Referer", fmt.Sprintf("https://gemini.google.com%s/app", prefix))
	h.Set("X-Same-Domain", "1")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	if prefix != "" {
		h.Set("X-Goog-AuthUser", config.CONFIG.AuthUser)
	}

	if cookieStr != "" {
		h.Set("Cookie", cookieStr)
	}
	if sapisid != "" {
		h.Set("Authorization", MakeSapisidHash(sapisid))
	}
	return h
}

func BuildHeaders() http.Header {
	cStr, sapisid := GetNextCookie()
	return BuildHeadersWithCookie(cStr, sapisid)
}

func GetStreamURL() string {
	return GetStreamURLWithReqID(0)
}

func GetStreamURLWithReqID(reqID int64) string {
	return GetStreamURLWithParams(reqID, "", "")
}

func GetStreamURLWithParams(reqID int64, bl, fsid string) string {
	if reqID <= 0 {
		reqID = time.Now().Unix() % 1000000
	}
	if bl == "" {
		bl = config.CONFIG.GeminiBL
	}
	if bl == "" {
		bl = "boq_assistant-bard-web-server_20260818.16_p0"
	}
	prefix := AccountPrefix()
	base := fmt.Sprintf("https://gemini.google.com%s/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s", prefix, url.QueryEscape(bl))
	if fsid != "" {
		base += fmt.Sprintf("&f.sid=%s", url.QueryEscape(fsid))
	}
	base += fmt.Sprintf("&hl=en&_reqid=%d&rt=c", reqID)
	return base
}
