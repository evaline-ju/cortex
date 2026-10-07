package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestDialectFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want dialect
	}{
		// Anthropic Messages at the root and under every prefix seen so far.
		{"/v1/messages", dialectAnthropic},
		{"/zen/v1/messages", dialectAnthropic},
		{"/zen/go/v1/messages", dialectAnthropic},
		{"/anthropic/v1/messages", dialectAnthropic},
		// OpenAI chat completions and legacy completions, likewise.
		{"/v1/chat/completions", dialectOpenAI},
		{"/chat/completions", dialectOpenAI},
		{"/v1/completions", dialectOpenAI},
		{"/completions", dialectOpenAI},
		{"/inference/v1/chat/completions", dialectOpenAI},
		{"/zen/v1/chat/completions", dialectOpenAI},
		{"/zen/go/v1/chat/completions", dialectOpenAI},
		{"/api/v1/chat/completions", dialectOpenAI},
		{"/openai/v1/chat/completions", dialectOpenAI},
		{"/openai/deployments/gpt-4o/chat/completions", dialectOpenAI},
		{"/v1beta/openai/chat/completions", dialectOpenAI},
		// OpenAI Responses API: the public endpoint, and Codex's chatgpt.com-hosted
		// gateway — sharing no prefix, only the "/responses" ending.
		{"/v1/responses", dialectResponses},
		{"/backend-api/codex/responses", dialectResponses},
		// Endpoints beside an inference one, sharing its prefix but not its ending.
		{"/v1/messages/count_tokens", dialectNone},
		{"/v1/messages/batches", dialectNone},
		{"/v1/embeddings", dialectNone},
		{"/v1/rerank", dialectNone},
		{"/inference/v1/model/info", dialectNone},
		{"/v1/autocompletions", dialectNone},
		{"/foov1/messages", dialectNone},                 // the segment boundary, for the suffix checked first
		{"/inference/v1/chat/completions/", dialectNone}, // trailing slash: not trimmed
		{"", dialectNone},
		{"/", dialectNone},
	} {
		if got := dialectFor(tc.path); got != tc.want {
			t.Errorf("dialectFor(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

// A provider mounted under a prefix of its own is parsed, and its stream read as the same
// dialect: OpenRouter's /api prefix.
func TestInferenceParser_PrefixedOpenAIPath_StreamedEndToEnd(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/api/v1/chat/completions",
		Body: []byte(`{"model":"anthropic/claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil for /api/v1/chat/completions")
	}
	for _, f := range []string{
		`{"choices":[{"delta":{"content":"po"}}]}`,
		`{"choices":[{"delta":{"content":"ng"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
	} {
		if action := p.OnResponseFrame(context.Background(), pctx, []byte(f), false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "pong" || ext.FinishReason != "stop" {
		t.Errorf("Completion = %q, FinishReason = %q, want pong / stop", ext.Completion, ext.FinishReason)
	}
	if ext.PromptTokens != 9 || ext.CompletionTokens != 2 {
		t.Errorf("tokens = %d/%d, want 9/2", ext.PromptTokens, ext.CompletionTokens)
	}
}

// Azure names the deployment in the path and sends no model. The request is still
// inference, with an empty model, and its response still yields tokens and a finish
// reason.
func TestInferenceParser_AzureDeploymentPath_NoModel(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/openai/deployments/gpt-4o/chat/completions",
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil for an Azure deployment path")
	}
	if ext.Model != "" {
		t.Errorf("Model = %q, want empty: the body named none", ext.Model)
	}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)
	if ext.Completion != "pong" || ext.PromptTokens != 9 || ext.CompletionTokens != 2 {
		t.Errorf("Completion = %q, tokens = %d/%d; want pong, 9/2", ext.Completion, ext.PromptTokens, ext.CompletionTokens)
	}
	if ext.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", ext.FinishReason)
	}
}

// A gateway's /anthropic prefix routes the response to the Anthropic reader too. Read as
// OpenAI, this stream yields no tokens at all.
func TestInferenceParser_PrefixedAnthropicPath_StreamedEndToEnd(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/anthropic/v1/messages",
		Body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil for /anthropic/v1/messages")
	}
	for _, f := range []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":25,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`,
		`{"type":"message_stop"}`,
	} {
		if action := p.OnResponseFrame(context.Background(), pctx, []byte(f), false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "pong" || ext.FinishReason != "end_turn" {
		t.Errorf("Completion = %q, FinishReason = %q, want pong / end_turn", ext.Completion, ext.FinishReason)
	}
	if ext.PromptTokens != 25 || ext.CompletionTokens != 15 {
		t.Errorf("tokens = %d/%d, want 25/15", ext.PromptTokens, ext.CompletionTokens)
	}
}
