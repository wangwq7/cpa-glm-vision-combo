package main

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func streamJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestVisionStreamAccumulatorHandlesSplitSSEChunks(t *testing.T) {
	accumulator := &visionStreamAccumulator{}
	accumulator.add([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OCR: "))
	accumulator.add([]byte("screen\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"shot\"}}]}\n\ndata: [DONE]\n\n"))
	if got := accumulator.text(); got != "OCR: screenshot" {
		t.Fatalf("text = %q", got)
	}
}

func TestVisionStreamAccumulatorHandlesFramesWithoutTrailingNewlines(t *testing.T) {
	accumulator := &visionStreamAccumulator{}
	accumulator.add([]byte(`data: {"choices":[{"delta":{"content":"OCR: "}}]}`))
	accumulator.add([]byte(`data: {"choices":[{"delta":{"content":"screen"}}]}`))
	accumulator.add([]byte(`data: {"choices":[{"delta":{"content":"shot"}}]}`))
	if got := accumulator.text(); got != "OCR: screenshot" {
		t.Fatalf("text = %q", got)
	}
}

func TestVisionStreamAccumulatorHandlesProviderStreamDialects(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name: "openai responses",
			chunks: []string{
				`data: {"type":"response.reasoning_summary_text.delta","delta":"ignore me"}`,
				`data: {"type":"response.output_text.delta","delta":"visible "}`,
				`data: {"type":"response.output_text.delta","delta":"text"}`,
			},
			want: "visible text",
		},
		{
			name: "anthropic",
			chunks: []string{
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"claude "}}`,
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"text"}}`,
			},
			want: "claude text",
		},
		{
			name: "gemini antigravity",
			chunks: []string{
				`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"ignore"},{"text":"gemini "}]}}]}}`,
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":"text"}]}}]}}`,
			},
			want: "gemini text",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accumulator := &visionStreamAccumulator{}
			for _, chunk := range test.chunks {
				accumulator.add([]byte(chunk))
			}
			if got := accumulator.text(); got != test.want {
				t.Fatalf("text = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExecuteVisionStreamReturnsTextFromHostStream(t *testing.T) {
	var mu sync.Mutex
	read := 0
	closeCalls := 0
	invoke := func(method string, _ any) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "vision-1"}), nil
		case pluginabi.MethodHostModelStreamRead:
			read++
			if read == 1 {
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"OCR \"}}]}\n\n")}), nil
			}
			return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\n\ndata: [DONE]\n\n"), Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			closeCalls++
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected host method")
		}
	}

	text, err := executeVisionStreamWithTimeout("callback", "vision(low)", []byte(`{"stream":true}`), time.Second, time.Second, invoke)
	if err != nil {
		t.Fatal(err)
	}
	if text != "OCR complete" {
		t.Fatalf("text = %q", text)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestExecuteVisionStreamClosesAndConfirmsBeforeTimeoutFallback(t *testing.T) {
	release := make(chan struct{})
	readStarted := make(chan struct{})
	var once sync.Once
	closeCalls := 0
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "vision-timeout"}), nil
		case pluginabi.MethodHostModelStreamRead:
			once.Do(func() { close(readStarted) })
			<-release
			return streamJSON(pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			closeCalls++
			once.Do(func() {})
			select {
			case <-release:
			default:
				close(release)
			}
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected host method")
		}
	}

	text, err := executeVisionStreamWithTimeout("callback", "vision(low)", []byte(`{"stream":true}`), 15*time.Millisecond, 200*time.Millisecond, invoke)
	if text != "" {
		t.Fatalf("text = %q, want empty on timeout", text)
	}
	if !isVisionStreamTimeout(err) {
		t.Fatalf("error = %v, want cancellable timeout", err)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	select {
	case <-readStarted:
	default:
		t.Fatal("stream read never started")
	}
}

func TestCloseAndConfirmVisionStreamDrainsQueuedPayload(t *testing.T) {
	pending := make(chan visionStreamReadResult, 1)
	pending <- visionStreamReadResult{response: pluginapi.HostModelStreamReadResponse{Payload: []byte("queued")}}
	reads := 0
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamRead:
			reads++
			return streamJSON(pluginapi.HostModelStreamReadResponse{Done: true}), nil
		default:
			return nil, errors.New("unexpected host method")
		}
	}

	if err := closeAndConfirmVisionStream(invoke, "vision-queued", pending, time.Second); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("follow-up reads = %d, want 1", reads)
	}
}

func TestExecuteVisionStreamStopsFallbackWhenCancellationIsUnconfirmed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "vision-unconfirmed"}), nil
		case pluginabi.MethodHostModelStreamRead:
			<-release
			return streamJSON(pluginapi.HostModelStreamReadResponse{Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			return nil, errors.New("close callback failed")
		default:
			return nil, errors.New("unexpected host method")
		}
	}

	_, err := executeVisionStreamWithTimeout("callback", "vision(low)", []byte(`{"stream":true}`), 10*time.Millisecond, 10*time.Millisecond, invoke)
	if !isVisionCancellationUnconfirmed(err) {
		t.Fatalf("error = %v, want unconfirmed cancellation", err)
	}
}
