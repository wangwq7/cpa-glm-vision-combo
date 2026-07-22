package main

import "sort"

// historyToolRefs records the structural edges that must never cross a
// compression boundary. The wire formats differ, but every supported protocol
// has a call identifier that can be paired with its result identifier.
type historyToolRefs struct {
	calls   map[string][]int
	results map[string][]int
}

func newHistoryToolRefs() historyToolRefs {
	return historyToolRefs{
		calls:   map[string][]int{},
		results: map[string][]int{},
	}
}

func collectHistoryToolRefs(items []any) historyToolRefs {
	refs := newHistoryToolRefs()
	for index, item := range items {
		collectHistoryToolRefsValue(item, index, &refs)
	}
	return refs
}

func collectHistoryToolRefsValue(value any, index int, refs *historyToolRefs) {
	switch current := value.(type) {
	case []any:
		for _, child := range current {
			collectHistoryToolRefsValue(child, index, refs)
		}
	case map[string]any:
		typ := stringValue(current["type"])
		switch typ {
		case "function_call", "tool_use":
			if id := historyToolCallID(current); id != "" {
				refs.calls[id] = append(refs.calls[id], index)
			}
		case "function_call_output", "tool_result":
			if id := historyToolResultID(current); id != "" {
				refs.results[id] = append(refs.results[id], index)
			}
		}

		// OpenAI Chat represents calls and results through role-specific
		// structures rather than the Responses/Claude block types.
		if stringValue(current["role"]) == "assistant" {
			if toolCalls, ok := current["tool_calls"].([]any); ok {
				for _, rawTool := range toolCalls {
					tool, _ := rawTool.(map[string]any)
					if id := stringValue(tool["id"]); id != "" {
						refs.calls[id] = append(refs.calls[id], index)
					}
				}
			}
		}
		if stringValue(current["role"]) == "tool" {
			if id := stringValue(current["tool_call_id"]); id != "" {
				refs.results[id] = append(refs.results[id], index)
			}
		}

		for _, child := range current {
			collectHistoryToolRefsValue(child, index, refs)
		}
	}
}

func historyToolCallID(value map[string]any) string {
	if id := stringValue(value["call_id"]); id != "" {
		return id
	}
	return stringValue(value["id"])
}

func historyToolResultID(value map[string]any) string {
	if id := stringValue(value["call_id"]); id != "" {
		return id
	}
	return stringValue(value["tool_use_id"])
}

// historyBoundarySafe reports whether replacing items[:boundary] with a text
// summary can leave any structured tool result without its call, or move an
// unfinished call into the summary. Unfinished calls are allowed only in the
// retained suffix, where the downstream client can still complete them.
func historyBoundarySafe(items []any, boundary int) bool {
	if boundary < 0 || boundary > len(items) {
		return false
	}
	refs := collectHistoryToolRefs(items)
	for id, callIndexes := range refs.calls {
		resultIndexes := refs.results[id]
		for _, callIndex := range callIndexes {
			if callIndex < boundary {
				if len(resultIndexes) == 0 {
					return false
				}
				for _, resultIndex := range resultIndexes {
					if resultIndex >= boundary {
						return false
					}
				}
				continue
			}
			for _, resultIndex := range resultIndexes {
				if resultIndex < boundary {
					return false
				}
			}
		}
	}
	for id, resultIndexes := range refs.results {
		if _, hasCall := refs.calls[id]; hasCall {
			continue
		}
		for _, resultIndex := range resultIndexes {
			if resultIndex >= boundary {
				return false
			}
		}
	}
	return true
}

func historyItemContainsToolResult(value any) bool {
	found := false
	var visit func(any)
	visit = func(current any) {
		if found {
			return
		}
		switch item := current.(type) {
		case []any:
			for _, child := range item {
				visit(child)
			}
		case map[string]any:
			if item["type"] == "function_call_output" || item["type"] == "tool_result" || item["role"] == "tool" {
				found = true
				return
			}
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	return found
}

func historyItemStartsUserTurn(value any) bool {
	item, _ := value.(map[string]any)
	return stringValue(item["role"]) == "user" && !historyItemContainsToolResult(value)
}

// historyCompressionBoundaries returns semantic, structurally safe cut points.
// It includes real user-turn starts and the end of each completed tool
// transaction. The latter is essential for a long single user turn containing
// hundreds of tool rounds.
func historyCompressionBoundaries(items []any) []int {
	candidates := map[int]struct{}{0: {}}
	for index := 1; index <= len(items); index++ {
		if index == len(items) || historyItemStartsUserTurn(items[index]) || historyItemContainsToolResult(items[index-1]) {
			candidates[index] = struct{}{}
		}
	}
	boundaries := make([]int, 0, len(candidates))
	for index := range candidates {
		if historyBoundarySafe(items, index) {
			boundaries = append(boundaries, index)
		}
	}
	sort.Ints(boundaries)
	return boundaries
}

func containsHistoryBoundary(boundaries []int, wanted int) bool {
	for _, boundary := range boundaries {
		if boundary == wanted {
			return true
		}
	}
	return false
}
