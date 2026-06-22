package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const synthesizedToolIDPrefix = "gemini_synth_"

func anthropicMessagesToGemini(body []byte) ([]byte, error) {
	var src map[string]interface{}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	if err := validateGeminiTopLevelFields(src); err != nil {
		return nil, err
	}

	dst := map[string]interface{}{}
	if system, ok, err := buildGeminiSystemInstruction(src); err != nil {
		return nil, err
	} else if ok {
		dst["systemInstruction"] = system
	}

	contents, err := buildGeminiContents(src)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("messages must produce at least one Gemini content item")
	}
	dst["contents"] = contents

	if cfg, ok, err := buildGeminiGenerationConfig(src); err != nil {
		return nil, err
	} else if ok {
		dst["generationConfig"] = cfg
	}
	if tools, ok, err := buildGeminiTools(src); err != nil {
		return nil, err
	} else if ok {
		dst["tools"] = tools
	}
	if toolConfig, ok, err := buildGeminiToolConfig(src); err != nil {
		return nil, err
	} else if ok {
		dst["toolConfig"] = toolConfig
	}

	return json.Marshal(dst)
}

func validateGeminiTopLevelFields(src map[string]interface{}) error {
	allowed := map[string]bool{
		"anthropic_version":  true,
		"container":          true,
		"context_management": true,
		"max_tokens":         true,
		"messages":           true,
		"metadata":           true,
		"model":              true,
		"output_config":      true,
		"service_tier":       true,
		"stop_sequences":     true,
		"stream":             true,
		"system":             true,
		"temperature":        true,
		"thinking":           true,
		"tool_choice":        true,
		"tools":              true,
		"top_k":              true,
		"top_p":              true,
	}
	for key := range src {
		if !allowed[key] {
			return fmt.Errorf("unsupported Anthropic field for Gemini conversion: %s", key)
		}
	}
	return nil
}

func buildGeminiSystemInstruction(src map[string]interface{}) (map[string]interface{}, bool, error) {
	var texts []string
	if system, ok := src["system"]; ok {
		got, err := collectAnthropicTexts(system, "system")
		if err != nil {
			return nil, false, err
		}
		texts = append(texts, got...)
	}
	messages, _ := src["messages"].([]interface{})
	for _, item := range messages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("messages entries must be objects")
		}
		if role, _ := message["role"].(string); role == "system" {
			got, err := collectAnthropicTexts(message["content"], "system message")
			if err != nil {
				return nil, false, err
			}
			texts = append(texts, got...)
		}
	}
	if len(texts) == 0 {
		return nil, false, nil
	}
	return map[string]interface{}{
		"parts": []interface{}{
			map[string]interface{}{"text": strings.Join(texts, "\n\n")},
		},
	}, true, nil
}

func collectAnthropicTexts(value interface{}, field string) ([]string, error) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []interface{}:
		var texts []string
		for _, item := range v {
			block, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("%s content blocks must be objects", field)
			}
			blockType, _ := block["type"].(string)
			if blockType != "" && blockType != "text" {
				return nil, fmt.Errorf("%s only supports text blocks for Gemini systemInstruction", field)
			}
			text, _ := block["text"].(string)
			if text != "" {
				texts = append(texts, text)
			}
		}
		return texts, nil
	default:
		return nil, fmt.Errorf("%s must be a string or text block array", field)
	}
}

func buildGeminiContents(src map[string]interface{}) ([]interface{}, error) {
	rawMessages, ok := src["messages"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	toolNameByID := map[string]string{}
	for _, item := range rawMessages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("messages entries must be objects")
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		blocks, _ := message["content"].([]interface{})
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]interface{})
			if blockType, _ := block["type"].(string); blockType != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id != "" && name != "" {
				toolNameByID[id] = name
			}
		}
	}

	var contents []interface{}
	for _, item := range rawMessages {
		message, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("messages entries must be objects")
		}
		role, _ := message["role"].(string)
		if role == "system" {
			continue
		}
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		} else if role != "user" {
			return nil, fmt.Errorf("unsupported message role for Gemini conversion: %s", role)
		}
		parts, err := convertAnthropicContentToGeminiParts(message["content"], role, toolNameByID)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, map[string]interface{}{
			"role":  geminiRole,
			"parts": parts,
		})
	}
	return contents, nil
}

func convertAnthropicContentToGeminiParts(content interface{}, role string, toolNameByID map[string]string) ([]interface{}, error) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		return []interface{}{map[string]interface{}{"text": v}}, nil
	case []interface{}:
		var parts []interface{}
		for _, rawBlock := range v {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("message content blocks must be objects")
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				text, _ := block["text"].(string)
				if text != "" {
					parts = append(parts, map[string]interface{}{"text": text})
				}
			case "image", "document":
				part, err := anthropicMediaBlockToGeminiPart(block, blockType)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			case "tool_use":
				if role != "assistant" {
					return nil, fmt.Errorf("tool_use blocks are only valid in assistant messages")
				}
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if name == "" {
					return nil, fmt.Errorf("tool_use block missing name")
				}
				if id != "" {
					toolNameByID[id] = name
				}
				call := map[string]interface{}{
					"name": name,
					"args": objectOrEmpty(block["input"]),
				}
				if id != "" && !strings.HasPrefix(id, synthesizedToolIDPrefix) {
					call["id"] = id
				}
				part := map[string]interface{}{"functionCall": call}
				if signature := thoughtSignature(block); signature != "" {
					part["thoughtSignature"] = signature
				}
				parts = append(parts, part)
			case "tool_result":
				toolUseID, _ := block["tool_use_id"].(string)
				name := toolNameByID[toolUseID]
				if name == "" {
					return nil, fmt.Errorf("unable to resolve Gemini functionResponse.name for tool_use_id %q", toolUseID)
				}
				resp := map[string]interface{}{
					"name":     name,
					"response": normalizeToolResult(block["content"]),
				}
				if toolUseID != "" && !strings.HasPrefix(toolUseID, synthesizedToolIDPrefix) {
					resp["id"] = toolUseID
				}
				parts = append(parts, map[string]interface{}{"functionResponse": resp})
			case "thinking", "redacted_thinking":
			default:
				return nil, fmt.Errorf("unsupported Anthropic content block for Gemini conversion: %s", blockType)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("message content must be a string or array")
	}
}

func anthropicMediaBlockToGeminiPart(block map[string]interface{}, blockType string) (map[string]interface{}, error) {
	source, ok := block["source"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s block missing source", blockType)
	}
	sourceType, _ := source["type"].(string)
	if sourceType != "base64" {
		return nil, fmt.Errorf("Gemini conversion only supports base64 %s sources, got %q", blockType, sourceType)
	}
	mimeType, _ := source["media_type"].(string)
	if mimeType == "" {
		if blockType == "document" {
			mimeType = "application/pdf"
		} else {
			mimeType = "image/png"
		}
	}
	data, _ := source["data"].(string)
	if data == "" {
		return nil, fmt.Errorf("%s block source missing data", blockType)
	}
	return map[string]interface{}{
		"inlineData": map[string]interface{}{
			"mimeType": mimeType,
			"data":     data,
		},
	}, nil
}

func buildGeminiGenerationConfig(src map[string]interface{}) (map[string]interface{}, bool, error) {
	cfg := map[string]interface{}{}
	copyField(src, cfg, "max_tokens", "maxOutputTokens")
	copyField(src, cfg, "temperature", "temperature")
	copyField(src, cfg, "top_p", "topP")
	copyField(src, cfg, "top_k", "topK")
	copyField(src, cfg, "stop_sequences", "stopSequences")

	level, ok, err := resolveGeminiThinkingLevel(src)
	if err != nil {
		return nil, false, err
	}
	if ok {
		cfg["thinkingConfig"] = map[string]interface{}{
			"thinkingLevel": level,
		}
	}
	if len(cfg) == 0 {
		return nil, false, nil
	}
	return cfg, true, nil
}

func resolveGeminiThinkingLevel(src map[string]interface{}) (string, bool, error) {
	if raw, ok := src["output_config"]; ok {
		outputConfig, ok := raw.(map[string]interface{})
		if !ok {
			return "", false, fmt.Errorf("output_config must be an object")
		}
		for key := range outputConfig {
			if key != "effort" {
				return "", false, fmt.Errorf("unsupported output_config field for Gemini conversion: %s", key)
			}
		}
		if effort, _ := outputConfig["effort"].(string); effort != "" {
			level, ok := mapEffortToGeminiThinkingLevel(effort)
			if !ok {
				return "", false, fmt.Errorf("unsupported output_config.effort for Gemini conversion: %s", effort)
			}
			return level, true, nil
		}
	}
	if raw, ok := src["thinking"]; ok {
		thinking, ok := raw.(map[string]interface{})
		if !ok {
			return "", false, fmt.Errorf("thinking must be an object")
		}
		thinkingType, _ := thinking["type"].(string)
		switch thinkingType {
		case "", "disabled":
			return "", false, nil
		case "adaptive":
			return "HIGH", true, nil
		case "enabled":
			budget, _ := numberAsFloat(thinking["budget_tokens"])
			switch {
			case budget > 0 && budget <= 4096:
				return "LOW", true, nil
			case budget > 0 && budget <= 16000:
				return "MEDIUM", true, nil
			default:
				return "HIGH", true, nil
			}
		default:
			return "", false, fmt.Errorf("unsupported thinking.type for Gemini conversion: %s", thinkingType)
		}
	}
	return "", false, nil
}

func mapEffortToGeminiThinkingLevel(effort string) (string, bool) {
	switch strings.ToLower(effort) {
	case "minimal":
		return "MINIMAL", true
	case "low":
		return "LOW", true
	case "medium":
		return "MEDIUM", true
	case "high", "max", "xhigh":
		return "HIGH", true
	default:
		return "", false
	}
}

func buildGeminiTools(src map[string]interface{}) ([]interface{}, bool, error) {
	rawTools, ok := src["tools"].([]interface{})
	if !ok || len(rawTools) == 0 {
		return nil, false, nil
	}
	var declarations []interface{}
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("tools entries must be objects")
		}
		if toolType, _ := tool["type"].(string); toolType == "BatchTool" {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, false, fmt.Errorf("tool missing name")
		}
		schema := ensureObjectSchema(tool["input_schema"])
		declaration := map[string]interface{}{
			"name":        name,
			"description": stringOrEmpty(tool["description"]),
		}
		if requiresGeminiParametersJSONSchema(schema) {
			declaration["parametersJsonSchema"] = schema
		} else {
			declaration["parameters"] = schema
		}
		declarations = append(declarations, declaration)
	}
	if len(declarations) == 0 {
		return nil, false, nil
	}
	return []interface{}{map[string]interface{}{"functionDeclarations": declarations}}, true, nil
}

func buildGeminiToolConfig(src map[string]interface{}) (map[string]interface{}, bool, error) {
	choice, ok := src["tool_choice"]
	if !ok {
		return nil, false, nil
	}
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]interface{}{"functionCallingConfig": map[string]interface{}{"mode": "AUTO"}}, true, nil
		case "none":
			return map[string]interface{}{"functionCallingConfig": map[string]interface{}{"mode": "NONE"}}, true, nil
		default:
			return nil, false, fmt.Errorf("unsupported tool_choice string for Gemini conversion: %s", v)
		}
	case map[string]interface{}:
		choiceType, _ := v["type"].(string)
		switch choiceType {
		case "auto":
			return map[string]interface{}{"functionCallingConfig": map[string]interface{}{"mode": "AUTO"}}, true, nil
		case "none":
			return map[string]interface{}{"functionCallingConfig": map[string]interface{}{"mode": "NONE"}}, true, nil
		case "any":
			return map[string]interface{}{"functionCallingConfig": map[string]interface{}{"mode": "ANY"}}, true, nil
		case "tool":
			name, _ := v["name"].(string)
			if name == "" {
				return nil, false, fmt.Errorf("tool_choice tool missing name")
			}
			return map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{
					"mode":                 "ANY",
					"allowedFunctionNames": []interface{}{name},
				},
			}, true, nil
		default:
			return nil, false, fmt.Errorf("unsupported tool_choice type for Gemini conversion: %s", choiceType)
		}
	default:
		return nil, false, fmt.Errorf("tool_choice must be a string or object")
	}
}

func geminiResponseToAnthropic(body []byte) ([]byte, error) {
	var src map[string]interface{}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	response := geminiObjectToAnthropic(src)
	return json.Marshal(response)
}

func geminiObjectToAnthropic(src map[string]interface{}) map[string]interface{} {
	if feedback, _ := src["promptFeedback"].(map[string]interface{}); feedback != nil {
		if reason, _ := feedback["blockReason"].(string); reason != "" {
			return map[string]interface{}{
				"id":            stringOrEmpty(src["responseId"]),
				"type":          "message",
				"role":          "assistant",
				"model":         stringOrEmpty(src["modelVersion"]),
				"content":       []interface{}{map[string]interface{}{"type": "text", "text": "Request blocked by Gemini safety filters: " + reason}},
				"stop_reason":   "refusal",
				"stop_sequence": nil,
				"usage":         buildAnthropicUsage(src["usageMetadata"]),
			}
		}
	}
	candidate := firstCandidate(src)
	content := []interface{}{}
	hasToolUse := false
	contentValue, _ := candidate["content"].(map[string]interface{})
	if parts, _ := contentValue["parts"].([]interface{}); parts != nil {
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]interface{})
			if thought, _ := part["thought"].(bool); thought {
				continue
			}
			if text, _ := part["text"].(string); text != "" {
				content = append(content, map[string]interface{}{"type": "text", "text": text})
			}
			if call, _ := part["functionCall"].(map[string]interface{}); call != nil {
				hasToolUse = true
				id, _ := call["id"].(string)
				if id == "" {
					id = synthesizeToolID()
				}
				block := map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  stringOrEmpty(call["name"]),
					"input": objectOrEmpty(call["args"]),
				}
				if signature := thoughtSignature(part); signature != "" {
					block["thought_signature"] = signature
				}
				content = append(content, block)
			}
		}
	}
	return map[string]interface{}{
		"id":            stringOrEmpty(src["responseId"]),
		"type":          "message",
		"role":          "assistant",
		"model":         stringOrEmpty(src["modelVersion"]),
		"content":       content,
		"stop_reason":   mapGeminiFinishReason(candidate["finishReason"], hasToolUse),
		"stop_sequence": nil,
		"usage":         buildAnthropicUsage(src["usageMetadata"]),
	}
}

func firstCandidate(src map[string]interface{}) map[string]interface{} {
	candidates, _ := src["candidates"].([]interface{})
	if len(candidates) == 0 {
		return map[string]interface{}{}
	}
	candidate, _ := candidates[0].(map[string]interface{})
	if candidate == nil {
		return map[string]interface{}{}
	}
	return candidate
}

func mapGeminiFinishReason(value interface{}, hasToolUse bool) string {
	reason, _ := value.(string)
	switch reason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "SPII", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "refusal"
	default:
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	}
}

func buildAnthropicUsage(value interface{}) map[string]interface{} {
	usage, _ := value.(map[string]interface{})
	prompt := uint64Number(usage["promptTokenCount"])
	cached := uint64Number(usage["cachedContentTokenCount"])
	total := uint64Number(usage["totalTokenCount"])
	if total == 0 {
		total = prompt + uint64Number(usage["candidatesTokenCount"]) + uint64Number(usage["thoughtsTokenCount"])
	}
	input := prompt
	if cached <= input {
		input -= cached
	}
	output := uint64(0)
	if total >= prompt {
		output = total - prompt
	}
	result := map[string]interface{}{
		"input_tokens":  input,
		"output_tokens": output,
	}
	if cached > 0 {
		result["cache_read_input_tokens"] = cached
	}
	return result
}

func geminiCountTokensToAnthropic(body []byte) ([]byte, error) {
	var src map[string]interface{}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	total := uint64Number(src["totalTokens"])
	return json.Marshal(map[string]interface{}{"input_tokens": total})
}

func (s *Server) streamGeminiAsAnthropic(w http.ResponseWriter, r io.Reader) ([]byte, int) {
	flusher, _ := w.(http.Flusher)
	captured := bytes.NewBuffer(nil)
	writeEvent := func(event string, payload map[string]interface{}) {
		line, _ := json.Marshal(payload)
		chunk := []byte("event: " + event + "\ndata: " + string(line) + "\n\n")
		_, _ = w.Write(chunk)
		_, _ = captured.Write(chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var dataLines []string
	started := false
	messageID := ""
	model := ""
	accumulatedText := ""
	textOpen := false
	textIndex := 0
	nextIndex := 0
	var latestUsage interface{}
	var latestFinish interface{}
	var toolUses []map[string]interface{}
	toolIDByKey := map[string]string{}

	processData := func(data string) {
		if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
			return
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return
		}
		if messageID == "" {
			messageID = stringOrEmpty(chunk["responseId"])
		}
		if model == "" {
			model = stringOrEmpty(chunk["modelVersion"])
		}
		if usage, ok := chunk["usageMetadata"]; ok {
			latestUsage = usage
		}
		if !started {
			writeEvent("message_start", map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":    messageID,
					"type":  "message",
					"role":  "assistant",
					"model": model,
					"usage": buildAnthropicUsage(latestUsage),
				},
			})
			started = true
		}
		candidate := firstCandidate(chunk)
		if finish, ok := candidate["finishReason"]; ok {
			latestFinish = finish
		}
		content, _ := candidate["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		visibleText := strings.Builder{}
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]interface{})
			if thought, _ := part["thought"].(bool); thought {
				continue
			}
			if text, _ := part["text"].(string); text != "" {
				visibleText.WriteString(text)
			}
			if call, _ := part["functionCall"].(map[string]interface{}); call != nil {
				id, _ := call["id"].(string)
				if id == "" {
					key := stableToolCallKey(call)
					id = toolIDByKey[key]
					if id == "" {
						id = synthesizeToolID()
						toolIDByKey[key] = id
					}
				}
				toolUses = append(toolUses, map[string]interface{}{
					"id":                id,
					"name":              stringOrEmpty(call["name"]),
					"input":             objectOrEmpty(call["args"]),
					"thought_signature": thoughtSignature(part),
				})
			}
		}
		text := visibleText.String()
		if text != "" {
			delta := text
			if strings.HasPrefix(text, accumulatedText) {
				delta = text[len(accumulatedText):]
				accumulatedText = text
			} else {
				accumulatedText += text
			}
			if delta != "" {
				if !textOpen {
					textIndex = nextIndex
					nextIndex++
					writeEvent("content_block_start", map[string]interface{}{
						"type":          "content_block_start",
						"index":         textIndex,
						"content_block": map[string]interface{}{"type": "text", "text": ""},
					})
					textOpen = true
				}
				writeEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": textIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": delta},
				})
			}
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if len(dataLines) > 0 {
				processData(strings.Join(dataLines, "\n"))
				dataLines = nil
			}
			continue
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(data))
		}
	}
	if len(dataLines) > 0 {
		processData(strings.Join(dataLines, "\n"))
	}
	if !started {
		writeEvent("message_start", map[string]interface{}{
			"type":    "message_start",
			"message": map[string]interface{}{"id": messageID, "type": "message", "role": "assistant", "model": model, "usage": buildAnthropicUsage(latestUsage)},
		})
	}
	if textOpen {
		writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIndex})
	}
	seenTools := map[string]bool{}
	for _, tool := range toolUses {
		key := stringOrEmpty(tool["id"]) + ":" + stringOrEmpty(tool["name"])
		if seenTools[key] {
			continue
		}
		seenTools[key] = true
		index := nextIndex
		nextIndex++
		block := map[string]interface{}{"type": "tool_use", "id": tool["id"], "name": tool["name"]}
		if signature := stringOrEmpty(tool["thought_signature"]); signature != "" {
			block["thought_signature"] = signature
		}
		writeEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         index,
			"content_block": block,
		})
		partial, _ := json.Marshal(tool["input"])
		writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(partial)},
		})
		writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
	}
	writeEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": mapGeminiFinishReason(latestFinish, len(toolUses) > 0), "stop_sequence": nil},
		"usage": buildAnthropicUsage(latestUsage),
	})
	writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
	return captured.Bytes(), captured.Len()
}

func copyField(src, dst map[string]interface{}, from, to string) {
	if value, ok := src[from]; ok {
		dst[to] = value
	}
}

func ensureObjectSchema(value interface{}) map[string]interface{} {
	schema, _ := value.(map[string]interface{})
	if schema == nil {
		schema = map[string]interface{}{}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if schema["type"] == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]interface{}{}
		}
	}
	return schema
}

func requiresGeminiParametersJSONSchema(schema interface{}) bool {
	switch v := schema.(type) {
	case map[string]interface{}:
		for key, value := range v {
			switch key {
			case "type":
				if _, ok := value.([]interface{}); ok {
					return true
				}
			case "format", "title", "description", "nullable", "enum", "maxItems", "minItems",
				"required", "minProperties", "maxProperties", "minLength", "maxLength",
				"pattern", "example", "propertyOrdering", "default", "minimum", "maximum":
			case "properties":
				properties, ok := value.(map[string]interface{})
				if !ok {
					return true
				}
				for _, property := range properties {
					if requiresGeminiParametersJSONSchema(property) {
						return true
					}
				}
			case "items":
				if _, ok := value.(map[string]interface{}); !ok {
					return true
				}
				if requiresGeminiParametersJSONSchema(value) {
					return true
				}
			case "anyOf":
				values, ok := value.([]interface{})
				if !ok {
					return true
				}
				for _, item := range values {
					if requiresGeminiParametersJSONSchema(item) {
						return true
					}
				}
			default:
				return true
			}
		}
	case []interface{}:
		for _, item := range v {
			if requiresGeminiParametersJSONSchema(item) {
				return true
			}
		}
	}
	return false
}

func stableToolCallKey(call map[string]interface{}) string {
	name := stringOrEmpty(call["name"])
	args, _ := json.Marshal(objectOrEmpty(call["args"]))
	return name + ":" + string(args)
}

func thoughtSignature(value map[string]interface{}) string {
	if value == nil {
		return ""
	}
	if signature := stringOrEmpty(value["thoughtSignature"]); signature != "" {
		return signature
	}
	return stringOrEmpty(value["thought_signature"])
}

func normalizeToolResult(value interface{}) map[string]interface{} {
	switch v := value.(type) {
	case string:
		return map[string]interface{}{"content": v}
	case []interface{}:
		var texts []string
		for _, rawBlock := range v {
			block, _ := rawBlock.(map[string]interface{})
			if blockType, _ := block["type"].(string); blockType != "text" {
				continue
			}
			if text, _ := block["text"].(string); text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return map[string]interface{}{"content": strings.Join(texts, "\n")}
		}
		return map[string]interface{}{"content": v}
	case nil:
		return map[string]interface{}{"content": ""}
	default:
		return map[string]interface{}{"content": v}
	}
}

func objectOrEmpty(value interface{}) interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func stringOrEmpty(value interface{}) string {
	got, _ := value.(string)
	return got
}

func synthesizeToolID() string {
	return fmt.Sprintf("%s%d", synthesizedToolIDPrefix, time.Now().UnixNano())
}

func uint64Number(value interface{}) uint64 {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case int:
		if v > 0 {
			return uint64(v)
		}
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case json.Number:
		n, _ := v.Int64()
		if n > 0 {
			return uint64(n)
		}
	}
	return 0
}

func numberAsFloat(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}
