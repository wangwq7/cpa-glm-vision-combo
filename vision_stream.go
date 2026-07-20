package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostCallFunc func(string, any) (json.RawMessage, error)

type visionStreamReadResult struct {
	response pluginapi.HostModelStreamReadResponse
	err      error
}

type visionStreamTimeoutError struct {
	model   string
	timeout time.Duration
}

func (e *visionStreamTimeoutError) Error() string {
	return fmt.Sprintf("visual model %s exceeded the %s cancellable stream budget", e.model, e.timeout.Round(time.Millisecond))
}

type visionCancellationUnconfirmedError struct {
	cause error
}

func (e *visionCancellationUnconfirmedError) Error() string {
	if e == nil || e.cause == nil {
		return "visual stream cancellation could not be confirmed"
	}
	return "visual stream cancellation could not be confirmed: " + e.cause.Error()
}

func (e *visionCancellationUnconfirmedError) Unwrap() error { return e.cause }

func isVisionStreamTimeout(err error) bool {
	var timeout *visionStreamTimeoutError
	return errors.As(err, &timeout)
}

func isVisionCancellationUnconfirmed(err error) bool {
	var unconfirmed *visionCancellationUnconfirmedError
	return errors.As(err, &unconfirmed)
}

func hostExecuteVisionStreamWithTimeout(callbackID, model string, body []byte, seconds, graceSeconds int) (string, error) {
	timeout := time.Duration(seconds) * time.Second
	grace := time.Duration(graceSeconds) * time.Second
	return executeVisionStreamWithTimeout(callbackID, model, body, timeout, grace, callHost)
}

// executeVisionStreamWithTimeout only starts its budget after the host has
// returned a stream ID. The existing Host ABI exposes stream_close only at
// that point; starting a second visual candidate earlier would recreate the
// orphaned-request and duplicate-billing problem this path prevents.
func executeVisionStreamWithTimeout(callbackID, model string, body []byte, timeout, grace time.Duration, invoke hostCallFunc) (string, error) {
	if invoke == nil {
		return "", fmt.Errorf("host callback is unavailable")
	}
	startedRaw, err := invoke(pluginabi.MethodHostModelExecuteStream, hostModelRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         model,
			Stream:        true,
			Body:          body,
		},
		HostCallbackID: callbackID,
	})
	if err != nil {
		return "", err
	}
	var started pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(startedRaw, &started); err != nil {
		return "", fmt.Errorf("decode visual stream start: %w", err)
	}
	if started.StatusCode >= 400 {
		return "", fmt.Errorf("visual model %s returned HTTP %d while starting its stream", model, started.StatusCode)
	}
	if strings.TrimSpace(started.StreamID) == "" {
		return "", fmt.Errorf("visual model %s returned no stream id", model)
	}

	streamID := started.StreamID
	streamClosed := false
	defer func() {
		if !streamClosed {
			_, _ = invoke(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
		}
	}()

	var timer *time.Timer
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutC = timer.C
		defer timer.Stop()
	}

	accumulator := &visionStreamAccumulator{}
	for {
		readResult := readVisionHostStream(invoke, streamID)
		if timeoutC == nil {
			result := <-readResult
			if text, done, err := accumulator.consume(result); err != nil {
				return "", err
			} else if done {
				return text, nil
			}
			continue
		}

		select {
		case result := <-readResult:
			if text, done, err := accumulator.consume(result); err != nil {
				return "", err
			} else if done {
				return text, nil
			}
		case <-timeoutC:
			// Prefer a completed read that raced the timer. It may contain the
			// final event, in which case no cancellation or fallback is needed.
			pendingRead := readResult
			select {
			case result := <-readResult:
				if text, done, err := accumulator.consume(result); err != nil {
					return "", err
				} else if done {
					return text, nil
				}
				// The raced read was only a queued non-terminal payload. Start
				// another read so the close path can wait for a terminal ack.
				pendingRead = readVisionHostStream(invoke, streamID)
			default:
			}

			if err := closeAndConfirmVisionStream(invoke, streamID, pendingRead, grace); err != nil {
				return "", &visionCancellationUnconfirmedError{cause: err}
			}
			streamClosed = true
			return "", &visionStreamTimeoutError{model: model, timeout: timeout}
		}
	}
}

func readVisionHostStream(invoke hostCallFunc, streamID string) <-chan visionStreamReadResult {
	result := make(chan visionStreamReadResult, 1)
	go func() {
		raw, err := invoke(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
		if err != nil {
			result <- visionStreamReadResult{err: err}
			return
		}
		var response pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			result <- visionStreamReadResult{err: fmt.Errorf("decode visual stream chunk: %w", err)}
			return
		}
		result <- visionStreamReadResult{response: response}
	}()
	return result
}

func closeAndConfirmVisionStream(invoke hostCallFunc, streamID string, pending <-chan visionStreamReadResult, grace time.Duration) error {
	if _, err := invoke(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID}); err != nil {
		return fmt.Errorf("stream_close failed: %w", err)
	}
	if grace <= 0 {
		grace = 15 * time.Second
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	readResult := pending
	for {
		select {
		case result := <-readResult:
			if result.err != nil {
				return fmt.Errorf("stream read did not return a terminal acknowledgement: %w", result.err)
			}
			if result.response.Done {
				return nil
			}
			// A queued payload can arrive after close. Read again until the
			// host confirms the stream has reached its terminal state.
			readResult = readVisionHostStream(invoke, streamID)
		case <-deadline.C:
			return fmt.Errorf("stream did not acknowledge cancellation within %s", grace.Round(time.Millisecond))
		}
	}
}

func (a *visionStreamAccumulator) consume(result visionStreamReadResult) (string, bool, error) {
	if result.err != nil {
		return "", false, result.err
	}
	if result.response.Error != "" {
		return "", false, fmt.Errorf("visual stream error: %s", result.response.Error)
	}
	a.add(result.response.Payload)
	if !result.response.Done {
		return "", false, nil
	}
	if a.truncationReason != "" {
		return "", false, fmt.Errorf("visual stream returned truncated output (%s)", a.truncationReason)
	}
	text := a.text()
	if text == "" {
		return "", false, fmt.Errorf("visual stream returned no usable text")
	}
	return text, true, nil
}

type visionStreamAccumulator struct {
	pending          string
	delta            strings.Builder
	fallback         strings.Builder
	truncationReason string
}

func (a *visionStreamAccumulator) add(payload []byte) {
	a.pending += string(payload)
	for {
		index := strings.IndexByte(a.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSuffix(a.pending[:index], "\r")
		a.pending = a.pending[index+1:]
		a.consumeLine(line)
	}
	// CPA commonly exposes one complete SSE data frame per Host stream read
	// without preserving the trailing newline. Consume a complete frame at the
	// read boundary while retaining genuinely split JSON for the next read.
	a.consumeCompletePendingFrame()
}

func (a *visionStreamAccumulator) text() string {
	if strings.TrimSpace(a.pending) != "" {
		a.consumeCompletePendingFrame()
	}
	if text := strings.TrimSpace(a.delta.String()); text != "" {
		return text
	}
	return strings.TrimSpace(a.fallback.String())
}

func (a *visionStreamAccumulator) consumeCompletePendingFrame() {
	line := strings.TrimSpace(strings.TrimSuffix(a.pending, "\r"))
	if line == "" {
		a.pending = ""
		return
	}
	if strings.HasPrefix(line, "event:") && !strings.Contains(line, "{") {
		a.pending = ""
		return
	}
	candidate := line
	if strings.HasPrefix(candidate, "data:") {
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "data:"))
	}
	if candidate == "[DONE]" || json.Valid([]byte(candidate)) {
		a.pending = ""
		a.consumeLine(line)
	}
}

func (a *visionStreamAccumulator) consumeLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || line == "data: [DONE]" || line == "[DONE]" {
		return
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if !strings.HasPrefix(line, "{") {
		return
	}
	var root map[string]any
	if json.Unmarshal([]byte(line), &root) != nil {
		return
	}
	if a.truncationReason == "" {
		a.truncationReason = truncationReasonFromValue(root)
	}
	handledFallback := false
	eventType := strings.ToLower(strings.TrimSpace(stringValue(root["type"])))
	if delta, ok := root["delta"].(string); ok && delta != "" && (eventType == "" || eventType == "response.output_text.delta") {
		a.delta.WriteString(delta)
	}
	if eventType == "content_block_delta" {
		if delta, ok := root["delta"].(map[string]any); ok {
			if text := stringValue(delta["text"]); text != "" {
				a.delta.WriteString(text)
			}
		}
	}
	if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if text := contentText(delta["content"]); text != "" {
					a.delta.WriteString(text)
				}
			}
			if message, ok := choice["message"].(map[string]any); ok {
				if text := contentText(message["content"]); text != "" {
					a.fallback.WriteString(text)
					handledFallback = true
				}
			}
			if text := contentText(choice["text"]); text != "" {
				a.fallback.WriteString(text)
				handledFallback = true
			}
		}
	}
	if response, ok := root["response"].(map[string]any); ok {
		if text := geminiStreamText(response); text != "" {
			a.delta.WriteString(text)
		}
	}
	if eventType == "response.output_text.done" {
		if text := stringValue(root["text"]); text != "" {
			a.writeFallback(text)
			handledFallback = true
		}
	}
	if part, ok := root["part"].(map[string]any); ok {
		if text := stringValue(part["text"]); text != "" {
			a.writeFallback(text)
			handledFallback = true
		}
	}
	if !handledFallback {
		if text := extractVisionText([]byte(line)); text != "" {
			a.writeFallback(text)
		}
	}
}

func (a *visionStreamAccumulator) writeFallback(text string) {
	if a.fallback.Len() == 0 {
		a.fallback.WriteString(text)
	}
}

func geminiStreamText(response map[string]any) string {
	candidates, _ := response["candidates"].([]any)
	if len(candidates) == 0 {
		return ""
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var text strings.Builder
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if thought, _ := part["thought"].(bool); thought {
			continue
		}
		text.WriteString(stringValue(part["text"]))
	}
	return text.String()
}

func responseTruncationReason(raw []byte) string {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	return truncationReasonFromValue(root)
}

func truncationReasonFromValue(value any) string {
	switch current := value.(type) {
	case map[string]any:
		if reason := normalizedStopReason(current["finish_reason"]); reason == "length" {
			return "finish_reason=length"
		}
		if reason := normalizedStopReason(current["finishReason"]); reason == "max_tokens" || reason == "max_output_tokens" {
			return "finishReason=" + stringValue(current["finishReason"])
		}
		if reason := normalizedStopReason(current["stop_reason"]); reason == "max_tokens" || reason == "max_output_tokens" {
			return "stop_reason=" + stringValue(current["stop_reason"])
		}
		eventType := strings.ToLower(strings.TrimSpace(stringValue(current["type"])))
		status := strings.ToLower(strings.TrimSpace(stringValue(current["status"])))
		_, hasIncompleteDetails := current["incomplete_details"].(map[string]any)
		if eventType == "response.incomplete" || status == "incomplete" && hasIncompleteDetails {
			reason := incompleteReason(current)
			if reason == "" {
				reason = "unknown"
			}
			return "response.incomplete=" + reason
		}
		for _, key := range []string{"choices", "candidates", "response", "delta"} {
			nested, exists := current[key]
			if !exists {
				continue
			}
			if reason := truncationReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	case []any:
		for _, nested := range current {
			if reason := truncationReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func normalizedStopReason(value any) string {
	reason := strings.ToLower(strings.TrimSpace(stringValue(value)))
	reason = strings.ReplaceAll(reason, "-", "_")
	reason = strings.ReplaceAll(reason, " ", "_")
	return reason
}

func incompleteReason(root map[string]any) string {
	if details, ok := root["incomplete_details"].(map[string]any); ok {
		return normalizedStopReason(details["reason"])
	}
	if response, ok := root["response"].(map[string]any); ok {
		if details, ok := response["incomplete_details"].(map[string]any); ok {
			return normalizedStopReason(details["reason"])
		}
	}
	return ""
}
