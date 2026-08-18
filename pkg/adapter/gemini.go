package adapter

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const synthesizedToolIDPrefix = "gemini_synth_"
const signatureCarrierPrefix = "aadapter-signature-v1:"

var geminiFunctionNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
var synthesizedIDCounter uint64

func anthropicMessagesToGemini(body []byte) ([]byte, error) {
	return anthropicMessagesToGeminiForModel(body, nil, "")
}

func anthropicMessagesToGeminiWithSignatures(body []byte, signatures map[string]string) ([]byte, error) {
	return anthropicMessagesToGeminiForModel(body, signatures, "")
}

func anthropicMessagesToGeminiForModel(body []byte, signatures map[string]string, model string) ([]byte, error) {
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

	contents, err := buildGeminiContents(src, signatures)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("messages must produce at least one Gemini content item")
	}
	dst["contents"] = contents
	if model == "gemini-3.6-flash" {
		last := contents[len(contents)-1].(map[string]interface{})
		if last["role"] == "model" {
			return nil, fmt.Errorf("gemini-3.6-flash does not support an assistant/model message as the final input turn")
		}
	}

	if cfg, ok, err := buildGeminiGenerationConfigForModel(src, model); err != nil {
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
	if container, ok := src["container"]; ok && container != nil {
		return fmt.Errorf("Anthropic container execution is not supported by Gemini conversion")
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

func buildGeminiContents(src map[string]interface{}, signatures map[string]string) ([]interface{}, error) {
	rawMessages, ok := src["messages"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	toolNameByID := map[string]string{}
	mergedSignatures := make(map[string]string, len(signatures))
	for key, signature := range signatures {
		mergedSignatures[key] = signature
	}
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
			if blockType, _ := block["type"].(string); blockType == "redacted_thinking" {
				if key, signature, ok := decodeSignatureCarrier(stringOrEmpty(block["data"])); ok {
					mergedSignatures[key] = signature
				}
				continue
			}
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
		parts, err := convertAnthropicContentToGeminiParts(message["content"], role, toolNameByID, mergedSignatures)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		if len(contents) > 0 {
			previous, _ := contents[len(contents)-1].(map[string]interface{})
			if previous != nil && previous["role"] == geminiRole {
				previousParts, _ := previous["parts"].([]interface{})
				previous["parts"] = append(previousParts, parts...)
				continue
			}
		}
		contents = append(contents, map[string]interface{}{
			"role":  geminiRole,
			"parts": parts,
		})
	}
	return contents, nil
}

func convertAnthropicContentToGeminiParts(content interface{}, role string, toolNameByID map[string]string, signatures map[string]string) ([]interface{}, error) {
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
					part := map[string]interface{}{"text": text}
					signature := thoughtSignature(block)
					if signature == "" {
						signature = signatures[textSignatureKey(text)]
					}
					if signature != "" {
						part["thoughtSignature"] = signature
					}
					parts = append(parts, part)
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
				input, err := objectMapOrEmpty(block["input"])
				if err != nil {
					return nil, fmt.Errorf("tool_use %q input: %w", name, err)
				}
				call := map[string]interface{}{
					"name": name,
					"args": input,
				}
				if id != "" && !strings.HasPrefix(id, synthesizedToolIDPrefix) {
					call["id"] = id
				}
				part := map[string]interface{}{"functionCall": call}
				signature := thoughtSignature(block)
				if signature == "" && id != "" {
					signature = signatures[toolSignatureKey(id)]
					if signature == "" {
						signature = signatures[id]
					}
				}
				if signature != "" {
					part["thoughtSignature"] = signature
				}
				parts = append(parts, part)
			case "tool_result":
				toolUseID, _ := block["tool_use_id"].(string)
				name := toolNameByID[toolUseID]
				if name == "" {
					return nil, fmt.Errorf("unable to resolve Gemini functionResponse.name for tool_use_id %q", toolUseID)
				}
				response, mediaParts, err := normalizeToolResult(block)
				if err != nil {
					return nil, err
				}
				resp := map[string]interface{}{"name": name, "response": response}
				if len(mediaParts) > 0 {
					resp["parts"] = mediaParts
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
	if sourceType == "text" && blockType == "document" {
		text, _ := source["data"].(string)
		if text == "" {
			return nil, fmt.Errorf("document text source missing data")
		}
		return map[string]interface{}{"text": text}, nil
	}
	mimeType, _ := source["media_type"].(string)
	if sourceType == "url" {
		uri, _ := source["url"].(string)
		if uri == "" {
			return nil, fmt.Errorf("%s URL source missing url", blockType)
		}
		parsed, err := url.Parse(uri)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "gs") {
			return nil, fmt.Errorf("invalid %s URL source", blockType)
		}
		if mimeType == "" {
			mimeType = mime.TypeByExtension(path.Ext(parsed.Path))
		}
		if mimeType == "" {
			return nil, fmt.Errorf("%s URL source requires media_type when it cannot be inferred from the URL", blockType)
		}
		return map[string]interface{}{"fileData": map[string]interface{}{"mimeType": mimeType, "fileUri": uri}}, nil
	}
	if sourceType != "base64" {
		return nil, fmt.Errorf("unsupported Gemini %s source type %q", blockType, sourceType)
	}
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
	return buildGeminiGenerationConfigForModel(src, "")
}

func buildGeminiGenerationConfigForModel(src map[string]interface{}, model string) (map[string]interface{}, bool, error) {
	cfg := map[string]interface{}{}
	copyField(src, cfg, "max_tokens", "maxOutputTokens")
	if model != "gemini-3.6-flash" {
		copyField(src, cfg, "temperature", "temperature")
		copyField(src, cfg, "top_p", "topP")
	}
	if model != "gemini-3.5-flash" && model != "gemini-3.6-flash" {
		copyField(src, cfg, "top_k", "topK")
	}
	if model != "gemini-3.5-flash" && model != "gemini-3.6-flash" {
		copyField(src, cfg, "stop_sequences", "stopSequences")
	}

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
		if value, present := outputConfig["effort"]; present {
			effort, ok := value.(string)
			if !ok || effort == "" {
				return "", false, fmt.Errorf("output_config.effort must be a non-empty string")
			}
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
	raw, present := src["tools"]
	if !present {
		return nil, false, nil
	}
	rawTools, ok := raw.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("tools must be an array")
	}
	if len(rawTools) == 0 {
		return nil, false, nil
	}
	var declarations []interface{}
	seenNames := map[string]bool{}
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("tools entries must be objects")
		}
		toolType, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if toolType == "BatchTool" || name == "BatchTool" {
			continue
		}
		if toolType != "" && toolType != "custom" {
			return nil, false, fmt.Errorf("unsupported Anthropic server tool type for Gemini conversion: %s", toolType)
		}
		if !geminiFunctionNamePattern.MatchString(name) {
			return nil, false, fmt.Errorf("invalid Gemini function name %q", name)
		}
		if seenNames[name] {
			return nil, false, fmt.Errorf("duplicate tool name %q", name)
		}
		seenNames[name] = true
		schema, err := ensureObjectSchema(tool["input_schema"])
		if err != nil {
			return nil, false, fmt.Errorf("tool %q input_schema: %w", name, err)
		}
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
	if len(declarations) > 128 {
		return nil, false, fmt.Errorf("Gemini 3.5/3.6 supports at most 128 function declarations")
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
		for key := range v {
			if key != "type" && key != "name" && key != "disable_parallel_tool_use" {
				return nil, false, fmt.Errorf("unsupported tool_choice field for Gemini conversion: %s", key)
			}
		}
		if disable, ok := v["disable_parallel_tool_use"].(bool); ok && disable {
			return nil, false, fmt.Errorf("tool_choice.disable_parallel_tool_use=true cannot be enforced by Vertex Gemini")
		} else if _, present := v["disable_parallel_tool_use"]; present && !ok {
			return nil, false, fmt.Errorf("tool_choice.disable_parallel_tool_use must be a boolean")
		}
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
			if !geminiFunctionNamePattern.MatchString(name) {
				return nil, false, fmt.Errorf("tool_choice tool has invalid name %q", name)
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
	converted, _, err := geminiResponseToAnthropicWithSignatures(body)
	return converted, err
}

func geminiResponseToAnthropicWithSignatures(body []byte) ([]byte, map[string]string, error) {
	return geminiResponseToAnthropicWithSignaturesAndStops(body, nil)
}

func geminiResponseToAnthropicWithSignaturesAndStops(body []byte, stopSequences []string) ([]byte, map[string]string, error) {
	return geminiResponseToAnthropicWithDefaults(body, stopSequences, "")
}

func geminiResponseToAnthropicWithDefaults(body []byte, stopSequences []string, fallbackModel string) ([]byte, map[string]string, error) {
	var src map[string]interface{}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, nil, err
	}
	if len(firstCandidate(src)) == 0 {
		feedback, _ := src["promptFeedback"].(map[string]interface{})
		if feedback == nil || stringOrEmpty(feedback["blockReason"]) == "" {
			return nil, nil, fmt.Errorf("Gemini response contained no candidates")
		}
	}
	if err := validateGeminiCandidate(firstCandidate(src)); err != nil {
		return nil, nil, err
	}
	response, signatures := geminiObjectToAnthropicWithSignatures(src)
	if stringOrEmpty(response["id"]) == "" {
		response["id"] = synthesizeMessageID()
	}
	if stringOrEmpty(response["model"]) == "" {
		response["model"] = fallbackModel
	}
	applyAnthropicStopSequences(response, stopSequences)
	converted, err := json.Marshal(response)
	if err != nil {
		return nil, nil, err
	}
	return converted, signatures, nil
}

func geminiObjectToAnthropic(src map[string]interface{}) map[string]interface{} {
	response, _ := geminiObjectToAnthropicWithSignatures(src)
	return response
}

func geminiObjectToAnthropicWithSignatures(src map[string]interface{}) (map[string]interface{}, map[string]string) {
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
			}, nil
		}
	}
	candidate := firstCandidate(src)
	content := []interface{}{}
	signatures := map[string]string{}
	hasToolUse := false
	contentValue, _ := candidate["content"].(map[string]interface{})
	if parts, _ := contentValue["parts"].([]interface{}); parts != nil {
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]interface{})
			if thought, _ := part["thought"].(bool); thought {
				continue
			}
			if text, hasText := part["text"].(string); hasText && text != "" {
				block := map[string]interface{}{"type": "text", "text": text}
				if signature := thoughtSignature(part); signature != "" {
					block["thought_signature"] = signature
					signatures[textSignatureKey(text)] = signature
				}
				content = append(content, block)
			} else if hasText {
				if signature := thoughtSignature(part); signature != "" && len(content) > 0 {
					previous, _ := content[len(content)-1].(map[string]interface{})
					if previous != nil && stringOrEmpty(previous["type"]) == "text" {
						previous["thought_signature"] = signature
						key := textSignatureKey(stringOrEmpty(previous["text"]))
						signatures[key] = signature
					}
				}
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
					signatures[toolSignatureKey(id)] = signature
				}
				if signature := thoughtSignature(part); signature != "" {
					content = append(content, signatureCarrierBlock(toolSignatureKey(id), signature))
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
	}, signatures
}

func validateGeminiCandidate(candidate map[string]interface{}) error {
	if len(candidate) == 0 {
		return nil
	}
	contentValue, present := candidate["content"]
	if !present {
		return nil
	}
	content, ok := contentValue.(map[string]interface{})
	if !ok {
		return fmt.Errorf("Gemini candidate content must be an object")
	}
	partsValue, present := content["parts"]
	if !present {
		return nil
	}
	parts, ok := partsValue.([]interface{})
	if !ok {
		return fmt.Errorf("Gemini candidate parts must be an array")
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			return fmt.Errorf("Gemini candidate part must be an object")
		}
		if value, present := part["text"]; present {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("Gemini text part must contain a string")
			}
		}
		if value, present := part["functionCall"]; present {
			call, ok := value.(map[string]interface{})
			if !ok {
				return fmt.Errorf("Gemini functionCall must be an object")
			}
			if stringOrEmpty(call["name"]) == "" {
				return fmt.Errorf("Gemini functionCall missing name")
			}
			if args, present := call["args"]; present && args != nil {
				if _, ok := args.(map[string]interface{}); !ok {
					return fmt.Errorf("Gemini functionCall args must be an object")
				}
			}
		}
	}
	return nil
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
	case "SAFETY", "RECITATION", "LANGUAGE", "OTHER", "SPII", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY",
		"IMAGE_PROHIBITED_CONTENT", "MODEL_ARMOR", "JAILBREAK", "MALFORMED_FUNCTION_CALL",
		"UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "NO_IMAGE", "IMAGE_RECITATION", "IMAGE_OTHER":
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

func (s *Server) streamGeminiAsAnthropic(w http.ResponseWriter, r io.Reader, sessionID string, stopSequences []string, fallbackModel string) (string, int, error) {
	flusher, _ := w.(http.Flusher)
	captured := newCappedBuffer(s.cfg.MaxDebugCaptureBytes)
	written := 0
	writeEvent := func(event string, payload map[string]interface{}) error {
		line, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		chunk := []byte("event: " + event + "\ndata: " + string(line) + "\n\n")
		n, err := w.Write(chunk)
		written += n
		_, _ = captured.Write(chunk[:n])
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	started := false
	messageID, model := "", ""
	nextIndex := 0
	textOpen, textIndex := false, 0
	currentText := strings.Builder{}
	currentTextSignature := ""
	var latestUsage, latestFinish interface{}
	hasToolUse := false
	stopFilter := newTextStopFilter(stopSequences)
	matchedStop := ""
	emittedCalls := map[string]int{}
	var outputErr error

	startMessage := func() error {
		if started {
			return nil
		}
		started = true
		if messageID == "" {
			messageID = synthesizeMessageID()
		}
		if model == "" {
			model = fallbackModel
		}
		return writeEvent("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id": messageID, "type": "message", "role": "assistant", "content": []interface{}{},
				"model": model, "stop_reason": nil, "stop_sequence": nil, "usage": buildAnthropicUsage(latestUsage),
			},
		})
	}
	closeText := func() error {
		if !textOpen {
			return nil
		}
		key := textSignatureKey(currentText.String())
		if currentTextSignature != "" {
			s.signatures.remember(sessionID, map[string]string{key: currentTextSignature})
		}
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIndex}); err != nil {
			return err
		}
		currentText.Reset()
		currentTextSignature = ""
		textOpen = false
		return nil
	}
	emitText := func(text, signature string) error {
		if text == "" {
			return nil
		}
		text, matched := stopFilter.Feed(text)
		if matched != "" {
			matchedStop = matched
		}
		if text == "" {
			return nil
		}
		if err := startMessage(); err != nil {
			return err
		}
		if !textOpen {
			textIndex = nextIndex
			nextIndex++
			block := map[string]interface{}{"type": "text", "text": ""}
			if signature != "" {
				block["thought_signature"] = signature
			}
			if err := writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": textIndex, "content_block": block}); err != nil {
				return err
			}
			textOpen = true
		}
		currentText.WriteString(text)
		if signature != "" {
			currentTextSignature = signature
		}
		return writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": textIndex, "delta": map[string]interface{}{"type": "text_delta", "text": text}})
	}
	emitTool := func(call, part map[string]interface{}) error {
		if matchedStop != "" {
			return nil
		}
		if err := startMessage(); err != nil {
			return err
		}
		if err := closeText(); err != nil {
			return err
		}
		id := stringOrEmpty(call["id"])
		if id == "" {
			id = synthesizeToolID()
		}
		name := stringOrEmpty(call["name"])
		if name == "" {
			return fmt.Errorf("Gemini functionCall missing name")
		}
		input := objectOrEmpty(call["args"])
		signature := thoughtSignature(part)
		if signature != "" {
			s.signatures.remember(sessionID, map[string]string{toolSignatureKey(id): signature})
		}
		if signature != "" {
			carrierIndex := nextIndex
			nextIndex++
			carrier := signatureCarrierBlock(toolSignatureKey(id), signature)
			if err := writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": carrierIndex, "content_block": carrier}); err != nil {
				return err
			}
			if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": carrierIndex}); err != nil {
				return err
			}
		}
		index := nextIndex
		nextIndex++
		block := map[string]interface{}{"type": "tool_use", "id": id, "name": name, "input": map[string]interface{}{}}
		if signature != "" {
			block["thought_signature"] = signature
		}
		if err := writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": index, "content_block": block}); err != nil {
			return err
		}
		partial, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if err := writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": index, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(partial)}}); err != nil {
			return err
		}
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		hasToolUse = true
		return nil
	}

	processData := func(data string) error {
		if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode Gemini SSE event: %w", err)
		}
		if upstreamError, _ := chunk["error"].(map[string]interface{}); upstreamError != nil {
			message := stringOrEmpty(upstreamError["message"])
			if message == "" {
				message = "Gemini stream returned an error"
			}
			return fmt.Errorf("Gemini stream error: %s", message)
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
		if feedback, _ := chunk["promptFeedback"].(map[string]interface{}); feedback != nil {
			if reason := stringOrEmpty(feedback["blockReason"]); reason != "" {
				latestFinish = "SAFETY"
				return emitText("Request blocked by Gemini safety filters: "+reason, "")
			}
		}
		candidate := firstCandidate(chunk)
		if err := validateGeminiCandidate(candidate); err != nil {
			return err
		}
		if finish, ok := candidate["finishReason"]; ok {
			latestFinish = finish
		}
		content, _ := candidate["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		chunkOccurrences := map[string]int{}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				return fmt.Errorf("Gemini SSE part must be an object")
			}
			if thought, _ := part["thought"].(bool); thought {
				continue
			}
			if text := stringOrEmpty(part["text"]); text != "" {
				if err := emitText(text, thoughtSignature(part)); err != nil {
					return err
				}
			} else if _, hasText := part["text"]; hasText && thoughtSignature(part) != "" && textOpen {
				currentTextSignature = thoughtSignature(part)
			}
			if call, _ := part["functionCall"].(map[string]interface{}); call != nil {
				fingerprint := stableToolCallKey(call) + ":" + thoughtSignature(part)
				chunkOccurrences[fingerprint]++
				if chunkOccurrences[fingerprint] <= emittedCalls[fingerprint] {
					continue
				}
				if err := emitTool(call, part); err != nil {
					return err
				}
				emittedCalls[fingerprint] = chunkOccurrences[fingerprint]
			}
		}
		return nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), s.cfg.MaxStreamEventBytes)
	var dataLines []string
	eventBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if len(dataLines) > 0 {
				if err := processData(strings.Join(dataLines, "\n")); err != nil {
					outputErr = err
					break
				}
				dataLines = nil
				eventBytes = 0
			}
			continue
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			eventBytes += len(data)
			if eventBytes > s.cfg.MaxStreamEventBytes {
				outputErr = fmt.Errorf("Gemini SSE event exceeds configured size limit")
				break
			}
			dataLines = append(dataLines, data)
		}
	}
	if outputErr == nil && len(dataLines) > 0 {
		outputErr = processData(strings.Join(dataLines, "\n"))
	}
	if outputErr == nil {
		outputErr = scanner.Err()
	}
	if outputErr != nil {
		_ = writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": outputErr.Error()}})
		return captured.String(), written, outputErr
	}
	if matchedStop == "" {
		if tail := stopFilter.Flush(); tail != "" {
			sequences := stopFilter.sequences
			stopFilter.sequences = nil
			if err := emitText(tail, ""); err != nil {
				return captured.String(), written, err
			}
			stopFilter.sequences = sequences
		}
	}
	if err := startMessage(); err != nil {
		return captured.String(), written, err
	}
	if err := closeText(); err != nil {
		return captured.String(), written, err
	}
	usage := buildAnthropicUsage(latestUsage)
	stopReason := mapGeminiFinishReason(latestFinish, hasToolUse)
	var stopSequence interface{}
	if matchedStop != "" {
		stopReason, stopSequence = "stop_sequence", matchedStop
	}
	if err := writeEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": stopSequence},
		"usage": map[string]interface{}{"output_tokens": usage["output_tokens"]},
	}); err != nil {
		return captured.String(), written, err
	}
	if err := writeEvent("message_stop", map[string]interface{}{"type": "message_stop"}); err != nil {
		return captured.String(), written, err
	}
	return captured.String(), written, nil
}

func applyAnthropicStopSequences(response map[string]interface{}, sequences []string) {
	if len(sequences) == 0 {
		return
	}
	blocks, _ := response["content"].([]interface{})
	filter := newTextStopFilter(sequences)
	result := make([]interface{}, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]interface{})
		if block == nil || stringOrEmpty(block["type"]) != "text" {
			if tail := filter.Flush(); tail != "" && len(result) > 0 {
				if previous, _ := result[len(result)-1].(map[string]interface{}); stringOrEmpty(previous["type"]) == "text" {
					previous["text"] = stringOrEmpty(previous["text"]) + tail
				}
			}
			result = append(result, rawBlock)
			continue
		}
		text := stringOrEmpty(block["text"])
		visible, matched := filter.Feed(text)
		copyBlock := make(map[string]interface{}, len(block))
		for key, value := range block {
			copyBlock[key] = value
		}
		copyBlock["text"] = visible
		result = append(result, copyBlock)
		if matched != "" {
			response["content"] = result
			response["stop_reason"] = "stop_sequence"
			response["stop_sequence"] = matched
			return
		}
	}
	if tail := filter.Flush(); tail != "" && len(result) > 0 {
		if previous, _ := result[len(result)-1].(map[string]interface{}); stringOrEmpty(previous["type"]) == "text" {
			previous["text"] = stringOrEmpty(previous["text"]) + tail
		}
	}
	response["content"] = result
}

func earliestStop(text string, sequences []string) (int, string) {
	best := -1
	matched := ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (best < 0 || index < best) {
			best, matched = index, sequence
		}
	}
	return best, matched
}

type textStopFilter struct {
	sequences []string
	pending   string
	stopped   bool
}

func newTextStopFilter(sequences []string) *textStopFilter {
	return &textStopFilter{sequences: sequences}
}

func (f *textStopFilter) Feed(text string) (string, string) {
	if f.stopped {
		return "", ""
	}
	combined := f.pending + text
	if index, matched := earliestStop(combined, f.sequences); index >= 0 {
		f.pending = ""
		f.stopped = true
		return combined[:index], matched
	}
	keep := 0
	for _, sequence := range f.sequences {
		for length := 1; length < len(sequence) && length <= len(combined); length++ {
			if strings.HasSuffix(combined, sequence[:length]) && length > keep {
				keep = length
			}
		}
	}
	output := combined[:len(combined)-keep]
	f.pending = combined[len(combined)-keep:]
	return output, ""
}

func (f *textStopFilter) Flush() string {
	if f.stopped {
		return ""
	}
	text := f.pending
	f.pending = ""
	return text
}

func copyField(src, dst map[string]interface{}, from, to string) {
	if value, ok := src[from]; ok {
		dst[to] = value
	}
}

func ensureObjectSchema(value interface{}) (map[string]interface{}, error) {
	if value == nil {
		schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		return schema, nil
	}
	schema, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	if len(schema) == 0 {
		schema = map[string]interface{}{}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if schemaType, ok := schema["type"].(string); !ok || schemaType != "object" {
		return nil, fmt.Errorf("top-level type must be object")
	} else {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]interface{}{}
		}
	}
	return schema, nil
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

func normalizeToolResult(block map[string]interface{}) (map[string]interface{}, []interface{}, error) {
	value := block["content"]
	isError := false
	if raw, present := block["is_error"]; present {
		var ok bool
		isError, ok = raw.(bool)
		if !ok {
			return nil, nil, fmt.Errorf("tool_result is_error must be a boolean")
		}
	}
	responseKey := "output"
	if isError {
		responseKey = "error"
	}
	var content interface{}
	var mediaParts []interface{}
	switch v := value.(type) {
	case string:
		content = v
	case []interface{}:
		var texts []string
		for _, rawBlock := range v {
			contentBlock, ok := rawBlock.(map[string]interface{})
			if !ok {
				return nil, nil, fmt.Errorf("tool_result content blocks must be objects")
			}
			switch blockType, _ := contentBlock["type"].(string); blockType {
			case "text":
				if text, _ := contentBlock["text"].(string); text != "" {
					texts = append(texts, text)
				}
			case "image", "document":
				part, err := anthropicMediaBlockToGeminiPart(contentBlock, blockType)
				if err != nil {
					return nil, nil, err
				}
				if inline, ok := part["inlineData"]; ok {
					mediaParts = append(mediaParts, map[string]interface{}{"inlineData": inline})
				} else if file, ok := part["fileData"]; ok {
					mediaParts = append(mediaParts, map[string]interface{}{"fileData": file})
				} else if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			default:
				return nil, nil, fmt.Errorf("unsupported tool_result content block: %s", blockType)
			}
		}
		if len(texts) > 0 {
			content = strings.Join(texts, "\n")
		} else {
			content = ""
		}
	case nil:
		content = ""
	default:
		return nil, nil, fmt.Errorf("tool_result content must be a string or content block array")
	}
	return map[string]interface{}{responseKey: content}, mediaParts, nil
}

func objectOrEmpty(value interface{}) interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	return value
}

func objectMapOrEmpty(value interface{}) (map[string]interface{}, error) {
	if value == nil {
		return map[string]interface{}{}, nil
	}
	object, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	return object, nil
}

func stringOrEmpty(value interface{}) string {
	got, _ := value.(string)
	return got
}

func synthesizeToolID() string {
	return fmt.Sprintf("%s%d_%d", synthesizedToolIDPrefix, time.Now().UnixNano(), atomic.AddUint64(&synthesizedIDCounter, 1))
}

func synthesizeMessageID() string {
	return fmt.Sprintf("msg_gemini_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&synthesizedIDCounter, 1))
}

func toolSignatureKey(id string) string { return "tool:" + id }

func textSignatureKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("text:%x", sum[:])
}

type signatureCarrier struct {
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

func signatureCarrierBlock(key, signature string) map[string]interface{} {
	payload, _ := json.Marshal(signatureCarrier{Key: key, Signature: signature})
	return map[string]interface{}{
		"type": "redacted_thinking",
		"data": signatureCarrierPrefix + base64.RawURLEncoding.EncodeToString(payload),
	}
}

func decodeSignatureCarrier(data string) (string, string, bool) {
	encoded, ok := strings.CutPrefix(data, signatureCarrierPrefix)
	if !ok {
		return "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	var carrier signatureCarrier
	if json.Unmarshal(payload, &carrier) != nil || carrier.Key == "" || carrier.Signature == "" {
		return "", "", false
	}
	return carrier.Key, carrier.Signature, true
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
