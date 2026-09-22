package client

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"geminigo/internal/config"
)

var (
	codePattern   = regexp.MustCompile("(?s)```(python|javascript|text)\\?code_(reference|stdout)&code_event_index=\\d+\\n.*?```\\n?")
	cardPattern   = regexp.MustCompile(`http://googleusercontent\.com/card_content/\d+\n?`)
	imageURLRegex = regexp.MustCompile(`(?i)(?:https?:)?//[^\s"'<>\\]+|(?:[a-z0-9.-]+\.)?googleusercontent\.com/[^\s"'<>\\]+`)
	convIDRegex   = regexp.MustCompile(`\b(c_[a-zA-Z0-9_-]+)\b`)
	respIDRegex   = regexp.MustCompile(`\b(r_[a-zA-Z0-9_-]+)\b`)
	choiceIDRegex = regexp.MustCompile(`\b(rc_[a-zA-Z0-9_-]+)\b`)
)

func CleanText(text string) string {
	text = codePattern.ReplaceAllString(text, "")
	text = cardPattern.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func ExtractTextsFromLine(line string) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return nil
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}
	subArr, ok := arr[0].([]interface{})
	if !ok || len(subArr) < 3 {
		return nil
	}
	innerStr, ok := subArr[2].(string)
	if !ok || len(innerStr) < 50 {
		return nil
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return nil
	}
	if len(inner) <= 4 || inner[4] == nil {
		return nil
	}
	parts, ok := inner[4].([]interface{})
	if !ok || len(parts) == 0 {
		return nil
	}
	// Only extract from Candidate 0 (the primary response candidate) to prevent mixing alternative drafts
	p0, ok := parts[0].([]interface{})
	if !ok || len(p0) <= 1 || p0[1] == nil {
		return nil
	}
	tList, ok := p0[1].([]interface{})
	if !ok || len(tList) == 0 {
		return nil
	}
	var sb strings.Builder
	for _, t := range tList {
		if s, ok := t.(string); ok && s != "" {
			sb.WriteString(s)
		}
	}
	full := sb.String()
	if full == "" {
		return nil
	}
	return []string{full}
}

func ExtractResponseText(raw string) string {
	lastText := ""
	for _, line := range strings.Split(raw, "\n") {
		for _, t := range ExtractTextsFromLine(line) {
			if len(t) > len(lastText) {
				lastText = t
			}
		}
	}
	return CleanText(lastText)
}

// ExtractConversationMetadata extracts conversation_id (c_...), response_id (r_...),
// and choice_id (rc_...) from the Google Gemini response body.
func ExtractConversationMetadata(raw string) (convID, respID, choiceID string) {
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		if !strings.Contains(line, `"wrb.fr"`) || len(line) < 50 {
			continue
		}
		var arr []interface{}
		if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
			continue
		}
		subArr, ok := arr[0].([]interface{})
		if !ok || len(subArr) < 3 {
			continue
		}
		innerStr, ok := subArr[2].(string)
		if !ok || len(innerStr) < 20 {
			continue
		}
		var inner []interface{}
		if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
			continue
		}

		// 1. inner[1]: [conv_id, resp_id]
		if len(inner) > 1 && inner[1] != nil {
			if idList, ok := inner[1].([]interface{}); ok && len(idList) >= 2 {
				if c, ok := idList[0].(string); ok && c != "" {
					convID = c
				}
				if r, ok := idList[1].(string); ok && r != "" {
					respID = r
				}
			}
		}

		// 2. inner[4][0][0]: choice_id ("rc_...")
		if len(inner) > 4 && inner[4] != nil {
			if parts, ok := inner[4].([]interface{}); ok && len(parts) > 0 {
				if p0, ok := parts[0].([]interface{}); ok && len(p0) > 0 {
					if rc, ok := p0[0].(string); ok && rc != "" {
						choiceID = rc
					}
				}
			}
		}

		if convID != "" && respID != "" && choiceID != "" {
			return convID, respID, choiceID
		}
	}

	// Fallback: Regex extraction
	if convID == "" {
		if m := convIDRegex.FindStringSubmatch(raw); len(m) > 1 {
			convID = m[1]
		}
	}
	if respID == "" {
		if m := respIDRegex.FindStringSubmatch(raw); len(m) > 1 {
			respID = m[1]
		}
	}
	if choiceID == "" {
		if m := choiceIDRegex.FindStringSubmatch(raw); len(m) > 1 {
			choiceID = m[1]
		}
	}

	return convID, respID, choiceID
}

func ExtractThinkingAndText(raw string) (string, string) {
	var startIdx = -1
	var startLen = 0

	markers := []string{"<ctrl94>thought", "<ctrl94>"}
	for _, marker := range markers {
		if idx := strings.Index(raw, marker); idx != -1 {
			startIdx = idx
			startLen = len(marker)
			break
		}
	}

	if startIdx == -1 {
		return "", raw
	}

	var endIdx = -1
	var endLen = 0
	endMarkers := []string{"<ctrl95>"}
	for _, marker := range endMarkers {
		if idx := strings.Index(raw[startIdx+startLen:], marker); idx != -1 {
			endIdx = startIdx + startLen + idx
			endLen = len(marker)
			break
		}
	}

	if endIdx == -1 {
		thinking := strings.TrimSpace(raw[startIdx+startLen:])
		textBefore := strings.TrimSpace(raw[:startIdx])
		return thinking, textBefore
	}

	thinking := strings.TrimSpace(raw[startIdx+startLen : endIdx])
	textBefore := raw[:startIdx]
	textAfter := raw[endIdx+endLen:]

	textContent := strings.TrimSpace(textBefore + textAfter)
	return thinking, textContent
}

var videoChipRegex = regexp.MustCompile(`https?://[^\s"'<>\\]*video_gen_chip[^\s"'<>\\]*`)

func CollectVideos(text string) []string {
	var videos []string
	seen := make(map[string]bool)
	for _, match := range videoChipRegex.FindAllString(text, -1) {
		cleaned := strings.TrimSpace(match)
		cleaned = strings.TrimRight(cleaned, ".,);]")
		if !seen[cleaned] {
			seen[cleaned] = true
			videos = append(videos, cleaned)
		}
	}
	return videos
}

func ExtractVideoURLs(rawBody string) []string {
	var urls []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(rawBody, "\n") {
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		var arr []interface{}
		if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
			continue
		}
		subArr, ok := arr[0].([]interface{})
		if !ok || len(subArr) < 3 {
			continue
		}
		innerStr, ok := subArr[2].(string)
		if !ok {
			continue
		}
		var inner []interface{}
		if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
			continue
		}
		if len(inner) > 4 {
			if candidates, ok := inner[4].([]interface{}); ok && len(candidates) > 0 {
				if c0, ok := candidates[0].([]interface{}); ok {
					if len(c0) > 12 {
						if media, ok := c0[12].([]interface{}); ok && len(media) > 59 && media[59] != nil {
							if v59, ok := media[59].([]interface{}); ok && len(v59) > 0 {
								if v59_0, ok := v59[0].([]interface{}); ok && len(v59_0) > 0 {
									if v59_0_0, ok := v59_0[0].([]interface{}); ok && len(v59_0_0) > 0 {
										if v59_0_0_0, ok := v59_0_0[0].([]interface{}); ok && len(v59_0_0_0) > 0 {
											if v59_0_0_0_0, ok := v59_0_0_0[0].([]interface{}); ok && len(v59_0_0_0_0) > 7 {
												if urlsArr, ok := v59_0_0_0_0[7].([]interface{}); ok {
													if len(urlsArr) >= 2 {
														if vidURL, ok := urlsArr[1].(string); ok && vidURL != "" && !seen[vidURL] {
															seen[vidURL] = true
															urls = append(urls, vidURL)
														}
													} else if len(urlsArr) == 1 {
														if vidURL, ok := urlsArr[0].(string); ok && vidURL != "" && !seen[vidURL] {
															seen[vidURL] = true
															urls = append(urls, vidURL)
														}
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return urls
}

func CollectImages(text string) []string {
	var images []string
	seen := make(map[string]bool)
	matches := imageURLRegex.FindAllString(text, -1)
	for _, match := range matches {
		cleaned := strings.TrimSpace(match)
		cleaned = strings.TrimRight(cleaned, ".,);]")
		lower := strings.ToLower(cleaned)
		if strings.Contains(lower, "image_generation_content") {
			continue
		}
		if strings.HasPrefix(cleaned, "//") {
			cleaned = "https:" + cleaned
		}
		if strings.Contains(lower, "googleusercontent.com") {
			if !strings.HasPrefix(lower, "http") {
				cleaned = "https://" + cleaned
			}
			if !seen[cleaned] {
				seen[cleaned] = true
				images = append(images, cleaned)
			}
		}
	}
	return images
}

var (
	httpClient     *http.Client
	httpClientOnce sync.Once
)

func GetHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		tr := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
		if config.CONFIG.Proxy != "" {
			proxyURL, err := url.Parse(config.CONFIG.Proxy)
			if err == nil {
				tr.Proxy = http.ProxyURL(proxyURL)
			}
		}
		timeout := config.CONFIG.RequestTimeoutSec
		if timeout <= 0 {
			timeout = 180
		}
		httpClient = &http.Client{
			Transport: tr,
			Timeout:   time.Duration(timeout) * time.Second,
		}
	})
	return httpClient
}

var (
	streamClient     *http.Client
	streamClientOnce sync.Once
)

func GetStreamHTTPClient() *http.Client {
	streamClientOnce.Do(func() {
		tr := &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}
		if config.CONFIG.Proxy != "" {
			proxyURL, err := url.Parse(config.CONFIG.Proxy)
			if err == nil {
				tr.Proxy = http.ProxyURL(proxyURL)
			}
		}
		streamClient = &http.Client{
			Transport: tr,
			Timeout:   0, // No overall timeout for long SSE streams
		}
	})
	return streamClient
}

func GenerateTextRaw(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid string) (string, string, error) {
	return GenerateTextRawWithMetadata(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid, "", ConversationMetadata{})
}

func GenerateTextRawWithUUID(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid, convUUID string) (string, string, error) {
	return GenerateTextRawWithMetadata(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid, convUUID, ConversationMetadata{})
}

func GenerateTextRawWithMetadata(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid, convUUID string, meta ConversationMetadata) (string, string, error) {
	return generateTextRawWithReqIDAndMeta(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid, convUUID, meta, 0)
}

func generateTextRawWithReqID(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid, convUUID string, reqID int64) (string, string, error) {
	return generateTextRawWithReqIDAndMeta(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid, convUUID, ConversationMetadata{}, reqID)
}

func generateTextRawWithReqIDAndMeta(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid, convUUID string, meta ConversationMetadata, reqID int64) (string, string, error) {
	client := GetHTTPClient()
	_, _, xsrf, bl, fsid := GetFullPageTokens(cookieStr)
	log.Printf("[DEBUG] Extracted XsrfToken: '%s', BL: '%s', FSID: '%s'\n", xsrf, bl, fsid)
	body := BuildPayloadWithMetadata(prompt, "", modelID, thinkMode, fileRefs, xsrf, convUUID, meta)
	if len(body) > 300 {
		log.Printf("[DEBUG] Payload body preview: %s\n", body[:300])
	} else {
		log.Printf("[DEBUG] Payload body: %s\n", body)
	}
	streamURL := GetStreamURLWithParams(reqID, bl, fsid)
	req, err := http.NewRequest("POST", streamURL, strings.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header = BuildHeadersWithCookie(cookieStr, sapisid)
	req.Header.Set("x-goog-ext-525001261-jspb", ModelHeader(modelID))

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if len(resp.Cookies()) > 0 {
		merged := MergeCookieString(cookieStr, resp.Cookies())
		if merged != cookieStr {
			UpdateCookieInPool(cookieStr, merged)
		}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	bodyStr := string(data)

	if geminiErr := ClassifyHTTPError(resp.StatusCode, bodyStr); geminiErr != nil {
		return "", bodyStr, geminiErr
	}

	if strings.Contains(bodyStr, `"xsrf",`) {
		re := regexp.MustCompile(`"xsrf","([^"]+)"`)
		if m := re.FindStringSubmatch(bodyStr); len(m) > 1 {
			newXsrf := m[1]
			log.Printf("[XSRF] Detected XSRF token in error response. Updating cache to '%s' and retrying generateText...\n", newXsrf)
			UpdateXsrfToken(newXsrf, cookieStr)
			// Retry once
			return generateTextRawWithReqIDAndMeta(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid, convUUID, meta, reqID)
		}
	}

	parsed := ExtractResponseText(bodyStr)
	if parsed == "" {
		snippet := bodyStr
		if len(snippet) > 1000 {
			snippet = snippet[:1000]
		}
		log.Printf("[DEBUG] Parsed text is empty. Raw response (len=%d) snippet: %s\n", len(data), snippet)
	} else {
		log.Printf("[DEBUG] Parsed text: '%s'\n", parsed)
	}

	if strings.Contains(parsed, "video_gen_chip") {
		log.Println("[Video] Video generating (async - needs WebSocket)")
	}

	if len(bodyStr) > 0 && bodyStr[0] == ')' {
		lines := strings.SplitN(bodyStr, "\n", 5)
		for i, line := range lines {
			if len(line) > 200 {
				line = line[:200]
			}
			log.Printf("[DEBUG] Raw line %d: %s", i, line)
		}
	}

	return parsed, bodyStr, nil
}

func GenerateText(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, cookieStr, sapisid string) (string, error) {
	parsed, _, err := GenerateTextRaw(prompt, modelID, thinkMode, fileRefs, cookieStr, sapisid)
	return parsed, err
}
