package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The request is what the server's guards accept: DELETE, and no Origin header.
func TestClearSessions_DecodesCounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/sessions" || r.Header.Get("Origin") != "" {
			t.Errorf("request %s %s origin=%q, want DELETE /v1/sessions with no Origin", r.Method, r.URL, r.Header.Get("Origin"))
		}
		w.Write([]byte(`{"sessions":3,"archivedSessions":40,"bytes":12345}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL).ClearSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 3 || res.ArchivedSessions != 40 || res.Bytes != 12345 || res.ArchiveError != "" {
		t.Fatalf("ClearSessions = %+v", res)
	}
}

// A refusal carries the server's reason, sanitized like every other server string this client
// prints: the endpoint is unauthenticated and this client cannot verify what answered.
func TestClearSessions_403CarriesTheServersReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not bound to loopback only\u001b[2J"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).ClearSessions(context.Background())
	if !errors.Is(err, ErrClearRefused) || !strings.Contains(err.Error(), "not bound to loopback only") {
		t.Fatalf("err = %v, want ErrClearRefused with the server's reason", err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Fatalf("an escape sequence reached the message: %q", err)
	}
}

// When the disk half failed the server still cleared memory, and says how much: the counts are
// returned with the archive's error, not dropped for it.
func TestClearSessions_500DecodesCountsAndArchiveError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"sessions":3,"archivedSessions":0,"bytes":0,"archiveError":"could not move the archive aside\u001b[2J"}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL).ClearSessions(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want the counts with ArchiveError set", err)
	}
	if res.Sessions != 3 || res.ArchiveError != "could not move the archive aside[2J" {
		t.Fatalf("ClearSessions = %+v, want the counts and the error with its escape removed", res)
	}
}

// A proxy that predates the endpoint answers 405, since GET /v1/sessions exists. That is "upgrade
// it", not a failure of the clear.
func TestClearSessions_405IsErrClearUnsupported(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer ts.Close()
	if _, err := New(ts.URL).ClearSessions(context.Background()); !errors.Is(err, ErrClearUnsupported) {
		t.Fatalf("err = %v, want ErrClearUnsupported", err)
	}
}
