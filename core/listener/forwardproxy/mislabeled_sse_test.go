package forwardproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestForwardProxy_MislabeledSSE_ErrorResponseStaysBuffered pins the fix for a real
// regression: isKnownMislabeledSSE used to match every response from the one known
// mislabeled endpoint regardless of status code, which routed error bodies (401, 405,
// 429 — all observed on live Codex traffic) into the SSE streaming path. sseframe.Reader
// only emits "data:"-prefixed frames, so a plain JSON error body like
// {"detail":"Unauthorized"} produced zero frames and the client received an empty body —
// confirmed on live traffic, where a 405's real body `{"detail":"Method Not Allowed"}`
// arrived as the literal string "Unknown error" once the allowlist started matching
// every status. The fix gates the allowlist on 2xx only; this test pins a 401 from the
// same host+path and asserts the real error body survives intact.
func TestForwardProxy_MislabeledSSE_ErrorResponseStaysBuffered(t *testing.T) {
	const errBody = `{"detail":"Unauthorized"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The real mislabeling: SSE-adjacent endpoint, but this response is a plain
		// JSON error, labeled (also) as application/json.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(errBody))
	}))
	defer backend.Close()

	probe := newStreamingProbe(false)
	pp, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if !pp.HasStreamingResponders() {
		t.Fatal("probe must register as a StreamingResponder for this test to exercise the streaming branch")
	}

	backendAddr := strings.TrimPrefix(backend.URL, "http://")
	srv, err := NewServer(pipeline.NewHolder(pp), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Redirect the proxy's own outbound dial for the fake host to the real local
	// backend, while leaving the request's Host/URL as the literal "chatgpt.com" that
	// isKnownMislabeledSSE checks — the same way a real client's request to that host
	// would look to the proxy, without needing real DNS or network access.
	srv.Client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == "chatgpt.com:80" {
					addr = backendAddr
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
	}
	req, _ := http.NewRequest("GET", "http://chatgpt.com/backend-api/codex/responses", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if string(body) != errBody {
		t.Errorf("body = %q, want %q — the error detail must survive, not be swallowed by the SSE path", body, errBody)
	}
}
