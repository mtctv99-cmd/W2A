package client

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"geminigo/internal/stats"
)

var (
	googleKeyMu     sync.RWMutex
	googleKeyHealth = make(map[string]time.Time)
	googleKeyIndex  uint64
)

func GetNextHealthyGoogleAPIKey() (string, string) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	var keys []struct {
		id  string
		key string
	}
	for _, acc := range pool.Accounts {
		if acc.GoogleAPIKey != "" {
			keys = append(keys, struct {
				id  string
				key string
			}{id: acc.ID, key: acc.GoogleAPIKey})
		}
	}

	if len(keys) == 0 {
		return "", ""
	}

	googleKeyMu.RLock()
	defer googleKeyMu.RUnlock()

	total := len(keys)
	start := int(atomic.AddUint64(&googleKeyIndex, 1)-1) % total

	for i := 0; i < total; i++ {
		idx := (start + i) % total
		k := keys[idx]
		cooldown, exists := googleKeyHealth[k.id]
		if !exists || time.Now().After(cooldown) {
			return k.key, k.id
		}
	}

	// Fallback to first key if all in cooldown
	return keys[start].key, keys[start].id
}

func MarkGoogleKeyCooldown(accountID string, d time.Duration) {
	googleKeyMu.Lock()
	defer googleKeyMu.Unlock()
	googleKeyHealth[accountID] = time.Now().Add(d)
	log.Printf("[GoogleAPI] %s API key placed on cooldown for %v\n", accountID, d)
}

func MarkGoogleKeyHealthy(accountID string) {
	googleKeyMu.Lock()
	defer googleKeyMu.Unlock()
	delete(googleKeyHealth, accountID)
}

type GoogleGenerateRequest struct {
	Contents []GoogleContent `json:"contents"`
}

type GoogleContent struct {
	Parts []interface{} `json:"parts"`
}

func buildGoogleAPIPayload(prompt string, images []ParsedImage) ([]byte, error) {
	var parts []interface{}
	if prompt != "" {
		parts = append(parts, map[string]string{"text": prompt})
	}
	for _, img := range images {
		parts = append(parts, map[string]interface{}{
			"inline_data": map[string]string{
				"mime_type": img.Mime,
				"data":      base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	req := GoogleGenerateRequest{
		Contents: []GoogleContent{
			{Parts: parts},
		},
	}
	return json.Marshal(req)
}

func GenerateTextGoogleAPI(prompt, modelName, apiKey string, images []ParsedImage) (string, error) {
	payload, err := buildGoogleAPIPayload(prompt, images)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", modelName, apiKey)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := GetHTTPClient()
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 429 {
			return "", &GeminiError{Type: "RATE_LIMIT", Message: "Google API rate limit (429)"}
		}
		if resp.StatusCode >= 500 {
			return "", &GeminiError{Type: "SERVER_ERROR", Message: fmt.Sprintf("Google API error %d: %s", resp.StatusCode, string(bodyBytes))}
		}
		return "", fmt.Errorf("Google API HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var respObj struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(bodyBytes, &respObj); err != nil {
		return "", fmt.Errorf("decode Google API response: %w", err)
	}

	if len(respObj.Candidates) == 0 || len(respObj.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Google API")
	}

	var sb strings.Builder
	for _, p := range respObj.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

func StreamChatGoogleAPI(w http.ResponseWriter, prompt, modelName, apiKey string, images []ParsedImage) error {
	payload, err := buildGoogleAPIPayload(prompt, images)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", modelName, apiKey)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := GetStreamHTTPClient()
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 429 {
			return &GeminiError{Type: "RATE_LIMIT", Message: "Google API rate limit (429)"}
		}
		return fmt.Errorf("Google API stream HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	cid := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var totalTokens int64

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		rawJSON := strings.TrimPrefix(line, "data: ")
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal([]byte(rawJSON), &chunk); err != nil {
			continue
		}

		if len(chunk.Candidates) > 0 && len(chunk.Candidates[0].Content.Parts) > 0 {
			textDelta := chunk.Candidates[0].Content.Parts[0].Text
			if textDelta != "" {
				totalTokens += int64(len(textDelta) / 4)
				outChunk := map[string]interface{}{
					"id":      cid,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   modelName,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"delta": map[string]string{
								"content": textDelta,
							},
							"finish_reason": nil,
						},
					},
				}
				data, _ := json.Marshal(outChunk)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	}

	stopChunk := map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	data, _ := json.Marshal(stopChunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	stats.UpdateStats(totalTokens + int64(len(prompt)/4))
	return nil
}
