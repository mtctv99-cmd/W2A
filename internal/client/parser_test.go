package client

import (
	"encoding/json"
	"testing"
)

func TestExtractTextsFromLine_SingleCandidate(t *testing.T) {
	// Candidate 0 with 2 parts and Candidate 1 (alternative draft)
	inner := []interface{}{
		nil, nil, nil, nil,
		[]interface{}{
			// Candidate 0: ["rc_01", ["Part 1: This is a longer text to make sure line exceeds minimum length required by parser", "Part 2: More long text to ensure buffer length."]],
			[]interface{}{"rc_01", []interface{}{"Part 1: This is a longer text to make sure line exceeds minimum length required by parser", "Part 2: More long text to ensure buffer length."}},
			// Candidate 1: ["rc_02", ["Draft 2 completely different text that should never be mixed with Candidate 0 text under any circumstance."]],
			[]interface{}{"rc_02", []interface{}{"Draft 2 completely different text that should never be mixed with Candidate 0 text under any circumstance."}},
		},
	}
	innerBytes, _ := json.Marshal(inner)
	lineObj := []interface{}{
		[]interface{}{"wrb.fr", nil, string(innerBytes)},
	}
	lineBytes, _ := json.Marshal(lineObj)
	lineStr := string(lineBytes)

	texts := ExtractTextsFromLine(lineStr)
	if len(texts) == 0 {
		t.Fatalf("ExtractTextsFromLine returned empty for valid wrb.fr line of length %d", len(lineStr))
	}
	// Must only contain text from Candidate 0 (not Candidate 1)
	for _, txt := range texts {
		if txt == "Draft 2 completely different text that should never be mixed with Candidate 0 text under any circumstance." {
			t.Fatalf("ExtractTextsFromLine should NOT include alternative draft candidate 1, got: %v", texts)
		}
	}
}

func TestClassifyHTTPError_BardErrorInfo(t *testing.T) {
	err1095 := ClassifyHTTPError(200, `[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1095]]]`)
	if err1095 == nil || err1095.Type != "AUTH_ERROR" {
		t.Fatalf("BardErrorInfo 1095 should be AUTH_ERROR, got: %v", err1095)
	}

	err1100 := ClassifyHTTPError(200, `[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1100]]]`)
	if err1100 == nil || err1100.Type != "AUTH_ERROR" {
		t.Fatalf("BardErrorInfo 1100 should be AUTH_ERROR, got: %v", err1100)
	}
}

func TestExtractConversationMetadata(t *testing.T) {
	inner := []interface{}{
		nil,
		[]interface{}{"c_test_conv_123", "r_test_resp_456"},
		nil, nil,
		[]interface{}{
			[]interface{}{"rc_test_choice_789", []interface{}{"Hello test"}},
		},
	}
	innerBytes, _ := json.Marshal(inner)
	lineObj := []interface{}{
		[]interface{}{"wrb.fr", nil, string(innerBytes)},
	}
	lineBytes, _ := json.Marshal(lineObj)
	raw := string(lineBytes)

	convID, respID, choiceID := ExtractConversationMetadata(raw)
	if convID != "c_test_conv_123" {
		t.Fatalf("Expected convID c_test_conv_123, got %s", convID)
	}
	if respID != "r_test_resp_456" {
		t.Fatalf("Expected respID r_test_resp_456, got %s", respID)
	}
	if choiceID != "rc_test_choice_789" {
		t.Fatalf("Expected choiceID rc_test_choice_789, got %s", choiceID)
	}

	// Also test regex fallback
	rawRegex := `Some random log with c_regex_111 and response r_regex_222 and candidate rc_regex_333 in text`
	c2, r2, rc2 := ExtractConversationMetadata(rawRegex)
	if c2 != "c_regex_111" || r2 != "r_regex_222" || rc2 != "rc_regex_333" {
		t.Fatalf("Regex fallback failed: %s, %s, %s", c2, r2, rc2)
	}
}
