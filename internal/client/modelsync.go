package client

import (
	"log"

	"geminigo/internal/config"
)

// SyncAllSupportedModels registers Gemini Web models and saves the active list to config.
// Google API querying is completely skipped to ensure 100% web-only routing.
func SyncAllSupportedModels() (int, error) {
	allModels := make(map[string]config.ModelCfg)

	// Gemini Web models
	allModels["gemini-3.8-flash"] = config.ModelCfg{
		Mode:       10,
		Think:      4,
		Desc:       "Gemini 3.8 Flash - Latest flagship, high-speed & all-around (Default)",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "free_recommended",
	}
	allModels["gemini-3.8-thinking"] = config.ModelCfg{
		Mode:       2,
		Think:      4,
		Desc:       "Gemini 3.8 Thinking - Extended thinking & deep problem solving",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "web_thinking",
	}
	allModels["gemini-3.7-thinking"] = config.ModelCfg{
		Mode:       2,
		Think:      4,
		Desc:       "Gemini 3.7 Thinking - Chain-of-thought deep reasoning",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "web_thinking",
	}
	allModels["gemini-3.7-flash"] = config.ModelCfg{
		Mode:       10,
		Think:      4,
		Desc:       "Gemini 3.7 Flash - High-speed reasoning",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "free_recommended",
	}
	allModels["gemini-3.1-pro"] = config.ModelCfg{
		Mode:       3,
		Think:      4,
		Desc:       "Gemini 3.1 Pro - Complex reasoning & code",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "pro",
	}
	allModels["gemini-3.5-flash"] = config.ModelCfg{
		Mode:       1,
		Think:      4,
		Desc:       "Gemini 3.5 Flash - Legacy web chat",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "free_recommended",
	}
	allModels["gemini-3.5-flash-lite"] = config.ModelCfg{
		Mode:       6,
		Think:      4,
		Desc:       "Gemini 3.5 Flash Lite - Fast response",
		Source:     "web_chat",
		InputLimit: 1048576,
		Category:   "free_recommended",
	}
	allModels["gemini-veo"] = config.ModelCfg{
		Mode:       10,
		Think:      0,
		Desc:       "Google Veo - Video generation (Playwright CDP)",
		Source:     "media_cdp",
		InputLimit: 1000,
		Category:   "media",
	}
	allModels["gemini-imagen"] = config.ModelCfg{
		Mode:       10,
		Think:      0,
		Desc:       "Imagen 3 - Image generation (Playwright CDP)",
		Source:     "media_cdp",
		InputLimit: 1000,
		Category:   "media",
	}

	
	// Microsoft 365 Copilot & GPT models
	allModels["copilot-auto"] = config.ModelCfg{
		Mode:       100,
		Think:      4,
		Desc:       "Microsoft 365 Copilot Auto - Tự động điều chỉnh",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["copilot-m365"] = config.ModelCfg{
		Mode:       100,
		Think:      4,
		Desc:       "Microsoft 365 Copilot Standard Mode",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["copilot-quick"] = config.ModelCfg{
		Mode:       101,
		Think:      0,
		Desc:       "Microsoft 365 Copilot Quick response - Phản hồi nhanh tức thì (~4s)",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["copilot-think"] = config.ModelCfg{
		Mode:       102,
		Think:      4,
		Desc:       "Microsoft 365 Copilot Think deeper - Suy nghĩ sâu cho câu hỏi khó",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["gpt-5.6-think"] = config.ModelCfg{
		Mode:       103,
		Think:      4,
		Desc:       "OpenAI GPT-5 Reasoning model (Think deeper)",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["gpt-5.6-quick"] = config.ModelCfg{
		Mode:       104,
		Think:      0,
		Desc:       "OpenAI GPT-5 Fast response mode",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["gpt-5.6"] = config.ModelCfg{
		Mode:       104,
		Think:      0,
		Desc:       "OpenAI GPT-5 (Alias)",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["gpt-4o"] = config.ModelCfg{
		Mode:       104,
		Think:      0,
		Desc:       "Alias for GPT-5.6 / Copilot",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["dall-e-3"] = config.ModelCfg{
		Mode:       105,
		Think:      0,
		Desc:       "OpenAI DALL-E 3 (via Copilot) - Tạo hình ảnh AI chất lượng cao 2048x2048",
		Source:     "copilot_ws",
		Category:   "copilot",
	}
	allModels["copilot-image"] = config.ModelCfg{
		Mode:       105,
		Think:      0,
		Desc:       "Microsoft Copilot DALL-E 3 - Tạo ảnh nghệ thuật độ phân giải cao",
		Source:     "copilot_ws",
		Category:   "copilot",
	}

	config.ReplaceAllModels(allModels)
	log.Printf("[ModelSync] Active Gemini Web models registry updated (%d total models)\n", len(allModels))
	return len(allModels), nil
}
