package pipeline

// ListenerSupport says which listener-dependent capabilities the listener a pipeline
// is built for can honor.
//
// Some capabilities are promises only a listener can keep. A plugin may ask for a
// redirect, but only a listener that builds the upstream request itself can send it
// somewhere else; anywhere else the plugin would run, record its decision, and change
// nothing. plugins.BuildWithDeps refuses such a plugin at build time, on startup and on
// reload, so a configuration that cannot work fails before traffic arrives.
//
// Each listener package says what it honors (forwardproxy.Support, reverseproxy.Support,
// extproc.Support) and each binary passes that into its builds. The zero value honors
// none of these capabilities, so a listener that has not considered one refuses the
// plugins that need it rather than silently ignoring them. Adding a capability of this
// kind means adding a field here and a case to Unsupported.
type ListenerSupport struct {
	// Listener names the listener in the build error, e.g. "forward proxy".
	Listener string

	// Destination: the listener re-originates requests from pctx after the pipeline
	// and honors pctx.Redirect. Required by WritesDestination plugins.
	Destination bool
}

// Unsupported returns the name of the first capability caps declares that s cannot
// honor, or "" when it can honor every one.
func (s ListenerSupport) Unsupported(caps PluginCapabilities) string {
	if caps.WritesDestination && !s.Destination {
		return "WritesDestination"
	}
	return ""
}

// Name is Listener, or a description of the listener for a build that named none.
func (s ListenerSupport) Name() string {
	if s.Listener == "" {
		return "listener this pipeline is built for"
	}
	return s.Listener
}
