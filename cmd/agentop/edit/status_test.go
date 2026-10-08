package edit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeStatusServer returns a httptest.Server whose /reload/status
// endpoint reads its body from the supplied function — letting the
// test advance state between polls.
func makeStatusServer(t *testing.T, body func() ReloadStatus) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPollUntilReloaded_Success(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		if c == 1 {
			return ReloadStatus{LastSuccess: applyTime.Add(-1 * time.Hour)}
		}
		return ReloadStatus{LastSuccess: time.Now()}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime, "")
	if res.Status != PollSuccess {
		t.Fatalf("status = %v, want PollSuccess", res.Status)
	}
}

func TestPollUntilReloaded_Failure(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		if c == 1 {
			return ReloadStatus{ReloadsFailed: 5}
		}
		return ReloadStatus{ReloadsFailed: 6, LastError: "invalid YAML at line 3"}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime, "")
	if res.Status != PollFailure {
		t.Fatalf("status = %v, want PollFailure", res.Status)
	}
	if res.LastError != "invalid YAML at line 3" {
		t.Fatalf("LastError: %q", res.LastError)
	}
}

func TestPollUntilReloaded_Timeout(t *testing.T) {
	applyTime := time.Now()
	srv := makeStatusServer(t, func() ReloadStatus {
		return ReloadStatus{LastSuccess: applyTime.Add(-1 * time.Hour)}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, applyTime, "")
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

func TestPollUntilReloaded_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, time.Now(), "")
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

// A refusal and an endpoint that stopped answering are both PollFailure; only the
// second is Unreachable, which is what lets a caller say which happened.
func TestPollUntilReloaded_MarksAnEndpointThatStoppedAnsweringUnreachable(t *testing.T) {
	shortenPollSchedule(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(ReloadStatus{})
			return
		}
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	res := PollUntilReloaded(context.Background(), srv.URL, time.Now(), "")
	if res.Status != PollFailure || !res.Unreachable || !strings.Contains(res.LastError, "unreachable") {
		t.Fatalf("result = %+v, want an Unreachable PollFailure", res)
	}

	var polls atomic.Int32
	refused := makeStatusServer(t, func() ReloadStatus {
		if polls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 0}
		}
		return ReloadStatus{ReloadsFailed: 1, LastError: "bad"}
	})
	if res := PollUntilReloaded(context.Background(), refused.URL, time.Now(), ""); res.Status != PollFailure || res.Unreachable {
		t.Fatalf("refusal = %+v, want a PollFailure that is not Unreachable", res)
	}
}
