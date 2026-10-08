package forwardproxy

import "github.com/rossoctl/cortex/core/pipeline"

// Support is what the forward proxy honors, for plugins.Deps. It re-originates every
// request it runs the pipeline on, plain or TLS-bridged, so it honors a redirect.
//
// Except whenever an mtls: block is configured, which is what mtls reports. Under
// mode: strict the forward proxy dials every upstream through its mTLS dialer, which
// wraps the connection in its own TLS, so an https redirect would nest TLS inside TLS
// — untested, and mTLS is an in-cluster feature no redirecting plugin needs today.
// Under permissive there is no such dialer, since outbound stays plaintext, so the
// refusal is broader than strictly needed; it fails closed. The reloader refuses any
// mtls change, which keeps this check matched to the mtls config the listeners were
// built from.
func Support(mtls bool) pipeline.ListenerSupport {
	if mtls {
		return pipeline.ListenerSupport{Listener: "forward proxy with mtls"}
	}
	return pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}
}
