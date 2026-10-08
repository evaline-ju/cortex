package reverseproxy

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestSupport_RefusesARedirect(t *testing.T) {
	if got := Support().Unsupported(pipeline.PluginCapabilities{WritesDestination: true}); got != "WritesDestination" {
		t.Error("the reverse proxy admits a redirect; it forwards to the local app and has nowhere else to send one")
	}
}
