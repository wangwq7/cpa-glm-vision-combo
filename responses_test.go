package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeResponsesStringInputPreservesSurroundingJSON(t *testing.T) {
	raw := []byte("{\n  \"instructions\" : \"keep <>&\", \n  \"in\\u0070ut\" : \"中文 \\\\ path \\\"quoted\\\" \\u003c\", \n  \"metadata\" : {\"input\":\"nested\",\"type\":\"input_image\"},\n  \"stream\" : true\n}")
	got, changed, err := normalizeResponsesStringInput(raw, "openai-response")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v body=%s", changed, err, got)
	}

	const prefix = "{\n  \"instructions\" : \"keep <>&\", \n  \"in\\u0070ut\" : "
	const suffix = ", \n  \"metadata\" : {\"input\":\"nested\",\"type\":\"input_image\"},\n  \"stream\" : true\n}"
	if !bytes.HasPrefix(got, []byte(prefix)) || !bytes.HasSuffix(got, []byte(suffix)) {
		t.Fatalf("surrounding JSON changed:\n%s", got)
	}

	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]any)
	message := input[0].(map[string]any)
	content := message["content"].([]any)
	part := content[0].(map[string]any)
	if part["text"] != "中文 \\ path \"quoted\" <" {
		t.Fatalf("text=%q", part["text"])
	}
	if root["instructions"] != "keep <>&" || root["stream"] != true {
		t.Fatalf("top-level fields changed: %#v", root)
	}
}

func TestNormalizeResponsesStringInputNonStringValuesRemainUnchanged(t *testing.T) {
	tests := []string{
		`{"input":[]}`,
		`{"input":{}}`,
		`{"input":null}`,
		`{"input":42}`,
		`{"input":true}`,
		`{"metadata":{"input":"nested"}}`,
		`null`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			got, changed, err := normalizeResponsesStringInput([]byte(raw), "openai-response")
			if err != nil || changed || string(got) != raw {
				t.Fatalf("changed=%v err=%v body=%s", changed, err, got)
			}
		})
	}
}

func TestNormalizeResponsesStringInputRejectsInvalidOrNonObjectJSON(t *testing.T) {
	tests := []string{
		``,
		`{"input":"before"`,
		`{"input":"before"} trailing`,
		`{"input":"bad\q"}`,
		`[]`,
		`"text"`,
		`true`,
		`42`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := normalizeResponsesStringInput([]byte(raw), "openai-response"); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestNormalizeResponsesStringInputDuplicateFieldsKeepLegacyBehavior(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantChanged bool
		wantText    string
	}{
		{
			name:        "last string wins",
			raw:         `{"input":[{"role":"user"}],"input":"last"}`,
			wantChanged: true,
			wantText:    "last",
		},
		{
			name:        "last array remains unchanged",
			raw:         `{"input":"first","input":[{"role":"user"}]}`,
			wantChanged: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed, err := normalizeResponsesStringInput([]byte(test.raw), "openai-response")
			if err != nil || changed != test.wantChanged {
				t.Fatalf("changed=%v err=%v body=%s", changed, err, got)
			}
			legacy, legacyChanged, legacyErr := normalizeResponsesStringInputLegacy([]byte(test.raw))
			if legacyErr != nil || legacyChanged != changed || !bytes.Equal(got, legacy) {
				t.Fatalf("optimized result differs from legacy:\noptimized=%s\nlegacy=%s", got, legacy)
			}
			if test.wantText != "" && !strings.Contains(string(got), test.wantText) {
				t.Fatalf("missing final input text in %s", got)
			}
		})
	}
}

func TestNormalizeResponsesStringInputMatchesLegacyTextSemantics(t *testing.T) {
	tests := []string{
		`{"input":""}`,
		`{"input":"plain ASCII"}`,
		`{"input":"中文与 emoji \ud83d\ude80"}`,
		`{"input":"slash \/ quote \" backslash \\ tab \t"}`,
		`{"input":"isolated surrogate \ud800"}`,
		`{"instructions":"keep","input":"<>&\u2028\u2029","metadata":{"trace":"same"}}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			got, changed, err := normalizeResponsesStringInput([]byte(raw), "openai-response")
			legacy, legacyChanged, legacyErr := normalizeResponsesStringInputLegacy([]byte(raw))
			if err != nil || legacyErr != nil || !changed || !legacyChanged {
				t.Fatalf("changed=%v/%v err=%v/%v", changed, legacyChanged, err, legacyErr)
			}
			var gotValue any
			var legacyValue any
			if err := json.Unmarshal(got, &gotValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(legacy, &legacyValue); err != nil {
				t.Fatal(err)
			}
			if !deepEqualJSON(gotValue, legacyValue) {
				t.Fatalf("semantic mismatch:\noptimized=%s\nlegacy=%s", got, legacy)
			}
		})
	}
}

func TestNormalizeResponsesStringInputInvalidUTF8UsesLegacySanitization(t *testing.T) {
	raw := []byte(`{"instructions":"before `)
	raw = append(raw, 0xff)
	raw = append(raw, []byte(`","input":"question"}`)...)
	got, changed, err := normalizeResponsesStringInput(raw, "openai-response")
	legacy, legacyChanged, legacyErr := normalizeResponsesStringInputLegacy(raw)
	if err != nil || legacyErr != nil || changed != legacyChanged || !bytes.Equal(got, legacy) {
		t.Fatalf("optimized result differs from legacy:\noptimized=%q\nlegacy=%q\nerr=%v/%v", got, legacy, err, legacyErr)
	}
}

func TestPreparePrimaryBodyNormalizesResponsesStringInputOnce(t *testing.T) {
	cfg := testRuntime()
	defer cfg.cache.close()
	event := cfg.events.begin(cfg.ComboModel, cfg.PrimaryModel, false)
	raw := []byte(`{"model":"glm-5.2-vision-combo","instructions":"keep","input":"question","metadata":{"trace":"same"},"stream":false}`)

	got, images, err := preparePrimaryBody(raw, "openai-response", cfg, "", event)
	if err != nil || images != 0 {
		t.Fatalf("images=%d err=%v body=%s", images, err, got)
	}
	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["input"].([]any); !ok || root["instructions"] != "keep" {
		t.Fatalf("request was not normalized correctly: %#v", root)
	}

	normalizedStages := 0
	for _, stored := range cfg.events.snapshot() {
		if stored.ID != event.ID {
			continue
		}
		for _, stage := range stored.Stages {
			if stage.Name == "规范化 Responses 输入" {
				normalizedStages++
			}
		}
	}
	if normalizedStages != 1 {
		t.Fatalf("normalization stages=%d", normalizedStages)
	}

	final, err := prepareTextHostBody(got, "openai-response", cfg, "", event)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(final, got) {
		t.Fatalf("text preparation normalized a second time:\nfirst=%s\nfinal=%s", got, final)
	}
}

func TestNormalizeResponsesStringInputOtherProtocolsRemainByteIdentical(t *testing.T) {
	raw := []byte(`{"input":"leave unchanged","messages":[{"role":"user","content":"hello"}]}`)
	for _, protocol := range []string{"openai", "claude", " OPENAI "} {
		got, changed, err := normalizeResponsesStringInput(raw, protocol)
		if err != nil || changed || !bytes.Equal(got, raw) {
			t.Fatalf("protocol=%q changed=%v err=%v body=%s", protocol, changed, err, got)
		}
	}
}

func FuzzNormalizeResponsesStringInputMatchesLegacy(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"input":"hello"}`),
		[]byte(`{"instructions":"rules","input":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"input":"first","input":"last"}`),
		[]byte(`{"in\u0070ut":"escaped key \ud800","stream":true}`),
		[]byte(`{"metadata":{"input":"nested","type":"input_image"},"input":"question"}`),
		[]byte(`null`),
		[]byte(`{"input":"truncated"`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		got, changed, err := normalizeResponsesStringInput(raw, "openai-response")
		legacy, legacyChanged, legacyErr := normalizeResponsesStringInputLegacy(raw)
		if (err != nil) != (legacyErr != nil) {
			t.Fatalf("error mismatch: optimized=%v legacy=%v raw=%q", err, legacyErr, raw)
		}
		if err != nil {
			return
		}
		if changed != legacyChanged {
			t.Fatalf("changed mismatch: optimized=%v legacy=%v raw=%q", changed, legacyChanged, raw)
		}
		if !changed {
			if !bytes.Equal(got, legacy) {
				t.Fatalf("unchanged bytes mismatch:\noptimized=%q\nlegacy=%q", got, legacy)
			}
			return
		}
		var gotValue any
		var legacyValue any
		if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal(legacy, &legacyValue) != nil || !deepEqualJSON(gotValue, legacyValue) {
			t.Fatalf("changed semantic mismatch:\noptimized=%q\nlegacy=%q", got, legacy)
		}
	})
}

func deepEqualJSON(left, right any) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
