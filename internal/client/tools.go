package client

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type Message struct {
	Role       string            `json:"role"`
	Content    interface{}       `json:"content"`
	ToolName   string            `json:"tool_name,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []ParsedToolCall  `json:"tool_calls,omitempty"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string            `json:"role"`
		Content    interface{}       `json:"content"`
		ToolName   string            `json:"tool_name,omitempty"`
		ToolCallID string            `json:"tool_call_id,omitempty"`
		ToolCalls  []ParsedToolCall  `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolName = raw.ToolName
	m.ToolCallID = raw.ToolCallID
	m.ToolCalls = raw.ToolCalls

	// Normalize content from various formats
	switch v := raw.Content.(type) {
	case string:
		m.Content = v
	case []interface{}:
		m.Content = v
	case nil:
		m.Content = ""
	default:
		if s, ok := v.(string); ok {
			m.Content = s
		} else {
			b, _ := json.Marshal(v)
			m.Content = string(b)
		}
	}
	return nil
}

// ParsedToolCall represents an OpenAI-format tool call response.
type ParsedToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description,omitempty"`
		Parameters  map[string]interface{} `json:"parameters,omitempty"`
	} `json:"function,omitempty"`
}

type ParsedImage struct {
	Data []byte
	Mime string
}

func MessagesToPrompt(messages []Message, tools []Tool, toolChoice interface{}) (string, []ParsedImage) {
	var parts []string
	var images []ParsedImage
	hasToolMsg := false

	for _, msg := range messages {
		if msg.Role == "tool" || msg.Role == "assistant" {
			hasToolMsg = true
			break
		}
	}

	if len(tools) > 0 && toolChoice != "none" {
		toolDefs := []map[string]interface{}{}
		for _, tool := range tools {
			name, desc := "", ""
			var params map[string]interface{}
			if tool.Type == "function" && tool.Function != nil {
				name = tool.Function.Name
				desc = tool.Function.Description
				params = tool.Function.Parameters
			}
			toolDefs = append(toolDefs, map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters":  params,
			})
		}
		if len(toolDefs) > 0 {
			toolDefsJSON, _ := json.MarshalIndent(toolDefs, "", "  ")
			var toolIntro string
			if hasToolMsg {
				toolIntro = "# Tool Use\n\nAvailable tools:\n"
			} else {
				toolIntro = "# Tool Use\n\nYou can call the following tools. Use this JSON format for a single tool call:\n```tool_call\n{\"name\": \"func_name\", \"arguments\": {...}}\n```\n\nFor multiple simultaneous calls, output several blocks one after another. When calling tools, output ONLY the ```tool_call blocks.\n\nAvailable tools:\n"
			}
			parts = append(parts, toolIntro+string(toolDefsJSON))
		}
	}

	for _, msg := range messages {
		role := msg.Role
		contentStr := ""

		switch val := msg.Content.(type) {
		case string:
			contentStr = val
		case []interface{}:
			var textParts []string
			for _, item := range val {
				if m, ok := item.(map[string]interface{}); ok {
					t, _ := m["type"].(string)
					switch t {
					case "text", "input_text":
						txt, _ := m["text"].(string)
						textParts = append(textParts, txt)
					case "image_url":
						if urlMap, ok := m["image_url"].(map[string]interface{}); ok {
							if urlStr, ok := urlMap["url"].(string); ok {
								if strings.HasPrefix(urlStr, "data:") {
									parts := strings.SplitN(urlStr, ",", 2)
									if len(parts) > 1 {
										mime := "image/png"
										header := parts[0]
										if strings.Contains(header, ";") {
											s1 := strings.Split(header, ";")[0]
											mime = strings.TrimPrefix(s1, "data:")
										}
										decoded, err := base64.StdEncoding.DecodeString(parts[1])
										if err == nil {
											images = append(images, ParsedImage{Data: decoded, Mime: mime})
										}
									}
								} else {
									decoded, err := FetchImageBytes(urlStr)
									if err == nil {
										images = append(images, ParsedImage{
											Data: decoded,
											Mime: "image/png",
										})
									}
								}
							}
						}
					case "tool_use", "tool-result", "tool_result":
						// Anthropic-style tool blocks
						name, _ := m["name"].(string)
						input, _ := json.Marshal(m["input"])
						if name != "" {
							textParts = append(textParts, fmt.Sprintf("[Called tool %s with input: %s]", name, string(input)))
						}
						if t == "tool-result" || t == "tool_result" {
							content, _ := m["content"].(string)
							textParts = append(textParts, fmt.Sprintf("[Tool result: %s]", content))
						}
					}
				}
			}
			contentStr = strings.Join(textParts, " ")
		}

		// Handle assistant tool_calls field (OpenAI format)
		toolCallBlocks := ""
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				toolCallBlocks += fmt.Sprintf("```tool_call\n{\"name\": %q, \"arguments\": %s}\n```\n", tc.Function.Name, tc.Function.Arguments)
			}
		}

		switch role {
		case "system":
			parts = append(parts, fmt.Sprintf("[System instruction]: %s", contentStr))
		case "assistant":
			line := fmt.Sprintf("[Assistant]: %s", contentStr)
			if toolCallBlocks != "" {
				if strings.TrimSpace(contentStr) == "" {
					line = "[Assistant]: " + strings.TrimSpace(toolCallBlocks)
				} else {
					line = "[Assistant]: " + strings.TrimSpace(contentStr) + "\n\n" + strings.TrimSpace(toolCallBlocks)
				}
			}
			parts = append(parts, line)
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool result from %s]: %s", msg.ToolName, contentStr))
		default:
			parts = append(parts, contentStr)
		}
	}

		return strings.Join(parts, "\n\n"), images
	}

func ParseToolCalls(text string) (string, []ParsedToolCall) {
	var toolCalls []ParsedToolCall
	pattern := `(?s)\` + "`" + `\` + "`" + `\` + "`" + `tool_call\s*\n(.*?)\n\` + "`" + `\` + "`" + `\` + "`"
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatch(text, -1)

	for i, match := range matches {
		if len(match) > 1 {
			var data struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(match[1])), &data); err == nil {
				argsJSON, _ := json.Marshal(data.Arguments)
				tc := ParsedToolCall{
					ID:   fmt.Sprintf("call_%d", i),
					Type: "function",
				}
				tc.Function.Name = data.Name
				tc.Function.Arguments = string(argsJSON)
				toolCalls = append(toolCalls, tc)
			}
		}
	}

	clean := re.ReplaceAllString(text, "")
	return strings.TrimSpace(clean), toolCalls
}
