package routerconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

const twoServers = `{
	"servers": {
		"ete": {"url": "https://ete.example.com", "key": "sk-ete"},
		"glm": {"url": "https://glm.example.com:8443", "key": "sk-glm"}
	},
	"agents": {"claude-code": "glm"}
}`

func TestDecode_AcceptsServersAndAgents(t *testing.T) {
	c, err := Decode(json.RawMessage(twoServers))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Servers) != 2 || c.Agents["claude-code"] != "glm" {
		t.Errorf("decoded %+v", c)
	}
}

func TestDecode_NoAgentsIsAnEmptyMap(t *testing.T) {
	c, err := Decode(json.RawMessage(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k"}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Agents == nil {
		t.Error("Agents is nil; an absent agents block routes nothing, as an empty one does")
	}
}

// A default server is the feature this design leaves out on purpose; a config that
// asks for one must be told, not silently route nothing.
func TestDecode_RefusesUnknownFields(t *testing.T) {
	_, err := Decode(json.RawMessage(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k"}}, "default": "ete"}`))
	if err == nil || !strings.Contains(err.Error(), `"default"`) {
		t.Fatalf("err = %v, want an unknown-field error naming default", err)
	}
}

func TestValidate_RefusesEachBrokenRule(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"no servers", `{"servers": {}}`, "at least one server"},
		{"absent servers", `{}`, "at least one server"},
		{"uppercase name", `{"servers": {"ETE": {"url": "https://e.example", "key": "k"}}}`, `"ETE" is not a server name`},
		{"name with a space", `{"servers": {"e te": {"url": "https://e.example", "key": "k"}}}`, `"e te" is not a server name`},
		{"ftp", `{"servers": {"e": {"url": "ftp://e.example", "key": "k"}}}`, "scheme must be http or https"},
		{"no scheme", `{"servers": {"e": {"url": "e.example", "key": "k"}}}`, "scheme must be http or https"},
		{"no host", `{"servers": {"e": {"url": "https://", "key": "k"}}}`, "has no host"},
		{"path", `{"servers": {"e": {"url": "https://e.example/v1", "key": "k"}}}`, "has a path"},
		{"query", `{"servers": {"e": {"url": "https://e.example?x=1", "key": "k"}}}`, "query or fragment"},
		{"fragment", `{"servers": {"e": {"url": "https://e.example#x", "key": "k"}}}`, "query or fragment"},
		{"user info", `{"servers": {"e": {"url": "https://u:p@e.example", "key": "k"}}}`, "user info"},
		{"non-numeric port", `{"servers": {"e": {"url": "https://e.example:abc", "key": "k"}}}`, "servers.e.url: not a valid URL"},
		{"port zero", `{"servers": {"e": {"url": "https://e.example:0", "key": "k"}}}`, "port must be a number from 1 to 65535"},
		{"port too high", `{"servers": {"e": {"url": "https://e.example:65536", "key": "k"}}}`, "port must be a number from 1 to 65535"},
		{"shared host, port and case ignored", `{"servers": {
			"a": {"url": "https://gw.example", "key": "k"},
			"b": {"url": "http://GW.example:4000", "key": "k"}}}`, `"a" and "b" are both on gw.example`},
		{"empty key", `{"servers": {"e": {"url": "https://e.example", "key": ""}}}`, "the key is empty"},
		{"key with a space", `{"servers": {"e": {"url": "https://e.example", "key": "sk 1"}}}`, "printable ASCII with no spaces"},
		{"agent naming no server", `{"servers": {"e": {"url": "https://e.example", "key": "k"}}, "agents": {"claude-code": "glm"}}`, `"glm" is not a server listed under servers`},
		{"agent name as a label", `{"servers": {"e": {"url": "https://e.example", "key": "k"}}, "agents": {"Claude Code": "e"}}`, `"Claude Code" is not an agent name`},
		{"the no-User-Agent bucket", `{"servers": {"e": {"url": "https://e.example", "key": "k"}}, "agents": {"unknown": "e"}}`, `"unknown" cannot be routed`},
		{"opus", `{"servers": {"e": {"url": "https://e.example", "key": "k", "opus": "glm-5.3"}}}`, "model mapping needs chained body writers (PR 4)"},
		{"haiku", `{"servers": {"e": {"url": "https://e.example", "key": "k", "haiku": "glm-5.3"}}}`, "model mapping needs chained body writers (PR 4)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(json.RawMessage(tc.config))
			if err == nil {
				t.Fatal("Decode accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The key is a secret; an error about it must not print it.
func TestCheckKey_NeverRepeatsTheKey(t *testing.T) {
	err := CheckKey("sk-secret value")
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("err = %v; want an error that does not contain the key", err)
	}
}

func TestParseURL_Normalises(t *testing.T) {
	for _, tc := range []struct {
		raw, url, host, hostname string
	}{
		{"https://ete.example.com", "https://ete.example.com", "ete.example.com", "ete.example.com"},
		{"https://ETE.Example.com:443/", "https://ete.example.com", "ete.example.com", "ete.example.com"},
		{"http://localhost:80", "http://localhost", "localhost", "localhost"},
		{"https://glm.example.com:8443", "https://glm.example.com:8443", "glm.example.com:8443", "glm.example.com"},
		{"https://x.example:0443", "https://x.example", "x.example", "x.example"},
		{"http://[::1]:4000", "http://[::1]:4000", "[::1]:4000", "::1"},
		{"https://[::1]:443", "https://[::1]", "[::1]", "::1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			ep, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			if ep.URL() != tc.url || ep.Host != tc.host || ep.Hostname != tc.hostname {
				t.Errorf("got URL %q Host %q Hostname %q, want %q %q %q", ep.URL(), ep.Host, ep.Hostname, tc.url, tc.host, tc.hostname)
			}
		})
	}
}

func TestNormalHost_DropsOnlyTheSchemesDefaultPort(t *testing.T) {
	for _, tc := range []struct{ scheme, hostport, want string }{
		{"https", "ete.example.com", "ete.example.com"},
		{"https", "ETE.example.com:443", "ete.example.com"},
		{"http", "localhost:80", "localhost"},
		{"https", "x.example:80", "x.example:80"},
		{"http", "x.example:443", "x.example:443"},
		{"https", "x.example:8443", "x.example:8443"},
		{"https", "[::1]:443", "[::1]"},
		{"http", "[::1]:4000", "[::1]:4000"},
	} {
		if got := NormalHost(tc.scheme, tc.hostport); got != tc.want {
			t.Errorf("NormalHost(%q, %q) = %q, want %q", tc.scheme, tc.hostport, got, tc.want)
		}
	}
}

func TestHostname_StripsThePortAndCase(t *testing.T) {
	for in, want := range map[string]string{
		"ete.example.com":      "ete.example.com",
		"ETE.example.com:8443": "ete.example.com",
		"[::1]:4000":           "::1",
		"[::1]":                "::1",
	} {
		if got := Hostname(in); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaintextRemote(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://gw.example":     false,
		"http://localhost:4000":  false,
		"http://127.0.0.1:4000":  false,
		"http://[::1]:4000":      false,
		"http://10.0.0.5:4000":   true,
		"http://gateway.lan":     true,
		"http://LOCALHOST:4000/": false,
	} {
		ep, err := ParseURL(raw)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", raw, err)
		}
		if got := ep.PlaintextRemote(); got != want {
			t.Errorf("PlaintextRemote(%q) = %v, want %v", raw, got, want)
		}
	}
}

// No ParseURL error quotes any part of the URL. Errors reach the logs and the
// unauthenticated /reload/status, and a key can land anywhere in a mistyped URL —
// user info, query, fragment, a path segment, or wherever url.Parse files it when
// the "//" is wrong — so redacting shape by shape kept missing one. Each error is
// fixed text naming the problem; this table is every input that once leaked.
func TestParseURL_QuotesNoPartOfTheURL(t *testing.T) {
	const (
		invalid = "not a valid URL"
		scheme  = "the URL's scheme must be http or https"
		noHost  = "the URL has no host"
		user    = "the URL carries user info"
		path    = "the URL has a path"
		query   = "the URL has a query or fragment"
	)
	for _, tc := range []struct{ url, want string }{
		// User info, query and fragment.
		{"https://sk-SECRET@h.example.com", user},
		{"https://u:sk-SECRET@h.example.com", user},
		{"ftp://u:sk-SECRET@h.example.com", scheme},
		{"https://u:sk-SECRET@", noHost},
		{"https:u:sk-SECRET@h.example.com", noHost},
		{"https://h.example.com/?key=sk-SECRET", query},
		{"https://h.example.com/#sk-SECRET", query},
		{"https://h.example.com/?a=1#sk-SECRET", query},
		{"ftp://h.example.com/?key=sk-SECRET", scheme},
		// A mistyped "//" files the key under the path.
		{"https:/u:sk-SECRET@h.example.com", noHost},
		{"https:///u:sk-SECRET@h.example.com", noHost},
		{"https//u:sk-SECRET@h.example.com", scheme},
		{"sk-SECRET@h.example.com", scheme},
		// url.Parse's own errors quote the bad port, or the escape.
		{"https://u:sk-SECRET/x@h.example.com", invalid},
		{"https://u:sk-SECRET#x@h.example.com", invalid},
		{"https://u:sk-SECRET?x@h.example.com", invalid},
		{"https://h.example.com:sk-SECRET", invalid},
		{"https://[::1]:sk-SECRET", invalid},
		{"https://u:sk-SECRET@h.example.com:abc", invalid},
		{"https://u:sk-SECRET%ZZ@h.example.com", invalid},
		// A key as a path segment.
		{"https://h.example.com/sk-SECRET", path},
	} {
		_, err := ParseURL(tc.url)
		if err == nil {
			t.Errorf("ParseURL(%q) accepted it", tc.url)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "sk-SECRET") || strings.Contains(msg, "u:") || strings.Contains(msg, "example.com") || strings.Contains(msg, "::1") {
			t.Errorf("ParseURL(%q) error quotes the URL: %q", tc.url, msg)
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("ParseURL(%q) error = %q, want %q", tc.url, msg, tc.want)
		}
	}
}
