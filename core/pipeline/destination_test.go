package pipeline

import (
	"context"
	"strings"
	"testing"
)

// capPlugin declares a capability set and does nothing, for build-time rules.
type capPlugin struct {
	name string
	caps PluginCapabilities
}

func (p *capPlugin) Name() string                     { return p.name }
func (p *capPlugin) Capabilities() PluginCapabilities { return p.caps }
func (p *capPlugin) OnRequest(context.Context, *Context) Action {
	return Action{Type: Continue}
}
func (p *capPlugin) OnResponse(context.Context, *Context) Action {
	return Action{Type: Continue}
}

func destWriter(name string) *capPlugin {
	return &capPlugin{name: name, caps: PluginCapabilities{WritesDestination: true, Description: "test"}}
}

func TestNew_RejectsTwoDestinationWriters(t *testing.T) {
	_, err := New([]Plugin{destWriter("first"), destWriter("second")})
	if err == nil {
		t.Fatal("New accepted two WritesDestination plugins; the request can go to only one place")
	}
	for _, want := range []string{"WritesDestination", `"first"`, `"second"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestNew_AcceptsOneDestinationWriter(t *testing.T) {
	bystander := &capPlugin{name: "bystander", caps: PluginCapabilities{Description: "test"}}
	if _, err := New([]Plugin{destWriter("router"), bystander}); err != nil {
		t.Fatalf("New rejected a single WritesDestination plugin: %v", err)
	}
}

// A plugin that rewrites a body AND chooses a destination is still one destination
// writer: the mutator branch of validateCapabilities ends in a continue, and the
// destination check must not sit behind it.
func TestNew_CountsADestinationWriterThatAlsoMutates(t *testing.T) {
	both := &capPlugin{name: "both", caps: PluginCapabilities{WritesDestination: true, WritesRequestBody: true, Description: "test"}}
	if _, err := New([]Plugin{both, destWriter("second")}); err == nil {
		t.Fatal("New accepted a second WritesDestination plugin after one that also writes the body")
	}
}
