package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexStreamInputTokensUseFinalPayload(t *testing.T) {
	for _, useWS := range []bool{false, true} {
		for _, buffering := range []bool{false, true} {
			for _, model := range []string{"gpt-4", "gpt-6-astra"} {
				t.Run(fmt.Sprintf("websocket_%t/buffer_%t/%s", useWS, buffering, model), func(t *testing.T) {
					captured := make(chan []byte, 1)
					events := [][]byte{
						[]byte(`{"type":"response.created","response":{"id":"resp_count","model":"` + model + `"}}`),
						[]byte(`{"type":"response.output_text.delta","item_id":"msg_count","output_index":0,"content_index":0,"delta":"ok"}`),
						[]byte(`{"type":"response.completed","response":{"id":"resp_count","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_count","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":25},"output_tokens":7,"total_tokens":107}}}`),
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if useWS {
							upgrader := websocket.Upgrader{}
							conn, err := upgrader.Upgrade(w, r, nil)
							if err != nil {
								t.Errorf("upgrade: %v", err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Errorf("read WS: %v", err)
								return
							}
							captured <- body
							for _, event := range events {
								if err := conn.WriteMessage(websocket.TextMessage, event); err != nil {
									t.Errorf("write WS: %v", err)
									return
								}
							}
							return
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("read HTTP: %v", err)
							return
						}
						captured <- body
						w.Header().Set("Content-Type", "text/event-stream")
						for _, event := range events {
							if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
								t.Errorf("write HTTP: %v", err)
								return
							}
						}
					}))
					defer server.Close()
					cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Payload: config.PayloadConfig{
						Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: map[string]any{
							"model":        model,
							"instructions": strings.Repeat("Final injected instructions. ", 50),
							"tools":        []any{map[string]any{"type": "function", "name": "lookup", "description": strings.Repeat("Injected description. ", 50), "parameters": map[string]any{"type": "object"}}},
							"text.format":  map[string]any{"type": "json_schema", "name": "answer", "schema": map[string]any{"type": "object", "properties": map[string]any{"result": map[string]any{"type": "string"}}}},
						}}},
					}}
					cfg.Codex.StreamBootstrapBuffering = buffering
					original := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"Привет! 中文测试。こんにちは世界"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLttAAAAABJRU5ErkJggg=="}}]}]}`)
					request := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: original}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var result *cliproxyexecutor.StreamResult
					var err error
					if useWS {
						exec := NewCodexWebsocketsExecutor(cfg)
						defer exec.closeAllExecutionSessions("test_cleanup")
						result, err = exec.ExecuteStream(ctx, codexOAuthTestAuth(server.URL), request, opts)
					} else {
						result, err = NewCodexExecutor(cfg).ExecuteStream(ctx, codexOAuthTestAuth(server.URL), request, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					var output []byte
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
					var payload []byte
					select {
					case payload = <-captured:
					default:
						t.Fatal("no upstream request")
					}
					if got := ojson.GetBytes(payload, "model").String(); got != model {
						t.Fatalf("effective model=%q,want %q", got, model)
					}
					if got := ojson.GetBytes(payload, "instructions").String(); !strings.HasPrefix(got, "Final injected instructions.") {
						t.Fatal("payload override missing")
					}
					if ojson.GetBytes(payload, "tools.0.name").String() != "lookup" ||
						ojson.GetBytes(payload, "text.format.name").String() != "answer" ||
						ojson.GetBytes(payload, "input.0.content.1.type").String() != "input_image" {
						t.Fatalf("fixture lost injected tools, schema or image: %s", payload)
					}
					enc, err := helps.CodexInputTokenizer(model)
					if err != nil {
						t.Fatal(err)
					}
					want, err := helps.CountResponsesInputTokens(enc, payload)
					if err != nil {
						t.Fatal(err)
					}
					old, err := helps.CountClaudeInputTokens(original)
					if err != nil {
						t.Fatal(err)
					}
					if want == old {
						t.Fatal("fixture does not distinguish original and final payload estimates")
					}
					var starts, deltas int
					for _, line := range bytes.Split(output, []byte("\n")) {
						data, ok := bytes.CutPrefix(line, []byte("data: "))
						if !ok {
							continue
						}
						event := ojson.ParseBytes(data)
						switch event.Get("type").String() {
						case "message_start":
							starts++
							if got := event.Get("message.usage.input_tokens").Int(); got != want {
								t.Fatalf("start=%d,want %d; output=%s", got, want, output)
							}
						case "message_delta":
							deltas++
							if event.Get("usage.input_tokens").Int() != 75 || event.Get("usage.cache_read_input_tokens").Int() != 25 || event.Get("usage.output_tokens").Int() != 7 {
								t.Fatalf("provider usage changed: %s", data)
							}
						}
					}
					if starts != 1 || deltas != 1 {
						t.Fatalf("starts=%d,deltas=%d; output=%s", starts, deltas, output)
					}
				})
			}
		}
	}
}
