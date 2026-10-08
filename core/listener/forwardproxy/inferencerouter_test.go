package forwardproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter"
	"github.com/rossoctl/cortex/core/session"
)

// seen is every request an origin received, by method and Authorization header —
// HEADs included, unlike origin.gets, so a key on any request is caught.
type seen struct {
	mu    sync.Mutex
	calls []string
}

func (s *seen) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.Header.Get("Authorization"))
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// recordingOrigin is newOrigin with every request's method and Authorization kept.
func recordingOrigin(t *testing.T, name string) (*origin, *seen) {
	t.Helper()
	s := &seen{}
	o := newOrigin(t, name, func(srv *httptest.Server) {
		srv.Config.Handler = s.record(srv.Config.Handler)
		srv.StartTLS()
	})
	return o, s
}

// A client that CONNECTs to one host and names an inference server's host inside the
// bridged TLS must not get that server's key sent to the host it CONNECTed to. The
// router decides on the Host header, which on a bridged request is the client's word,
// while the forward proxy dials the CONNECT authority unless a redirect says
// otherwise. So the router always redirects a routed request to its server, and the
// listener dials RedirectTarget — the server — whatever the Host header or the
// CONNECT named.
func TestConnectBridge_TheRoutersKeyGoesOnlyToItsServer(t *testing.T) {
	const key = "ete-secret-key"
	ete, eteSeen := recordingOrigin(t, "FROM-ETE")
	attacker, attackerSeen := recordingOrigin(t, "FROM-ATTACKER")

	router := inferencerouter.New()
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + ete.URL + `", "key": "` + key + `"}},
		"agents": {"claude-code": "ete"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	// The bridge intercepts the attacker's port, the one the client CONNECTs to.
	// httptest gives every TLS server one certificate, so trusting the attacker's
	// trusts ete's: the bridge would complete either handshake, and only the dial
	// target decides who gets the request.
	engine := redirectBridge(t, portOf(attacker.authority()), certPEM(attacker.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Shared:           shared,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	status, body := bridgedGet(t, proxy, engine.CAPEM, attacker.authority(), "/v1/messages", func(r *http.Request) {
		r.Host = ete.authority()
		r.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
		r.Header.Set("Authorization", "Bearer client-key")
	})

	for _, call := range attackerSeen.all() {
		if call != http.MethodHead+" " {
			t.Errorf("the CONNECT authority received %q; only the bridge's keyless HEAD probe may reach it", call)
		}
	}
	if hosts, _ := attacker.gets(); len(hosts) != 0 {
		t.Errorf("the CONNECT authority served the request (hosts %v): the router's key went where the client CONNECTed", hosts)
	}
	if status != http.StatusOK || body != "FROM-ETE" {
		t.Errorf("response = %d %q, want 200 FROM-ETE: the routed request must reach its server", status, body)
	}
	var routed bool
	for _, call := range eteSeen.all() {
		if call == http.MethodGet+" Bearer "+key {
			routed = true
		}
	}
	if !routed {
		t.Errorf("ete saw %v, want a GET carrying Bearer %s", eteSeen.all(), key)
	}
}
