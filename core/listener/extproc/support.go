package extproc

import "github.com/rossoctl/cortex/core/pipeline"

// Support is what the ext_proc listener honors, for plugins.Deps. It answers Envoy
// with header and body mutations and never holds the upstream connection; it skips
// every ":"-prefixed header, :authority included, so it cannot change where Envoy
// sends a request. Honoring a redirect would need Envoy-side routing that the Helm
// chart owns.
func Support() pipeline.ListenerSupport {
	return pipeline.ListenerSupport{Listener: "ext_proc listener"}
}
