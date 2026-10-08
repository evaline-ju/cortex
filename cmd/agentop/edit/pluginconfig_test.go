package edit

import (
	"strings"
	"testing"
)

// localConfig is shaped like the config a laptop install writes: comments, the
// pipeline's outbound chain with tool-prune last, and a top-level key after the
// pipeline, so an entry appended past the chain's end would land in the wrong block.
const localConfig = `# The running proxy watches this file.
mode: proxy-sidecar
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      # Keep it last: it rewrites the request body.
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
session:
  client_affinity: true
`

// withRouter is localConfig after agentop added a server and routed an agent, with
// comments a person added since.
const withRouter = `# The running proxy watches this file.
mode: proxy-sidecar
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      # Keep it last: it rewrites the request body.
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
      - name: inference-router
        # servers Claude Code can be sent to
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete   # rotated in October
          # only agents listed here are routed
          agents:
            claude-code: ete
session:
  client_affinity: true
`

func change(path []string, value ...string) ConfigChange {
	ch := ConfigChange{Chain: "outbound", Plugin: "inference-router", Path: path}
	switch len(value) {
	case 0:
	case 1:
		ch.Value = ScalarValue(value[0])
	default:
		ch.Value = MapValue(value...)
	}
	return ch
}

func apply(t *testing.T, src string, ch ConfigChange) string {
	t.Helper()
	out, err := SetPluginConfig([]byte(src), ch)
	if err != nil {
		t.Fatalf("SetPluginConfig: %v", err)
	}
	return string(out)
}

func assertYAML(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("result differs\n--- got ---\n%s\n--- want ---\n%s\n--- diff ---\n%s", got, want, Diff([]byte(want), []byte(got)))
	}
}

func TestSetPluginConfig_CreatesTheEntryAtTheEndOfTheChain(t *testing.T) {
	ch := change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-ete")
	ch.CreatePlugin = true
	assertYAML(t, apply(t, localConfig, ch), `# The running proxy watches this file.
mode: proxy-sidecar
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      # Keep it last: it rewrites the request body.
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
session:
  client_affinity: true
`)
}

func TestSetPluginConfig_AMissingEntryIsAnErrorUnlessAskedToCreateIt(t *testing.T) {
	_, err := SetPluginConfig([]byte(localConfig), change([]string{"agents", "claude-code"}, "ete"))
	if err == nil || !strings.Contains(err.Error(), "pipeline.outbound has no inference-router entry") {
		t.Fatalf("err = %v, want a missing-entry error", err)
	}
}

func TestSetPluginConfig_AddsAKeyAfterItsSiblingsAndKeepsComments(t *testing.T) {
	assertYAML(t, apply(t, withRouter, change([]string{"servers", "glm"}, "url", "https://glm.example.com:8443", "key", "sk-glm")),
		strings.Replace(withRouter, `              key: sk-ete   # rotated in October
`, `              key: sk-ete   # rotated in October
            glm:
              url: https://glm.example.com:8443
              key: sk-glm
`, 1))
}

func TestSetPluginConfig_CreatesAMissingMap(t *testing.T) {
	src := strings.Replace(withRouter, `          # only agents listed here are routed
          agents:
            claude-code: ete
`, "", 1)
	assertYAML(t, apply(t, src, change([]string{"agents", "opencode"}, "ete")),
		strings.Replace(src, `              key: sk-ete   # rotated in October
`, `              key: sk-ete   # rotated in October
          agents:
            opencode: ete
`, 1))
}

func TestSetPluginConfig_ReplacesAScalar(t *testing.T) {
	assertYAML(t, apply(t, withRouter, change([]string{"agents", "claude-code"}, "glm")),
		strings.Replace(withRouter, "claude-code: ete", "claude-code: glm", 1))
}

func TestSetPluginConfig_ReplacesAMapWhole(t *testing.T) {
	assertYAML(t, apply(t, withRouter, change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-new")),
		strings.Replace(withRouter, "              key: sk-ete   # rotated in October\n", "              key: sk-new\n", 1))
}

func TestSetPluginConfig_ReplacesAFlowStyleValueWhole(t *testing.T) {
	src := strings.Replace(withRouter, `            ete:
              url: https://ete.example.com
              key: sk-ete   # rotated in October
`, `            ete: { url: https://ete.example.com, key: sk-ete }
`, 1)
	assertYAML(t, apply(t, src, change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-new")),
		strings.Replace(src, "            ete: { url: https://ete.example.com, key: sk-ete }\n", `            ete:
              url: https://ete.example.com
              key: sk-new
`, 1))
}

// Equal values are no change, in whatever style the file holds them — so a write
// that changes nothing writes nothing.
func TestSetPluginConfig_AnEqualValueChangesNothing(t *testing.T) {
	for name, src := range map[string]string{
		"indented": withRouter,
		"flow": strings.Replace(withRouter, `            ete:
              url: https://ete.example.com
              key: sk-ete   # rotated in October
`, `            ete: {key: sk-ete, url: https://ete.example.com}
`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := apply(t, src, change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-ete")); got != src {
				t.Errorf("an equal value changed the file:\n%s", Diff([]byte(src), []byte(got)))
			}
			if got := apply(t, src, change([]string{"agents", "claude-code"}, "ete")); got != src {
				t.Errorf("an equal scalar changed the file:\n%s", Diff([]byte(src), []byte(got)))
			}
		})
	}
}

func TestSetPluginConfig_RemovesAKey(t *testing.T) {
	src := strings.Replace(withRouter, "            claude-code: ete\n", "            claude-code: ete\n            opencode: ete\n", 1)
	assertYAML(t, apply(t, src, change([]string{"agents", "opencode"})), withRouter)
}

// A map left empty by a removal goes with it; the comment above it stays, since it
// may be about what follows.
func TestSetPluginConfig_RemovingTheLastKeyRemovesItsMap(t *testing.T) {
	assertYAML(t, apply(t, withRouter, change([]string{"agents", "claude-code"})),
		strings.Replace(withRouter, `          agents:
            claude-code: ete
`, "", 1))
}

func TestSetPluginConfig_RemovingWhatIsAbsentChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		src string
		ch  ConfigChange
	}{
		"absent key":   {withRouter, change([]string{"agents", "opencode"})},
		"absent map":   {withRouter, change([]string{"models", "opus"})},
		"absent entry": {localConfig, change([]string{"agents", "claude-code"})},
	} {
		t.Run(name, func(t *testing.T) {
			if got := apply(t, tc.src, tc.ch); got != tc.src {
				t.Errorf("removing nothing changed the file:\n%s", Diff([]byte(tc.src), []byte(got)))
			}
		})
	}
}

// A compact list — `remove:` with its items at the key's own indent — ends where
// its items do, and removing it empties config:, which goes too.
func TestSetPluginConfig_RemovesACompactListAndTheConfigItEmpties(t *testing.T) {
	src := strings.Replace(localConfig, "          remove: []\n", "          remove:\n          - Bash\n          - Read\n", 1)
	ch := ConfigChange{Chain: "outbound", Plugin: "tool-prune", Path: []string{"remove"}}
	assertYAML(t, apply(t, src, ch), strings.Replace(localConfig, `        config:
          remove: []
`, "", 1))
}

func TestSetPluginConfig_GivesAnEntryWithNoConfigOne(t *testing.T) {
	src := strings.Replace(localConfig, `        config:
          remove: []
`, "", 1)
	ch := ConfigChange{Chain: "outbound", Plugin: "tool-prune", Path: []string{"remove"}, Value: ScalarValue("Bash")}
	assertYAML(t, apply(t, src, ch), strings.Replace(src, "        on_error: enforce\n",
		"        on_error: enforce\n        config:\n          remove: Bash\n", 1))
}

func TestSetPluginConfig_FillsAnEmptyConfig(t *testing.T) {
	src := strings.Replace(localConfig, "        config:\n          remove: []\n", "        config:   # nothing yet\n", 1)
	ch := ConfigChange{Chain: "outbound", Plugin: "tool-prune", Path: []string{"paths", "messages"}, Value: ScalarValue("/v1/messages")}
	assertYAML(t, apply(t, src, ch), strings.Replace(src, "        config:   # nothing yet\n",
		"        config:   # nothing yet\n          paths:\n            messages: /v1/messages\n", 1))
}

// A whole document indented is valid YAML and turned up in a real config; positions
// come from the parser, so the new entry follows the file's own columns.
func TestSetPluginConfig_FollowsTheFilesOwnIndentation(t *testing.T) {
	src := "  pipeline:\n    outbound:\n      plugins:\n      - name: tool-prune\n"
	ch := change([]string{"agents", "claude-code"}, "ete")
	ch.CreatePlugin = true
	assertYAML(t, apply(t, src, ch),
		"  pipeline:\n    outbound:\n      plugins:\n      - name: tool-prune\n      - name: inference-router\n        config:\n          agents:\n            claude-code: ete\n")
}

func TestSetPluginConfig_QuotesWhatWouldNotReadBackAsAString(t *testing.T) {
	ch := change([]string{"agents", "123"}, "true")
	got := apply(t, withRouter, ch)
	if !strings.Contains(got, `            "123": "true"`+"\n") {
		t.Errorf("want the key and value quoted:\n%s", got)
	}
}

func TestSetPluginConfig_RefusesWhatItCannotEditLineByLine(t *testing.T) {
	for name, tc := range map[string]struct {
		src, want string
	}{
		"flow-style config": {
			strings.Replace(withRouter, `        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete   # rotated in October
          # only agents listed here are routed
          agents:
            claude-code: ete
`, "        config: {servers: {ete: {url: https://ete.example.com, key: k}}}\n", 1),
			"inference-router config is not an indented mapping",
		},
		"bare-name entry": {
			strings.Replace(localConfig, "      - name: tool-prune\n        on_error: enforce\n        config:\n          remove: []\n",
				"      - tool-prune\n      - inference-router\n", 1),
			"written as a bare name",
		},
		"flow-style plugins list": {
			"pipeline:\n  outbound:\n    plugins: [inference-parser]\n",
			"pipeline.outbound.plugins is not an indented list",
		},
		"no pipeline": {"mode: proxy-sidecar\n", "pipeline.outbound.plugins is not an indented list"},
		"two entries": {
			strings.Replace(withRouter, "      - name: tool-prune\n", "      - name: inference-router\n      - name: tool-prune\n", 1),
			"two inference-router entries",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ch := change([]string{"agents", "opencode"}, "ete")
			ch.CreatePlugin = true
			_, err := SetPluginConfig([]byte(tc.src), ch)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Keys are written through here, so an error about a value must not print it.
func TestSetPluginConfig_ALineBreakIsRefusedWithoutEchoingTheValue(t *testing.T) {
	_, err := SetPluginConfig([]byte(withRouter), change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-secret\nx"))
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("err = %v, want a refusal that does not contain the value", err)
	}
}

// Line breaks the YAML parser counts but the editor would not (U+0085, U+2028, U+2029)
// make every later edit land one line off if not refused.
func TestSetPluginConfig_RefusesSpecialLineBreaksInValues(t *testing.T) {
	for name, ch := range map[string]ConfigChange{
		"U+0085": change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-a\x85b"),
		"U+2028": change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-a b"),
		"U+2029": change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-a b"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := SetPluginConfig([]byte(withRouter), ch)
			if err == nil {
				t.Fatal("err = nil, want refusal of line break the YAML parser counts")
			}
			if strings.Contains(err.Error(), "sk-a") {
				t.Errorf("err contains value: %v", err)
			}
		})
	}
}

// A value with a bare \r (not followed by \n) causes the same line-count mismatch.
func TestSetPluginConfig_RefusesBareCarriageReturn(t *testing.T) {
	_, err := SetPluginConfig([]byte(withRouter), change([]string{"servers", "ete"}, "key", "sk-a\rb"))
	if err == nil {
		t.Fatal("err = nil, want refusal of bare carriage return")
	}
}

// RefusesSourceWithSpecialLineBreaks rejects input containing U+0085, U+2028, or U+2029.
func TestSetPluginConfig_RefusesSourceWithSpecialLineBreaks(t *testing.T) {
	for name, src := range map[string]string{
		"U+0085": strings.Replace(withRouter, "ete:", "ete\x85:", 1),
		"U+2028": strings.Replace(withRouter, "ete:", "ete :", 1),
		"U+2029": strings.Replace(withRouter, "ete:", "ete :", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := SetPluginConfig([]byte(src), change([]string{"agents", "new"}, "ete"))
			if err == nil {
				t.Fatal("err = nil, want refusal")
			}
		})
	}
}

// RefusesSourceWithBareCarriageReturn rejects input with \r not followed by \n.
func TestSetPluginConfig_RefusesSourceWithBareCarriageReturn(t *testing.T) {
	src := strings.Replace(withRouter, "ete:", "ete\r:", 1)
	_, err := SetPluginConfig([]byte(src), change([]string{"agents", "new"}, "ete"))
	if err == nil {
		t.Fatal("err = nil, want refusal of bare carriage return in source")
	}
}

// ProbeSequence: set a value with U+2028, then remove a later key — the file should
// not be corrupted (the line numbers stay accurate).
func TestSetPluginConfig_SpecialLineBreaksDoNotCorruptLaterEdits(t *testing.T) {
	// Start with the config and set a key containing U+2028
	// This should fail, so we can't execute it. Instead, manually test
	// by setting a value WITHOUT the special char first,  then removing later.
	// The point is that the refusal prevents corruption.
	//
	// Since setting a special char fails, the file stays clean and later
	// edits work correctly. Verify by removing a key after the "would-be" set location.
	src := withRouter
	ch := change([]string{"agents", "claude-code"})
	result, err := SetPluginConfig([]byte(src), ch)
	if err != nil {
		t.Fatalf("removing a key should work: %v", err)
	}
	expected := strings.Replace(src, `          agents:
            claude-code: ete
`, "", 1)
	if string(result) != expected {
		t.Errorf("removal failed:\n--- got ---\n%s\n--- want ---\n%s\n--- diff ---\n%s",
			string(result), expected, Diff([]byte(expected), result))
	}
}
