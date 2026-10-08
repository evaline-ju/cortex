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
		{"non-numeric port", `{"servers": {"e": {"url": "https://e.example:abc", "key": "k"}}}`, "invalid port"},
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

// ParseURL must not expose credentials in its error messages, since errors
// go to logs and the unauthenticated /reload/status endpoint.
func TestParseURL_KeepsCredentialsOutOfErrors(t *testing.T) {
	testCases := []struct {
		name, url string
	}{
		{"bad port with userinfo", "https://user:pw@example.com:abc"},
		{"bad percent-escape with userinfo", "https://user:pw%ZZ@example.com"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseURL(tc.url)
			if err == nil {
				t.Fatal("ParseURL accepted invalid URL")
			}
			if strings.Contains(err.Error(), "pw") {
				t.Errorf("error exposes password: %v", err)
			}
			if strings.Contains(err.Error(), "user") {
				t.Errorf("error exposes username: %v", err)
			}
		})
	}
}

// User info is where a pasted key sits, and it may be the username alone, which
// url.URL.Redacted leaves intact. Every error a URL with user info can reach must
// drop it whole, and still name the host so the message points somewhere.
func TestParseURL_KeepsUserInfoOutOfEveryError(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
	}{
		{"key as the username", "https://sk-SECRET@h.example.com", `"https://h.example.com" carries user info`},
		{"key as the password", "https://u:sk-SECRET@h.example.com", `"https://h.example.com" carries user info`},
		{"wrong scheme", "ftp://u:sk-SECRET@h.example.com", `"ftp://h.example.com": the scheme must be http or https`},
		{"no host", "https://u:sk-SECRET@", "has no host"},
		{"no slashes", "https:u:sk-SECRET@h.example.com", `"https:h.example.com" has no host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseURL(tc.url)
			if err == nil {
				t.Fatal("ParseURL accepted a URL with user info")
			}
			if msg := err.Error(); strings.Contains(msg, "sk-SECRET") || strings.Contains(msg, "u:") || !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want %q in it and no user info", msg, tc.want)
			}
		})
	}
}

// A query or fragment can carry a key as surely as user info can
// (?key=..., #token), so neither reaches an error either.
func TestParseURL_KeepsQueryAndFragmentOutOfErrors(t *testing.T) {
	for _, raw := range []string{
		"https://h.example.com/?key=sk-SECRET",
		"https://h.example.com/#sk-SECRET",
		"https://h.example.com/?a=1#sk-SECRET",
		"ftp://h.example.com/?key=sk-SECRET",
	} {
		_, err := ParseURL(raw)
		if err == nil {
			t.Fatalf("ParseURL(%q) accepted a query or fragment", raw)
		}
		if msg := err.Error(); strings.Contains(msg, "sk-SECRET") || !strings.Contains(msg, "h.example.com") {
			t.Errorf("ParseURL(%q) error = %q, want the host named and no query or fragment", raw, msg)
		}
	}
}
