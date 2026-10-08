package reverseproxy

import "github.com/rossoctl/cortex/core/pipeline"

// Support is what the reverse proxy honors, for plugins.Deps. It forwards inbound
// requests to the local application, so there is nowhere else to send one: it
// honors no redirect, which is also what keeps a WritesDestination plugin off an
// inbound chain.
func Support() pipeline.ListenerSupport {
	return pipeline.ListenerSupport{Listener: "reverse proxy"}
}
