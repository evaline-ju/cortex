package inferenceparser

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/internal/parsercommon"
)

// --- request ---

// responsesRequest is the subset of an OpenAI Responses API request we
// surface. Unlike Chat Completions and Anthropic Messages, the conversation
// lives under `input`, not `messages`, and a tool manifest can arrive as its
// own input item (type "additional_tools") rather than a top-level `tools`
// field — see responsesInputItem.
//
// Input is json.RawMessage rather than []responsesInputItem directly: the
// documented API accepts input as EITHER that array OR a bare string (a
// shorthand for one user message, the common one-shot usage shown in
// OpenAI's own docs). Codex always sends the array form — this is a real
// gap for the public endpoint that our one live sample never exercised, not
// something confirmed on live traffic the way the rest of this file is. See
// parseResponsesRequest for where the two shapes are told apart.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Stream          bool            `json:"stream"`
	ToolChoice      any             `json:"tool_choice"`
	Temperature     *float64        `json:"temperature"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	TopP            *float64        `json:"top_p"`
}

// responsesInputItem is one entry of a Responses API request's `input`
// array. Two shapes are read, both confirmed on live Codex traffic:
//
//   - "message": the conversation itself — role + content, where content is
//     an array of parts tagged input_text/output_text/... (not Chat
//     Completions' or Anthropic's "text") carrying a `text` field.
//     flattenResponsesContent keeps any part with non-empty text regardless
//     of its type label, since Codex alone uses more than one and the
//     public API documents others still.
//   - "additional_tools": Codex's own tool-manifest convention — a
//     namespace wrapper (type "namespace", confirmed named "functions" on
//     the traffic this was built from) one level deep, each with its own
//     nested `tools` array holding the real definitions. An unwrapped flat
//     `tools` array from some other Responses API caller is not modeled
//     here and yields no tools for that request, the same outcome this
//     parser already has for any tool definition it cannot read.
//
// Every other item type (reasoning echoes, function_call/function_call_output
// round-trips) is skipped — this parser surfaces conversation text and the
// tool manifest, not the full agent-loop bookkeeping.
type responsesInputItem struct {
	Type         string
	Role         string
	Content      string
	ContentBytes int
	Tools        []pipeline.InferenceTool
}

func (it *responsesInputItem) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type    string                   `json:"type"`
		Role    string                   `json:"role"`
		Content json.RawMessage          `json:"content"`
		Tools   []responsesToolNamespace `json:"tools"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	it.Type = raw.Type
	it.Role = raw.Role
	if raw.Type == "message" {
		it.Content = flattenResponsesContent(raw.Content)
		it.ContentBytes = contentBytes(raw.Content)
	}
	for _, ns := range raw.Tools {
		for _, t := range ns.Tools {
			if t.Name == "" {
				continue
			}
			it.Tools = append(it.Tools, pipeline.InferenceTool{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schemaObject(t.Parameters),
			})
		}
	}
	return nil
}

// responsesToolNamespace is the "namespace" wrapper Codex sends around its
// tool manifest, one level of grouping around the real tool definitions.
type responsesToolNamespace struct {
	Type  string             `json:"type"`
	Name  string             `json:"name"`
	Tools []responsesToolDef `json:"tools"`
}

// responsesToolDef is one real tool definition inside a namespace. Parameters
// is a json.RawMessage for the same reason as inferenceFunction's field of
// the same name: a non-object value must not fail the whole decode. Confirmed
// absent (null) for at least one real tool (a custom code-exec tool with no
// JSON-schema arguments) and present as a real object for others —
// schemaObject already drops the former and keeps the latter.
type responsesToolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// flattenResponsesContent returns the text representation of a Responses API
// `input` message's content value.
//
// Unlike flattenContent (Chat Completions / Anthropic, which gate on
// type=="text"), this keeps any content part with a non-empty text field
// regardless of its type label — confirmed on live Codex traffic to be
// "input_text", and the Responses API documents "output_text" and "refusal"
// for the assistant side. Gating on one label would silently drop every part
// tagged something else, which is exactly the failure flattenContent's own
// doc comment warns about for a provider that adds a new one.
func flattenResponsesContent(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// maxDecodedRequestSize caps how large a zstd-decompressed request body is
// allowed to grow to, independent of the wire-size cap the listener already
// enforces (32 MiB as of this writing). zstd.NewReader's own default
// (WithDecoderMaxMemory) is 64 GiB — a highly compressible body well within
// the wire cap could otherwise decompress far past it. 64 MiB is generous
// relative to any real request seen so far (a live Codex sample decompressed
// to ~65 KB) while still bounding the worst case.
const maxDecodedRequestSize = 64 << 20

// requestZstd is a single decoder shared across requests rather than one
// constructed per call. klauspost/compress/zstd documents DecodeAll as safe
// to call concurrently on one Decoder — the size limit below applies to each
// call independently, not to total memory across concurrent requests.
var requestZstd, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecodedRequestSize))

// maybeDecompressRequest returns body decompressed per the request's
// Content-Encoding header, or body unchanged when the header names an
// encoding this function doesn't handle, or isn't present at all.
//
// zstd only, for now — the one encoding seen on live Codex traffic to this
// endpoint. Nothing upstream of this parser decompresses an inbound request
// body (every listener hands plugins the raw bytes it read off the wire), so
// this is the one dialect among the three that needs its own decode step.
//
// A failed decompression (truncated body, a header that lied, or a body
// that would exceed maxDecodedRequestSize) returns the original compressed
// bytes rather than erroring: the caller's subsequent json.Unmarshal then
// fails too, and the request is treated the same as any other body this
// parser cannot read, rather than this function needing its own error path
// every caller must thread through.
func maybeDecompressRequest(headers http.Header, body []byte) []byte {
	if headers == nil || !strings.EqualFold(headers.Get("Content-Encoding"), "zstd") {
		return body
	}
	out, err := requestZstd.DecodeAll(body, nil)
	if err != nil {
		return body
	}
	return out
}

// parseResponsesRequest builds an InferenceExtension from an OpenAI Responses
// API request body. Returns nil for an empty or non-JSON body, or one
// without a supported input value — the one thing every Responses API
// request carries (see parseOpenAIRequest for why dialectFor's loose suffix
// match needs this kind of body check as a backstop).
func parseResponsesRequest(headers http.Header, body []byte) *pipeline.InferenceExtension {
	body = maybeDecompressRequest(headers, body)
	if len(body) == 0 {
		return nil
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// Input is either the documented array, or a bare string shorthand for one user
	// message. Try the array first — the shape every live sample this file was built
	// from actually sends — and fall back to the string form before giving up.
	var input []responsesInputItem
	if err := json.Unmarshal(req.Input, &input); err != nil {
		var text string
		if err := json.Unmarshal(req.Input, &text); err != nil {
			return nil
		}
		input = []responsesInputItem{{
			Type:         "message",
			Role:         "user",
			Content:      text,
			ContentBytes: contentBytes(req.Input),
		}}
	}
	if input == nil {
		return nil
	}
	ext := &pipeline.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxOutputTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		IsAction:    true,
	}
	for _, item := range input {
		if item.Type == "message" {
			ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
				Role:         item.Role,
				Content:      item.Content,
				ContentBytes: item.ContentBytes,
			})
		}
		ext.Tools = append(ext.Tools, item.Tools...)
	}
	return ext
}

// --- usage (shared by response + streaming) ---

// responsesUsage mirrors the Responses API's usage block, confirmed from a
// live response.completed event. Pointer fields let an absent nested details
// object stay absent in Present rather than asserting a reported zero — the
// same convention the other two dialects use.
//
// usage.attribution (a per-prior-message token breakdown, richer than either
// other dialect's usage block) is not modeled: nothing downstream reads a
// per-item breakdown today, and it is Codex's own addition rather than
// something the published Responses API schema documents. A later consumer
// that wants it can decode it separately as pipeline.RawJSON without
// touching this struct.
type responsesUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
	TotalTokens  *int `json:"total_tokens"`

	InputTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responsesUsage) hasAny() bool {
	return u.InputTokens != nil || u.OutputTokens != nil || u.TotalTokens != nil ||
		u.InputTokensDetails != nil || u.OutputTokensDetails != nil
}

// toNeutral maps the Responses API's usage onto TokenUsage. input_tokens is
// a TOTAL that includes both cached_tokens and cache_write_tokens as
// subsets, not three independent counts — verified by summing a live
// response's usage.attribution.items (a per-prior-message breakdown not
// otherwise modeled here): every item's own input_tokens included its own
// cached_tokens as a subset, and both columns summed across items to
// exactly the top-level input_tokens (12862) and cached_tokens (11008)
// totals on that sample. So both sub-fields are subtracted out here, the
// same shape inferenceUsage.toNeutral uses for Chat Completions'
// prompt_tokens/cached_tokens, extended to the one additional sub-field this
// API reports that Chat Completions doesn't.
func (u responsesUsage) toNeutral() parsercommon.TokenUsage {
	usage := parsercommon.TokenUsage{}
	if u.TotalTokens != nil {
		usage.ReportedTotal = *u.TotalTokens
	}
	if u.InputTokens != nil {
		usage.Input = *u.InputTokens
		usage.Present |= parsercommon.KindInput
	}
	if u.OutputTokens != nil {
		usage.Output = *u.OutputTokens
		usage.Present |= parsercommon.KindOutput
	}
	if u.InputTokensDetails != nil {
		cached := u.InputTokensDetails.CachedTokens
		cacheWrite := u.InputTokensDetails.CacheWriteTokens
		usage.Input -= cached + cacheWrite
		if usage.Input < 0 {
			usage.Input = 0
		}
		usage.CacheRead = cached
		usage.Present |= parsercommon.KindCacheRead
		usage.CacheWrite = cacheWrite
		usage.Present |= parsercommon.KindCacheWrite
	}
	if u.OutputTokensDetails != nil {
		usage.Reasoning = u.OutputTokensDetails.ReasoningTokens
		usage.Present |= parsercommon.KindReasoning
	}
	return usage
}

// --- non-streaming response ---

// responsesNonStreaming is a non-streaming (stream:false) Responses API
// response body: the same "response" object the streamed response's
// terminal response.completed event wraps (see responsesStreamEvent), but
// delivered directly as the whole body with no SSE framing or event
// envelope. Modeled from the published Responses API schema — unlike the
// streamed shape below, this one wasn't exercised by the live traffic this
// file was built from (Codex always sends stream:true).
type responsesNonStreaming struct {
	Status string         `json:"status"`
	Usage  responsesUsage `json:"usage"`
	Output []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

// parseResponsesJSON parses a non-streaming (stream:false) Responses API
// response: output items' text parts -> completion, usage -> token counts,
// status -> a best-effort finish reason (see foldResponsesFrame for why
// this API has no direct finish_reason/stop_reason equivalent).
func parseResponsesJSON(body []byte, ext *pipeline.InferenceExtension) {
	var resp responsesNonStreaming
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Debug("inference-parser: invalid Responses API JSON", "error", err)
		return
	}
	var b strings.Builder
	for _, item := range resp.Output {
		for _, part := range item.Content {
			if part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
	}
	ext.Completion = b.String()
	if resp.Status != "" {
		ext.FinishReason = resp.Status
	}
	if resp.Usage.hasAny() {
		resp.Usage.toNeutral().Fill(ext)
	}
}

// --- streaming ---

// responsesStreamEvent is one SSE event's data payload. The Responses API
// stream is a sequence of typed events (vs OpenAI Chat Completions' uniform
// chunk): response.created, response.in_progress,
// response.output_item.added, response.content_part.added,
// response.output_text.delta (repeated), response.output_text.done,
// response.content_part.done, response.output_item.done, response.completed
// — the full sequence confirmed on live Codex traffic. Only the two event
// types that carry something this parser extracts are modeled: the
// repeated text delta, and the terminal event's full response snapshot
// (status + usage).
type responsesStreamEvent struct {
	Type     string  `json:"type"`
	Delta    *string `json:"delta"` // response.output_text.delta's text fragment
	Response *struct {
		Status string         `json:"status"`
		Usage  responsesUsage `json:"usage"`
	} `json:"response"` // the full snapshot on response.completed/incomplete/failed
}

// foldResponsesFrame folds one Responses API SSE event into the running
// stream state. The completion accumulates from response.output_text.delta
// events; usage and a best-effort finish reason arrive together on whichever
// terminal event ends the stream — response.completed on the one live sample
// this file was built from, plus the two other terminal events the published
// schema documents: response.incomplete (hit a limit, was cancelled) and
// response.failed (an upstream error). All three carry the same response
// snapshot shape, so one case handles them identically; OpenAI's own
// documented example for response.failed shows usage: null, so the
// hasAny() guard below is what keeps a failure from asserting a zero usage
// that was never reported. Every other event type in the sequence carries
// nothing this parser extracts and is silently ignored — not unrecognized,
// just uninteresting. Unlike foldAnthropicFrame, an unknown type is not
// logged here: the full event vocabulary was confirmed on live traffic
// rather than inferred from docs, so anything outside it is more likely a
// wire change worth its own look than routine.
func foldResponsesFrame(frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
	var ev responsesStreamEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		slog.Debug("inference-parser: malformed Responses API streaming frame, skipping",
			"error", err, "frameLen", len(frame))
		return
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != nil {
			state.completion.WriteString(*ev.Delta)
		}
	case "response.completed", "response.incomplete", "response.failed":
		if ev.Response == nil {
			return
		}
		if ev.Response.Status != "" {
			ext.FinishReason = ev.Response.Status
		}
		if ev.Response.Usage.hasAny() {
			state.usage = ev.Response.Usage.toNeutral()
			state.hasUsage = true
		}
	}
}

// parseResponsesSSE folds a fully-buffered Responses API SSE body. Mirrors
// parseAnthropicSSE/parseInferenceSSE for the legacy OnResponse path; the
// live listener uses foldResponsesFrame via OnResponseFrame instead.
func parseResponsesSSE(body []byte, ext *pipeline.InferenceExtension) {
	markStreamedResponse(body, ext)
	state := &inferenceStreamState{}
	for _, line := range bytes.Split(normalizeSSE(body), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		foldResponsesFrame(data, state, ext)
	}
	state.finalize(ext)
}
