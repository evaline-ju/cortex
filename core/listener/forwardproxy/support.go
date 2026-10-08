package forwardproxy

import "github.com/rossoctl/cortex/core/pipeline"

// Support is what the forward proxy honors, for plugins.Deps. It re-originates every
// request it runs the pipeline on, plain or TLS-bridged, so it honors a redirect.
//
// Except with mtls on. The mTLS dialer wraps every upstream connection in its own
// TLS, so an https redirect would nest TLS inside TLS — untested, and mTLS is an
// in-cluster feature no redirecting plugin needs today.
func Support(mtls bool) pipeline.ListenerSupport {
	if mtls {
		return pipeline.ListenerSupport{Listener: "forward proxy with mtls"}
	}
	return pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}
}
