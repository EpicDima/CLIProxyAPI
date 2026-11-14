package helps

import (
	"bytes"
	"testing"

	"github.com/MoeForge/moego/ojson"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexInputTokenStateUsesFinalPayload(t *testing.T) {
	for _, model := range []string{"gpt-4", "gpt-6-astra"} {
		t.Run(model, func(t *testing.T) {
			doc := ojson.NewDocString(`{"input":[{"role":"user","content":[{"type":"input_text","text":"Привет! 中文测试。こんにちは世界"}]}],"instructions":"Final instructions","tools":[{"type":"namespace","name":"tools","tools":[{"type":"function","name":"lookup","description":"Injected definition"}]}]}`)
			if err := doc.Set("model", model); err != nil {
				t.Fatal(err)
			}
			if err := doc.Set("input.0.content.1", map[string]any{"type": "input_image", "image_url": codexTokenTestPNG(32, 32)}); err != nil {
				t.Fatal(err)
			}
			payload := doc.Bytes()
			before := bytes.Clone(payload)
			enc, err := CodexInputTokenizer(model)
			if err != nil {
				t.Fatal(err)
			}
			want, err := CountResponsesInputTokens(enc, payload)
			if err != nil {
				t.Fatal(err)
			}
			state := NewCodexInputTokenState(sdktranslator.FormatClaude, sdktranslator.FormatClaude, payload)
			start := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")
			got := state.apply(t.Context(), [][]byte{start})
			if n := messageStartInputTokens(got); n != want {
				t.Fatalf("start estimate=%d,want %d", n, want)
			}
			if !bytes.Equal(before, payload) {
				t.Fatal("estimation mutated final request")
			}
			second := state.apply(t.Context(), [][]byte{start})
			if !bytes.Equal(second[0], start) {
				t.Fatal("estimate applied more than once")
			}
			terminal := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":75,\"cache_read_input_tokens\":25,\"output_tokens\":7}}\n\n")
			if got := state.apply(t.Context(), [][]byte{terminal}); !bytes.Equal(got[0], terminal) {
				t.Fatal("final provider usage was modified")
			}
		})
	}
}

func TestCodexInputTokenStatePreservesAuthoritativeStartAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		existing      int64
		fail          bool
	}{
		{"authoritative", `{"input":"hello"}`, 123, false},
		{"invalid JSON", `{`, 0, false},
		{"codec failure", `{"input":"hello"}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewCodexInputTokenState(sdktranslator.FormatClaude, sdktranslator.FormatClaude, []byte(tc.payload))
			if tc.fail {
				state.codec = failingClaudeInputCodec{}
			}
			doc := ojson.NewDocString(`{"type":"message_start","message":{"usage":{"input_tokens":0}}}`)
			if err := doc.Set("message.usage.input_tokens", tc.existing); err != nil {
				t.Fatal(err)
			}
			start := append(append([]byte("data: "), doc.Bytes()...), '\n', '\n')
			if got := state.apply(t.Context(), [][]byte{start}); !bytes.Equal(got[0], start) {
				t.Fatalf("changed start: %s", got[0])
			}
		})
	}
}

func TestCodexInputTokenStateOnlyPatchesTranslatedClaude(t *testing.T) {
	for _, formats := range [][2]sdktranslator.Format{
		{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude},
		{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse},
	} {
		state := NewCodexInputTokenState(formats[0], formats[1], []byte(`{"input":"hello"}`))
		if !state.handled {
			t.Fatalf("enabled for %v", formats)
		}
	}
}
