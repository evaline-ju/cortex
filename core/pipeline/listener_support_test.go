package pipeline

import "testing"

func TestListenerSupport_ZeroValueRefusesADestinationWriter(t *testing.T) {
	got := ListenerSupport{}.Unsupported(PluginCapabilities{WritesDestination: true})
	if got != "WritesDestination" {
		t.Errorf("Unsupported = %q, want WritesDestination: a listener that has not declared support must refuse, not ignore", got)
	}
}

func TestListenerSupport_DestinationHonored(t *testing.T) {
	got := ListenerSupport{Destination: true}.Unsupported(PluginCapabilities{WritesDestination: true})
	if got != "" {
		t.Errorf("Unsupported = %q with Destination: true, want none", got)
	}
}

func TestListenerSupport_OtherCapabilitiesNeedNoSupport(t *testing.T) {
	caps := PluginCapabilities{ReadsBody: true, WritesRequestBody: true, WritesResponseBody: true}
	if got := (ListenerSupport{}).Unsupported(caps); got != "" {
		t.Errorf("Unsupported = %q for body capabilities, which every listener handles", got)
	}
}

func TestListenerSupport_NameFallsBackForAnUnnamedListener(t *testing.T) {
	if got := (ListenerSupport{Listener: "ext_proc listener"}).Name(); got != "ext_proc listener" {
		t.Errorf("Name() = %q", got)
	}
	if got := (ListenerSupport{}).Name(); got == "" {
		t.Error("Name() is empty for an unnamed listener; the build error would read badly")
	}
}
