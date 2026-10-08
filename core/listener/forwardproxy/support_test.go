package forwardproxy

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

var destinationWriter = pipeline.PluginCapabilities{WritesDestination: true}

func TestSupport_HonorsARedirectWithoutMTLS(t *testing.T) {
	if got := Support(false).Unsupported(destinationWriter); got != "" {
		t.Errorf("Support(false) refuses %s; the forward proxy re-originates every request it runs the pipeline on", got)
	}
}

func TestSupport_RefusesARedirectWithMTLS(t *testing.T) {
	if got := Support(true).Unsupported(destinationWriter); got != "WritesDestination" {
		t.Errorf("Support(true) admits a redirect; through the mTLS dialer an https target would nest TLS in TLS")
	}
}
