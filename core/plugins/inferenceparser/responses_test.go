package inferenceparser

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestInferenceParser_ResponsesAPI_Request(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/backend-api/codex/responses",
		Body: []byte(`{
			"model": "gpt-6-luna",
			"stream": true,
			"tool_choice": "auto",
			"input": [
				{
					"type": "additional_tools",
					"id": "at_1",
					"role": "developer",
					"tools": [
						{
							"type": "namespace",
							"name": "functions",
							"description": "",
							"tools": [
								{"type": "custom", "name": "exec", "description": "run code"},
								{"type": "function", "name": "wait", "description": "wait for output",
									"parameters": {"type": "object", "properties": {}}}
							]
						}
					]
				},
				{
					"type": "message",
					"id": "msg_1",
					"role": "developer",
					"content": [{"type": "input_text", "text": "You are Codex."}]
				},
				{
					"type": "message",
					"id": "msg_2",
					"role": "user",
					"content": [{"type": "input_text", "text": "reply with just the word hello"}]
				}
			]
		}`),
	}

	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — /backend-api/codex/responses not parsed")
	}
	if ext.Model != "gpt-6-luna" {
		t.Errorf("Model = %q, want gpt-6-luna", ext.Model)
	}
	if !ext.IsAction {
		t.Error("IsAction should be true for an inference request")
	}
	// Only "message" items become Messages — the "additional_tools" item is a
	// tool manifest, not a conversation turn.
	if len(ext.Messages) != 2 || ext.Messages[0].Role != "developer" || ext.Messages[1].Role != "user" {
		t.Fatalf("Messages = %+v, want [developer, user]", ext.Messages)
	}
	if ext.Messages[1].Content != "reply with just the word hello" {
		t.Errorf("user content = %q", ext.Messages[1].Content)
	}
	// "exec" has no parameters object (a custom code-exec tool) and is still
	// surfaced; "wait" carries a real schema.
	if len(ext.Tools) != 2 || ext.Tools[0].Name != "exec" || ext.Tools[1].Name != "wait" {
		t.Fatalf("Tools = %+v, want [exec, wait]", ext.Tools)
	}
	if ext.Tools[0].Parameters != "" {
		t.Errorf("exec Parameters = %q, want empty (no schema on the wire)", ext.Tools[0].Parameters)
	}
	if ext.Tools[1].Parameters == "" {
		t.Error("wait Parameters is empty, want its object schema")
	}
}

// A request whose path ends in "/responses" but whose body carries no input array is not
// an inference request — the same backstop parseOpenAIRequest and parseAnthropicRequest
// have for their own loose suffix match.
func TestInferenceParser_ResponsesAPI_NonInferenceBodyIgnored(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/responses",
		Body: []byte(`{"id": "resp_1", "status": "completed"}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference != nil {
		t.Errorf("Extensions.Inference = %+v, want nil for a body with no input array", pctx.Extensions.Inference)
	}
}

// Codex's real request arrives zstd-compressed. A corrupt or absent decompression step
// would leave json.Unmarshal failing on compressed bytes, so this pins decode-before-parse
// rather than relying on parseResponsesRequest's nil-on-bad-JSON fallback to mask it.
func TestInferenceParser_ResponsesAPI_ZstdCompressedRequest(t *testing.T) {
	plain := []byte(`{"model":"gpt-6-luna","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(plain, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("enc.Close: %v", err)
	}

	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path:    "/backend-api/codex/responses",
		Headers: http.Header{"Content-Encoding": []string{"zstd"}},
		Body:    compressed,
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — zstd-compressed body was not decompressed before parsing")
	}
	if ext.Model != "gpt-6-luna" || len(ext.Messages) != 1 || ext.Messages[0].Content != "hi" {
		t.Errorf("ext = %+v, want model gpt-6-luna / one message \"hi\"", ext)
	}
}

// The real event sequence confirmed on live Codex traffic, sanitized to the "hello" fixture
// used throughout this capture. Usage figures match the real response.completed event:
// input_tokens=12862 (of which cached_tokens=11008), output_tokens=5, total_tokens=12867.
func TestInferenceParser_ResponsesAPI_StreamFoldsEvents(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`),
		[]byte(`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`),
		[]byte(`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`),
		[]byte(`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{` +
			`"input_tokens":12862,"input_tokens_details":{"cached_tokens":11008,"cache_write_tokens":0},` +
			`"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":12867}}}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello", ext.Completion)
	}
	if ext.FinishReason != "completed" {
		t.Errorf("FinishReason = %q, want completed (the terminal event's own status)", ext.FinishReason)
	}
	// Input is reported MINUS the cached+cache-write subsets: 12862 - 11008 - 0 = 1854.
	if ext.InputTokens != 1854 {
		t.Errorf("InputTokens = %d, want 1854 (12862 - 11008 cached)", ext.InputTokens)
	}
	if ext.CacheReadTokens != 11008 {
		t.Errorf("CacheReadTokens = %d, want 11008", ext.CacheReadTokens)
	}
	if ext.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0", ext.CacheWriteTokens)
	}
	if ext.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", ext.OutputTokens)
	}
	if ext.TotalTokens != 12867 {
		t.Errorf("TotalTokens = %d, want 12867 (the provider's own reported total)", ext.TotalTokens)
	}
}

// Codex's response arrives as real SSE wire bytes delivered whole on the terminal call
// (the buffered-fallback shape, same as any other listener-buffered SSE body) but under
// Content-Type: application/json rather than text/event-stream — the mislabeling
// confirmed on live traffic. settle.IsEventStream(pctx) reads that (wrong) header and
// says no; carriesSSEFraming(frame) reads the actual bytes and says yes. This pins that
// the fallback engages: parsing must not silently fail just because the header lied.
func TestInferenceParser_ResponsesAPI_BufferedFallback_MislabeledContentType(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}
	// The listener clones the real (wrong) response header onto pctx before dispatch.
	pctx.ResponseHeaders = http.Header{"Content-Type": []string{"application/json"}}

	var body bytes.Buffer
	for _, line := range []string{
		`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n",
		`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"status":"completed",` +
			`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\n",
	} {
		body.WriteString(line)
	}

	p.OnResponseFrame(context.Background(), pctx, body.Bytes(), true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello — carriesSSEFraming fallback did not engage", ext.Completion)
	}
	if ext.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", ext.TotalTokens)
	}
}

// A non-streaming (stream:false) Responses API reply, delivered as one plain JSON object
// — not SSE at all. Modeled from the published schema; not exercised by live Codex traffic
// (which always sends stream:true), so this only pins the shape this parser expects.
func TestInferenceParser_ResponsesAPI_NonStreamingResponse(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", IsAction: true}

	body := []byte(`{
		"status": "completed",
		"output": [{"content": [{"type": "output_text", "text": "hello"}]}],
		"usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello", ext.Completion)
	}
	if ext.FinishReason != "completed" {
		t.Errorf("FinishReason = %q, want completed", ext.FinishReason)
	}
	if ext.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", ext.TotalTokens)
	}
}
