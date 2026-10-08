package extproc

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestSupport_RefusesARedirect(t *testing.T) {
	if got := Support().Unsupported(pipeline.PluginCapabilities{WritesDestination: true}); got != "WritesDestination" {
		t.Error("ext_proc admits a redirect; it cannot change where Envoy sends a request")
	}
}
