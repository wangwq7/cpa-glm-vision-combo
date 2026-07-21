package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	protocolOpenAIChat = "openai"
	protocolResponses  = "openai-response"
	protocolAnthropic  = "claude"
)

// protocolAdapter owns the wire-format differences that must remain explicit
// while the visual extraction, caching, history, and fallback pipeline stays
// shared across all supported clients.
type protocolAdapter struct {
	protocol                string
	conversationField       string
	imageBlockType          string
	textBlockType           string
	supportsAdditionalTools bool
}

var protocolAdapters = map[string]protocolAdapter{
	protocolOpenAIChat: {
		protocol:          protocolOpenAIChat,
		conversationField: "messages",
		imageBlockType:    "image_url",
		textBlockType:     "text",
	},
	protocolResponses: {
		protocol:                protocolResponses,
		conversationField:       "input",
		imageBlockType:          "input_image",
		textBlockType:           "input_text",
		supportsAdditionalTools: true,
	},
	protocolAnthropic: {
		protocol:          protocolAnthropic,
		conversationField: "messages",
		imageBlockType:    "image",
		textBlockType:     "text",
	},
}

// normalizeProtocol maps host protocol aliases to the canonical values
// declared in the plugin ABI. SourceFormat remains authoritative when present.
func normalizeProtocol(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case protocolOpenAIChat, "openai-chat", "chat", "chat-completions":
		return protocolOpenAIChat
	case protocolResponses, "openai-responses", "response", "responses":
		return protocolResponses
	case protocolAnthropic, "anthropic", "anthropic-messages":
		return protocolAnthropic
	default:
		return value
	}
}

func isSupportedProtocol(value string) bool {
	_, ok := protocolAdapters[normalizeProtocol(value)]
	return ok
}

func adapterForProtocol(value string) (protocolAdapter, error) {
	canonical := normalizeProtocol(value)
	adapter, ok := protocolAdapters[canonical]
	if !ok {
		return protocolAdapter{}, fmt.Errorf("unsupported executor protocol %q", canonical)
	}
	return adapter, nil
}

// resolveProtocolAdapter uses the host's SourceFormat first, then Format. Only
// if both are absent does it inspect the payload. Text-only messages are
// intentionally not guessed because OpenAI Chat and Anthropic are structurally
// ambiguous without endpoint metadata.
func resolveProtocolAdapter(sourceFormat, format string, raw []byte) (protocolAdapter, error) {
	if strings.TrimSpace(sourceFormat) != "" {
		return adapterForProtocol(sourceFormat)
	}
	if strings.TrimSpace(format) != "" {
		return adapterForProtocol(format)
	}
	return detectProtocolAdapter(raw, true)
}

func detectProtocolAdapter(raw []byte, strict bool) (protocolAdapter, error) {
	if len(raw) == 0 {
		return protocolAdapter{}, fmt.Errorf("request protocol is missing and the request body is empty")
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return protocolAdapter{}, fmt.Errorf("request protocol is missing and payload detection failed: %w", err)
	}
	return detectProtocolAdapterFromRoot(root, strict)
}

func detectProtocolAdapterFromRoot(root any, strict bool) (protocolAdapter, error) {
	obj, ok := root.(map[string]any)
	if !ok {
		return protocolAdapter{}, fmt.Errorf("request protocol is missing and top-level JSON is not an object")
	}
	_, hasMessages := obj["messages"].([]any)
	_, inputIsArray := obj["input"].([]any)
	_, inputIsString := obj["input"].(string)
	hasInput := inputIsArray || inputIsString
	if hasInput && !hasMessages {
		return protocolAdapters[protocolResponses], nil
	}

	signals := map[string]bool{}
	collectProtocolSignals(root, signals)
	if hasInput {
		signals[protocolResponses] = true
	}
	if hasMessages {
		if _, ok := obj["system"]; ok {
			signals[protocolAnthropic] = true
		}
		collectToolShapeSignals(obj["tools"], signals)
	}

	selected := ""
	for _, candidate := range []string{protocolOpenAIChat, protocolResponses, protocolAnthropic} {
		if !signals[candidate] {
			continue
		}
		if selected != "" && selected != candidate {
			return protocolAdapter{}, fmt.Errorf("request protocol is missing and payload contains conflicting %s and %s protocol markers", selected, candidate)
		}
		selected = candidate
	}
	if selected != "" {
		return protocolAdapters[selected], nil
	}
	if !strict && hasMessages {
		// Token estimation is protocol-neutral for pure text. OpenAI Chat is a
		// safe structural default only for that best-effort internal path.
		return protocolAdapters[protocolOpenAIChat], nil
	}
	if hasMessages {
		return protocolAdapter{}, fmt.Errorf("request protocol is missing and text-only messages are ambiguous between OpenAI Chat and Anthropic")
	}
	return protocolAdapter{}, fmt.Errorf("request protocol is missing and payload has neither messages nor input")
}

func collectProtocolSignals(value any, signals map[string]bool) {
	switch current := value.(type) {
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(stringValue(current["type"])))
		switch typ {
		case "image_url", "tool_calls":
			signals[protocolOpenAIChat] = true
		case "input_image", "input_text", "additional_tools", "function_call_output":
			signals[protocolResponses] = true
		case "image", "tool_use", "tool_result":
			signals[protocolAnthropic] = true
		}
		for _, child := range current {
			collectProtocolSignals(child, signals)
		}
	case []any:
		for _, child := range current {
			collectProtocolSignals(child, signals)
		}
	}
}

func collectToolShapeSignals(value any, signals map[string]bool) {
	tools, _ := value.([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if _, ok := tool["function"].(map[string]any); ok {
			signals[protocolOpenAIChat] = true
		}
		if _, ok := tool["input_schema"].(map[string]any); ok {
			signals[protocolAnthropic] = true
		}
	}
}

func (a protocolAdapter) normalizeRequest(raw []byte) ([]byte, bool, *bool, error) {
	if a.protocol != protocolResponses {
		return raw, false, nil, nil
	}
	body, changed, media, err := normalizeResponsesStringInputWithMedia(raw, a.protocol)
	if err != nil {
		return nil, false, nil, err
	}
	return body, changed, &media, nil
}

func (a protocolAdapter) conversationItems(root any) []any {
	obj, _ := root.(map[string]any)
	items, _ := obj[a.conversationField].([]any)
	return items
}

func (a protocolAdapter) supportsImageType(typ string) bool {
	return strings.EqualFold(strings.TrimSpace(typ), a.imageBlockType)
}

func (a protocolAdapter) makeTextBlock(description string) map[string]any {
	return map[string]any{"type": a.textBlockType, "text": description}
}

func (a protocolAdapter) decodeImageBlock(item map[string]any, typ string) (string, error) {
	if !a.supportsImageType(typ) {
		return "", fmt.Errorf("%s request contains incompatible image block type %q", a.protocol, typ)
	}
	if a.protocol != protocolAnthropic {
		if raw := imageURL(item); raw != "" {
			return raw, nil
		}
		return "", fmt.Errorf("%s image block has no supported URL", a.protocol)
	}
	if raw := imageURL(item); raw != "" {
		return raw, nil
	}
	source, ok := item["source"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("Claude image block has no source")
	}
	sourceType := strings.ToLower(strings.TrimSpace(stringValue(source["type"])))
	switch sourceType {
	case "base64":
		mediaType := strings.ToLower(strings.TrimSpace(stringValue(source["media_type"])))
		if !strings.HasPrefix(mediaType, "image/") {
			if mediaType == "application/pdf" {
				return "", fmt.Errorf("PDF attachments are not supported by the image bridge")
			}
			return "", fmt.Errorf("unsupported Claude image media type %q", mediaType)
		}
		data := strings.TrimSpace(stringValue(source["data"]))
		if data == "" {
			return "", fmt.Errorf("Claude base64 image source is empty")
		}
		return "data:" + mediaType + ";base64," + data, nil
	case "url":
		raw := strings.TrimSpace(stringValue(source["url"]))
		if raw == "" {
			return "", fmt.Errorf("Claude URL image source is empty")
		}
		return raw, nil
	default:
		return "", fmt.Errorf("unsupported Claude image source type %q", sourceType)
	}
}

func (a protocolAdapter) removeRedundantImageTools(root map[string]any) bool {
	removed := filterNamedToolList(root, "tools", "view_image")
	if a.supportsAdditionalTools {
		if input, ok := root["input"].([]any); ok {
			for _, item := range input {
				obj, _ := item.(map[string]any)
				if strings.EqualFold(strings.TrimSpace(stringValue(obj["type"])), "additional_tools") {
					removed = filterNamedToolList(obj, "tools", "view_image") || removed
				}
			}
		}
	}
	if cleanToolChoice(root, "view_image") {
		removed = true
	}
	return removed
}
