package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type ModelCfg struct {
	Mode       int    `json:"mode"`
	Think      int    `json:"think"`
	Desc       string `json:"desc"`
	Source     string `json:"source,omitempty"`     // "google_api", "web_chat", "media_cdp"
	InputLimit int    `json:"input_limit,omitempty"` // Maximum input tokens
	Category   string `json:"category,omitempty"`   // "free_recommended", "web_thinking", "pro", "media"
}

type Config struct {
	Port              int                 `json:"port"`
	Host              string              `json:"host"`
	RetryAttempts     int                 `json:"retry_attempts"`
	RetryDelaySec     int                 `json:"retry_delay_sec"`
	RequestTimeoutSec int                 `json:"request_timeout_sec"`
	GeminiBL          string              `json:"gemini_bl"`
	AuthUser          string              `json:"auth_user"`
	XsrfToken         string              `json:"xsrf_token"`
	DefaultModel      string              `json:"default_model"`
	LogRequests       bool                `json:"log_requests"`
	CookieFile        string              `json:"cookie_file"`
	Proxy             string              `json:"proxy"`
	RequireAuth       bool                `json:"require_auth"`
	ApiKeys           []string            `json:"api_keys"`
	RateLimit         int                 `json:"rate_limit"`
	Models            map[string]ModelCfg `json:"models"`
}

var CONFIG = Config{
	Port:              8081,
	Host:              "0.0.0.0",
	RetryAttempts:     3,
	RetryDelaySec:     2,
	RequestTimeoutSec: 180,
	GeminiBL:          "boq_assistant-bard-web-server_20260525.09_p0",
	DefaultModel:      "gemini-3.7-flash",
	LogRequests:       true,
	RequireAuth:       false,
}

var CONFIG_PATH = ""
var modelsMu sync.RWMutex
var configMu sync.RWMutex

var MODELS = map[string]ModelCfg{
	// 1. Google Gemini (Web Chat)
	"gemini-3.8-flash": {
		Mode:     10,
		Think:    4,
		Desc:     "Gemini 3.8 Flash - Mặc định, siêu tốc, đa năng",
		Category: "gemini",
	},
	"gemini-3.8-thinking": {
		Mode:     2,
		Think:    4,
		Desc:     "Gemini 3.8 Thinking - Tư duy mở rộng, giải bài toán phức tạp",
		Category: "gemini",
	},
	"gemini-3.1-pro": {
		Mode:     3,
		Think:    4,
		Desc:     "Gemini 3.1 Pro - Lý luận nâng cao, lập trình & toán học",
		Category: "gemini",
	},

	// 2. Google Media (Playwright CDP)
	"gemini-imagen": {
		Mode:     10,
		Think:    0,
		Desc:     "Google Imagen 3 - Tạo ảnh AI chất lượng cao (Alias: dall-e-3)",
		Category: "media",
	},
	"gemini-veo": {
		Mode:     10,
		Think:    0,
		Desc:     "Google Veo - Tạo video AI chất lượng điện ảnh",
		Category: "media",
	},

	// 3. Microsoft Copilot & OpenAI GPT-5 (M365 Sydney)
	"copilot-quick": {
		Mode:     101,
		Think:    0,
		Desc:     "Copilot Quick - Phản hồi nhanh tức thì (~4s)",
		Category: "copilot",
	},
	"gpt-5.6-think": {
		Mode:     102,
		Think:    4,
		Desc:     "OpenAI GPT-5.6 Reasoning - Tư duy sâu (Think Deeper)",
		Category: "copilot",
	},
		"copilot-auto": {
			Mode:     100,
			Think:    4,
			Desc:     "Copilot Auto - Tự cân đối tốc độ và tư duy",
			Category: "copilot",
		},
		"dall-e-3": {
			Mode:     103,
			Think:    0,
			Desc:     "DALL-E 3 (Copilot) - Tạo hình ảnh AI chất lượng cao 2048x2048",
			Category: "copilot",
		},
		"copilot-image": {
			Mode:     103,
			Think:    0,
			Desc:     "Microsoft Copilot DALL-E 3 - Tạo ảnh nghệ thuật độ phân giải cao",
			Category: "copilot",
		},
	}

func GetModels() map[string]ModelCfg {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	res := make(map[string]ModelCfg, len(MODELS))
	for k, v := range MODELS {
		res[k] = v
	}
	return res
}

func GetModelCfg(name string) (ModelCfg, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	cfg, ok := MODELS[name]
	return cfg, ok
}

func UpdateModels(discovered map[string]ModelCfg) {
	modelsMu.Lock()
	for k, v := range discovered {
		MODELS[k] = v
	}
	configMu.Lock()
	if CONFIG.Models == nil {
		CONFIG.Models = make(map[string]ModelCfg)
	}
	for k, v := range MODELS {
		CONFIG.Models[k] = v
	}
	_ = saveConfigLocked()
	configMu.Unlock()
	modelsMu.Unlock()
}

func ReplaceAllModels(newModels map[string]ModelCfg) {
	modelsMu.Lock()
	MODELS = make(map[string]ModelCfg, len(newModels))
	for k, v := range newModels {
		MODELS[k] = v
	}
	configMu.Lock()
	CONFIG.Models = make(map[string]ModelCfg, len(newModels))
	for k, v := range newModels {
		CONFIG.Models[k] = v
	}
	_ = saveConfigLocked()
	configMu.Unlock()
	modelsMu.Unlock()
}

// ResolveModelRouting extracts routing target (hybrid, api_only, web_only, copilot) and base model.
func ResolveModelRouting(rawModel string) (baseModel string, routeMode string) {
	lower := strings.ToLower(strings.TrimSpace(rawModel))
	if strings.HasPrefix(lower, "copilot") || strings.HasPrefix(lower, "gpt-5") || strings.HasPrefix(lower, "dall-e") || strings.HasPrefix(lower, "dalle") || lower == "gpt-4o" {
		return ResolveModelAlias(lower), "copilot"
	}

	// ponytail: forced web_only routing to completely disable Google API and use Gemini Web cookies
	routeMode = "web_only"

	lower = strings.TrimSuffix(lower, "-api")
	lower = strings.TrimSuffix(lower, "-web")
	lower = strings.TrimSuffix(lower, "-wed")

	baseModel = ResolveModelAlias(lower)
	return baseModel, routeMode
}

// ResolveModelAlias maps legacy or 2.x/3.x model requests to target models.
func ResolveModelAlias(modelName string) string {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	if _, ok := MODELS[modelName]; ok {
		return modelName
	}
	lower := strings.ToLower(modelName)
		switch lower {
		case "copilot", "copilot-auto", "copilot-default", "copilot-m365":
			return "copilot-auto"
		case "copilot-fast", "copilot-quick", "gpt-5.6-quick", "gpt-5.6-fast", "gpt-4o", "gpt-5", "gpt-5.6":
			return "copilot-quick"
		case "copilot-think", "gpt-5.6-think", "gpt-5-think":
			return "gpt-5.6-think"
		case "gemini-flash", "gemini-2.0-flash", "gemini-2.5-flash", "gemini-auto", "flash", "gemini-3.8", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-flash-lite":
			return "gemini-3.8-flash"
		case "gemini-thinking", "gemini-2.0-flash-thinking", "gemini-2.5-flash-thinking", "thinking", "tư duy mở rộng", "gemini-3.8-thinking", "gemini-3.7-thinking", "gemini-3.6-thinking", "gemini-3.5-flash-thinking", "gemini-3.5-flash-thinking-lite":
			return "gemini-3.8-thinking"
		case "gemini-pro", "gemini-2.0-pro", "gemini-2.5-pro", "pro", "gemini-3.1-pro", "gemini-3.7-pro":
			return "gemini-3.1-pro"
		case "copilot-image", "copilot-dalle", "copilot-dall-e-3", "copilot-dall-e", "dall-e", "dall-e-3", "dall-e-2", "dalle":
			return "copilot-image"
		case "imagen-3.0", "imagen-3", "imagen", "imagen-3.0-generate-002", "gemini-image", "gemini-imagen", "image", "tạo ảnh", "tao anh":
			return "gemini-imagen"
		case "veo", "veo-2", "veo-3", "veo-2.0-generate-001", "gemini-video", "video", "sora", "tạo video", "tao video":
			return "gemini-veo"
		}
		return modelName
	}

	// ModelNameForID returns the model name string for a given Mode ID.
	func ModelNameForID(mode int) string {
		switch mode {
		case 10:
			return "gemini-3.8-flash"
		case 3:
			return "gemini-3.1-pro"
		case 2:
			return "gemini-3.8-thinking"
		default:
			return "gemini-3.8-flash"
		}
	}

func IsAuthRequired() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return CONFIG.RequireAuth
}

func SetRequireAuth(required bool) error {
	configMu.Lock()
	CONFIG.RequireAuth = required
	err := saveConfigLocked()
	configMu.Unlock()
	return err
}

func GetApiKeys() []string {
	configMu.RLock()
	defer configMu.RUnlock()
	keys := make([]string, len(CONFIG.ApiKeys))
	copy(keys, CONFIG.ApiKeys)
	return keys
}

func AddApiKey(newKey string) (string, error) {
	configMu.Lock()
	defer configMu.Unlock()
	if newKey == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		newKey = hex.EncodeToString(b)
	}
	CONFIG.ApiKeys = append(CONFIG.ApiKeys, newKey)
	return newKey, saveConfigLocked()
}

func DeleteApiKey(keyToDelete string) error {
	configMu.Lock()
	defer configMu.Unlock()
	found := false
	var updatedKeys []string
	for _, k := range CONFIG.ApiKeys {
		if k == keyToDelete {
			found = true
		} else {
			updatedKeys = append(updatedKeys, k)
		}
	}
	if !found {
		return fmt.Errorf("key not found")
	}
	CONFIG.ApiKeys = updatedKeys
	return saveConfigLocked()
}

func LoadConfig(path string) {
	if path == "" {
		return
	}
	configMu.Lock()
	defer configMu.Unlock()

	CONFIG_PATH = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Automatically create default config.json when missing
			defaultConfig := Config{
				Port:              8081,
				Host:              "0.0.0.0",
				RetryAttempts:     3,
				RetryDelaySec:     2,
				RequestTimeoutSec: 180,
				GeminiBL:          "boq_assistant-bard-web-server_20260525.09_p0",
				DefaultModel:      "gemini-3.7-flash",
				LogRequests:       true,
				CookieFile:        "data/cookies.json",
				RequireAuth:       false,
				ApiKeys:           []string{},
			}
			jsonData, marshalErr := json.MarshalIndent(defaultConfig, "", "  ")
			if marshalErr == nil {
				_ = os.WriteFile(path, jsonData, 0644)
				CONFIG = defaultConfig
				log.Printf("Config: created default %s", path)
				return
			}
		}
		log.Printf("Config: cannot read %s: %v (using defaults)", path, err)
		return
	}
	if err := json.Unmarshal(data, &CONFIG); err != nil {
		log.Printf("Config: invalid JSON in %s: %v (using partial defaults)", path, err)
	}
	if CONFIG.Models != nil {
		modelsMu.Lock()
		for name, cfg := range CONFIG.Models {
			MODELS[name] = cfg
		}
		modelsMu.Unlock()
	}
	if CONFIG.Models == nil {
		CONFIG.Models = make(map[string]ModelCfg)
	}
	modelsMu.RLock()
	for name, cfg := range MODELS {
		CONFIG.Models[name] = cfg
	}
	modelsMu.RUnlock()
}

func saveConfigLocked() error {
	if CONFIG_PATH == "" {
		CONFIG_PATH = "config.json"
	}
	data, err := json.MarshalIndent(CONFIG, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(CONFIG_PATH)
	_ = os.MkdirAll(dir, 0755)
	tmpFile := CONFIG_PATH + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, CONFIG_PATH)
}

func SaveConfig() error {
	configMu.Lock()
	defer configMu.Unlock()
	return saveConfigLocked()
}
