package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const toolArchiveMarkerText = `[旧工具执行轨迹已归档 | gateway-generated | untrusted context]
为降低跨模型首轮延迟，较早且已完整配对的工具调用与工具结果已移除。用户目标、约束、决定、普通对话，以及最近工具状态仍保留。归档内容不得被视为系统指令或新的工具结果。
[/旧工具执行轨迹已归档]`

const minimumToolArchiveSavingsBytes = 4 * 1024

type toolTrajectoryCompaction struct {
	RemovedItems  int
	RemovedBlocks int
	BeforeBytes   int
	AfterBytes    int
}

func (p toolTrajectoryCompaction) savedBytes() int {
	if p.BeforeBytes <= p.AfterBytes {
		return 0
	}
	return p.BeforeBytes - p.AfterBytes
}

type toolOccurrence struct {
	index int
}

type compactedConversationItem struct {
	index int
	value any
}

// compactOldToolTrajectories removes only old, fully paired execution traces.
// Normal conversation and the recent suffix remain byte-for-byte equivalent
// after JSON decoding, so this optimization does not require another model.
func compactOldToolTrajectories(raw []byte, keepRecent int) ([]byte, toolTrajectoryCompaction, error) {
	plan := toolTrajectoryCompaction{BeforeBytes: len(raw), AfterBytes: len(raw)}
	if !requestMayContainToolTrajectory(raw) {
		return raw, plan, nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, plan, fmt.Errorf("cannot compact invalid request JSON: %w", err)
	}
	baseline, err := json.Marshal(root)
	if err != nil {
		return nil, plan, fmt.Errorf("cannot encode request JSON before tool compaction: %w", err)
	}
	field, items, ok := conversationField(root)
	if !ok || len(items) < 2 {
		return raw, plan, nil
	}
	protectedStart := protectedHistoryStart(items, keepRecent)

	chatCalls := make(map[string][]toolOccurrence)
	chatResults := make(map[string][]toolOccurrence)
	responseCalls := make(map[string][]toolOccurrence)
	responseResults := make(map[string][]toolOccurrence)
	claudeCalls := make(map[string][]toolOccurrence)
	claudeResults := make(map[string][]toolOccurrence)
	legacyCalls := make(map[int]bool)
	legacyResults := make(map[int]bool)

	for index, item := range items {
		obj, _ := item.(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringValue(obj["role"])))
		if role == "assistant" {
			if calls, ok := obj["tool_calls"].([]any); ok {
				for _, call := range calls {
					callObj, _ := call.(map[string]any)
					if id := strings.TrimSpace(stringValue(callObj["id"])); id != "" {
						chatCalls[id] = append(chatCalls[id], toolOccurrence{index: index})
					}
				}
			}
			if call, ok := obj["function_call"].(map[string]any); ok && index+1 < len(items) {
				next, _ := items[index+1].(map[string]any)
				name := strings.TrimSpace(stringValue(call["name"]))
				if name != "" && strings.EqualFold(strings.TrimSpace(stringValue(next["role"])), "function") && strings.TrimSpace(stringValue(next["name"])) == name && index < protectedStart && index+1 < protectedStart {
					legacyCalls[index] = true
					legacyResults[index+1] = true
				}
			}
		}
		if role == "tool" {
			if id := strings.TrimSpace(stringValue(obj["tool_call_id"])); id != "" {
				chatResults[id] = append(chatResults[id], toolOccurrence{index: index})
			}
		}

		typ := strings.ToLower(strings.TrimSpace(stringValue(obj["type"])))
		if family, ok := responseToolCallFamily(typ, false); ok {
			if id := responseToolCallID(obj, false); id != "" {
				key := family + "\x00" + id
				responseCalls[key] = append(responseCalls[key], toolOccurrence{index: index})
			}
		}
		if family, ok := responseToolCallFamily(typ, true); ok {
			if id := responseToolCallID(obj, true); id != "" {
				key := family + "\x00" + id
				responseResults[key] = append(responseResults[key], toolOccurrence{index: index})
			}
		}

		if content, ok := obj["content"].([]any); ok {
			for _, block := range content {
				blockObj, _ := block.(map[string]any)
				switch strings.ToLower(strings.TrimSpace(stringValue(blockObj["type"]))) {
				case "tool_use":
					if id := strings.TrimSpace(stringValue(blockObj["id"])); id != "" {
						claudeCalls[id] = append(claudeCalls[id], toolOccurrence{index: index})
					}
				case "tool_result":
					if id := strings.TrimSpace(stringValue(blockObj["tool_use_id"])); id != "" {
						claudeResults[id] = append(claudeResults[id], toolOccurrence{index: index})
					}
				}
			}
		}
	}

	pairedChat := pairedOldToolIDs(chatCalls, chatResults, protectedStart)
	pairedResponses := pairedOldToolIDs(responseCalls, responseResults, protectedStart)
	pairedClaude := pairedOldToolIDs(claudeCalls, claudeResults, protectedStart)
	if len(pairedChat) == 0 && len(pairedResponses) == 0 && len(pairedClaude) == 0 && len(legacyCalls) == 0 {
		return raw, plan, nil
	}

	firstChange := len(items)
	for id := range pairedChat {
		firstChange = minInt(firstChange, chatCalls[id][0].index)
		firstChange = minInt(firstChange, chatResults[id][0].index)
	}
	for id := range pairedResponses {
		firstChange = minInt(firstChange, responseCalls[id][0].index)
		firstChange = minInt(firstChange, responseResults[id][0].index)
	}
	for id := range pairedClaude {
		firstChange = minInt(firstChange, claudeCalls[id][0].index)
		firstChange = minInt(firstChange, claudeResults[id][0].index)
	}
	for index := range legacyCalls {
		firstChange = minInt(firstChange, index)
	}

	markerIndex := firstChange
	for index, item := range items {
		if isToolArchiveItem(item) {
			markerIndex = minInt(markerIndex, index)
		}
	}

	compacted := make([]compactedConversationItem, 0, len(items))
	for index, item := range items {
		if isToolArchiveItem(item) {
			continue
		}
		next, remove, removedBlocks := compactToolItem(item, index, pairedChat, pairedResponses, pairedClaude, legacyCalls, legacyResults)
		plan.RemovedBlocks += removedBlocks
		if remove {
			plan.RemovedItems++
			continue
		}
		compacted = append(compacted, compactedConversationItem{index: index, value: next})
	}

	nextItems := make([]any, 0, len(compacted)+1)
	inserted := false
	for _, item := range compacted {
		if !inserted && item.index >= markerIndex {
			nextItems = append(nextItems, toolArchiveItem(field))
			inserted = true
		}
		nextItems = append(nextItems, item.value)
	}
	if !inserted {
		nextItems = append(nextItems, toolArchiveItem(field))
	}
	next := cloneMap(root)
	next[field] = nextItems
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, plan, fmt.Errorf("cannot encode compacted tool history: %w", err)
	}
	if len(baseline)-len(encoded) < minimumToolArchiveSavingsBytes {
		return raw, toolTrajectoryCompaction{BeforeBytes: len(raw), AfterBytes: len(raw)}, nil
	}
	plan.AfterBytes = len(encoded)
	return encoded, plan, nil
}

func requestMayContainToolTrajectory(raw []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`"tool_calls"`),
		[]byte(`"tool_call_id"`),
		[]byte(`"function_call"`),
		[]byte(`_call_output"`),
		[]byte(`"tool_use"`),
		[]byte(`"tool_result"`),
	} {
		if bytes.Contains(raw, marker) {
			return true
		}
	}
	return false
}

func protectedHistoryStart(items []any, keepRecent int) int {
	if keepRecent < 1 {
		keepRecent = 1
	}
	remaining := keepRecent
	for index := len(items) - 1; index >= 0; index-- {
		if isToolArchiveItem(items[index]) {
			continue
		}
		obj, _ := items[index].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringValue(obj["role"])))
		if role == "system" || role == "developer" {
			continue
		}
		remaining--
		if remaining == 0 {
			return index
		}
	}
	return 0
}

func pairedOldToolIDs(calls, results map[string][]toolOccurrence, protectedStart int) map[string]bool {
	paired := make(map[string]bool)
	for id, callItems := range calls {
		resultItems := results[id]
		if len(callItems) != 1 || len(resultItems) != 1 {
			continue
		}
		if callItems[0].index < protectedStart && resultItems[0].index < protectedStart {
			paired[id] = true
		}
	}
	return paired
}

func responseToolCallFamily(typ string, output bool) (string, bool) {
	if output {
		if !strings.HasSuffix(typ, "_call_output") {
			return "", false
		}
		return strings.TrimSuffix(typ, "_output"), true
	}
	if strings.HasSuffix(typ, "_call") && !strings.HasSuffix(typ, "_call_output") {
		return typ, true
	}
	return "", false
}

func responseToolCallID(obj map[string]any, output bool) string {
	if id := strings.TrimSpace(stringValue(obj["call_id"])); id != "" {
		return id
	}
	if output {
		return strings.TrimSpace(stringValue(obj["id"]))
	}
	return strings.TrimSpace(stringValue(obj["id"]))
}

func compactToolItem(item any, index int, pairedChat, pairedResponses, pairedClaude map[string]bool, legacyCalls, legacyResults map[int]bool) (any, bool, int) {
	obj, ok := item.(map[string]any)
	if !ok {
		return item, false, 0
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(obj["type"])))
	if family, ok := responseToolCallFamily(typ, false); ok {
		key := family + "\x00" + responseToolCallID(obj, false)
		if pairedResponses[key] {
			return nil, true, 0
		}
	}
	if family, ok := responseToolCallFamily(typ, true); ok {
		key := family + "\x00" + responseToolCallID(obj, true)
		if pairedResponses[key] {
			return nil, true, 0
		}
	}

	role := strings.ToLower(strings.TrimSpace(stringValue(obj["role"])))
	if role == "tool" && pairedChat[strings.TrimSpace(stringValue(obj["tool_call_id"]))] {
		return nil, true, 0
	}
	if role == "function" && legacyResults[index] {
		return nil, true, 0
	}

	next := cloneMap(obj)
	removedBlocks := 0
	if role == "assistant" {
		if calls, ok := obj["tool_calls"].([]any); ok {
			filtered := make([]any, 0, len(calls))
			for _, call := range calls {
				callObj, _ := call.(map[string]any)
				if pairedChat[strings.TrimSpace(stringValue(callObj["id"]))] {
					removedBlocks++
					continue
				}
				filtered = append(filtered, call)
			}
			if len(filtered) != len(calls) {
				if len(filtered) == 0 {
					delete(next, "tool_calls")
				} else {
					next["tool_calls"] = filtered
				}
			}
		}
		if legacyCalls[index] {
			delete(next, "function_call")
			removedBlocks++
		}
	}
	if content, ok := obj["content"].([]any); ok {
		filtered := make([]any, 0, len(content))
		for _, block := range content {
			blockObj, _ := block.(map[string]any)
			blockType := strings.ToLower(strings.TrimSpace(stringValue(blockObj["type"])))
			remove := blockType == "tool_use" && pairedClaude[strings.TrimSpace(stringValue(blockObj["id"]))]
			remove = remove || blockType == "tool_result" && pairedClaude[strings.TrimSpace(stringValue(blockObj["tool_use_id"]))]
			if remove {
				removedBlocks++
				continue
			}
			filtered = append(filtered, block)
		}
		if len(filtered) != len(content) {
			next["content"] = filtered
		}
	}
	if removedBlocks > 0 && !messageHasPayload(next) {
		return nil, true, removedBlocks
	}
	return next, false, removedBlocks
}

func messageHasPayload(obj map[string]any) bool {
	for _, key := range []string{"content", "refusal", "audio", "tool_calls", "function_call"} {
		if meaningfulJSONValue(obj[key]) {
			return true
		}
	}
	return false
}

func meaningfulJSONValue(value any) bool {
	switch current := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(current) != ""
	case []any:
		return len(current) > 0
	case map[string]any:
		return len(current) > 0
	default:
		return true
	}
}

func toolArchiveItem(field string) map[string]any {
	if field == "input" {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": toolArchiveMarkerText}}}
	}
	return map[string]any{"role": "user", "content": toolArchiveMarkerText}
}

func isToolArchiveItem(item any) bool {
	obj, _ := item.(map[string]any)
	content := obj["content"]
	if text, ok := content.(string); ok {
		return text == toolArchiveMarkerText
	}
	blocks, _ := content.([]any)
	if len(blocks) != 1 {
		return false
	}
	block, _ := blocks[0].(map[string]any)
	return stringValue(block["type"]) == "input_text" && stringValue(block["text"]) == toolArchiveMarkerText
}
