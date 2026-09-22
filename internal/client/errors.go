package client

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type GeminiError struct {
	Type    string
	Message string
}

func (e *GeminiError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Type, e.Message)
}

func ClassifyHTTPError(statusCode int, body string) *GeminiError {
	lower := strings.ToLower(body)

	switch {
	case statusCode == 429:
		return &GeminiError{Type: "RATE_LIMIT", Message: "HTTP 429 Too Many Requests"}
	case statusCode == 403:
		if strings.Contains(lower, "captcha") || strings.Contains(lower, "unusual traffic") {
			return &GeminiError{Type: "CAPTCHA", Message: "Google CAPTCHA/Unusual traffic detected"}
		}
		return &GeminiError{Type: "AUTH_ERROR", Message: "HTTP 403 Forbidden - cookie may be expired"}
	case statusCode == 401:
		return &GeminiError{Type: "AUTH_ERROR", Message: "HTTP 401 Unauthorized - cookie expired"}
	case statusCode == 405:
		return &GeminiError{Type: "METHOD_NOT_ALLOWED", Message: "HTTP 405 - cookie invalid or endpoint mismatch"}
	case statusCode >= 500:
		return &GeminiError{Type: "SERVER_ERROR", Message: fmt.Sprintf("HTTP %d - Google server error", statusCode)}
	}

	if strings.Contains(lower, `"rate limit"`) || strings.Contains(lower, `"quota"`) {
		return &GeminiError{Type: "RATE_LIMIT", Message: "Rate limit/quota exceeded"}
	}
	if strings.Contains(lower, `"captcha"`) || strings.Contains(lower, `"unusual traffic"`) {
		return &GeminiError{Type: "CAPTCHA", Message: "CAPTCHA triggered"}
	}
	if strings.Contains(lower, "barderrorinfo") {
		code := ""
		if idx := strings.Index(lower, "barderrorinfo"); idx != -1 {
			after := lower[idx+len("barderrorinfo"):]
			after = strings.TrimLeft(after, `"\,[]`)
			for _, c := range after {
				if c >= '0' && c <= '9' {
					code += string(c)
				} else {
					break
				}
			}
		}
			// Any BardErrorInfo code represents a session, cookie, quota or account validation error from Google Web
			if code == "" {
				code = "unknown"
			}
			return &GeminiError{Type: "AUTH_ERROR", Message: fmt.Sprintf("BardErrorInfo code %s - cookie/session expired or rejected", code)}
	}
	if strings.Contains(lower, `"sapisidhash"`) && statusCode == 401 {
		return &GeminiError{Type: "AUTH_ERROR", Message: "SAPISIDHASH invalid"}
	}
	if strings.Contains(lower, `]}'\\n`) && strings.Contains(lower, "dsf") {
		return &GeminiError{Type: "TOKEN_EXPIRED", Message: "XSRF/token expired - dsf in response"}
	}

	return nil
}

func ClassifyStreamError(resp *http.Response, firstLines string) *GeminiError {
	err := ClassifyHTTPError(resp.StatusCode, firstLines)
	if err != nil {
		return err
	}

	lower := strings.ToLower(firstLines)
	if strings.Contains(lower, `"rate limit"`) || strings.Contains(lower, `"quota"`) {
		return &GeminiError{Type: "RATE_LIMIT", Message: "Rate limit in stream"}
	}
	if strings.Contains(lower, `"captcha"`) {
		return &GeminiError{Type: "CAPTCHA", Message: "CAPTCHA in stream"}
	}
	return nil
}

func GetCooldownDuration(errType string, failCount int) time.Duration {
	switch errType {
	case "RATE_LIMIT":
		if failCount >= 3 {
			return cooldownLong
		}
		if failCount >= 2 {
			return cooldownMedium
		}
		return cooldownShort
	case "CAPTCHA":
		return cooldownLong
	case "AUTH_ERROR", "TOKEN_EXPIRED", "GEMINI_ERROR":
		return cooldownLong
	case "METHOD_NOT_ALLOWED":
		return cooldownShort
	case "SERVER_ERROR":
		return cooldownMedium
	default:
		return cooldownShort
	}
}
