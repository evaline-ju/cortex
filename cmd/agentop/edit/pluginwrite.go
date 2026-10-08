package edit

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/rossoctl/cortex/core/config"
)

// ConfigWrite is one write of plugin config changes to a local Cortex config file.
type ConfigWrite struct {
	// Path is the config file the proxy was started with and watches.
	Path string
	// StatsURL is the base URL of that proxy's stats server, where /reload/status
	// says whether it took the write. Empty when no proxy answers there; the file is
	// then written and nothing is polled.
	StatsURL string
	// Changes are applied in order, each to the result of the one before.
	Changes []ConfigChange
	// Verify, when set, checks the result after config.Load has accepted it. This is
	// where a caller puts its plugin's own rules, which config.Load does not know:
	// a result Verify refuses is never written.
	Verify func(*config.Config) error
}

// WriteOutcome is how a write ended once the file was, or was not, written.
type WriteOutcome int

const (
	// WriteUnchanged: the file already held every change. Nothing was written and
	// nothing polled: the reloader ignores a byte-identical file, so its
	// last_success would never move and the poll would wait out its deadline.
	WriteUnchanged WriteOutcome = iota
	// WriteReloaded: written, and the proxy reloaded it.
	WriteReloaded
	// WriteReloadFailed: written, and the proxy refused it. The file was restored
	// to what it had; ReloadError says why. RolledBack reports whether the restore
	// succeeded.
	WriteReloadFailed
	// WriteReloadTimedOut: written, and the proxy reported neither a reload nor a
	// failure within LocalPollDeadline. The file is left as written; the reload may
	// be slow or the state uncertain.
	WriteReloadTimedOut
	// WriteNotRunning: StatsURL was empty (no proxy to poll). The file was written.
	WriteNotRunning
)

// WriteResult is what WritePluginConfig reports.
type WriteResult struct {
	Outcome     WriteOutcome
	ReloadError string // set for WriteReloadFailed
	RolledBack  bool   // set for WriteReloadFailed when the restore succeeded
}

// WritePluginConfig applies w.Changes to the file at w.Path and waits for the proxy
// to reload it.
//
// It returns an error, having written nothing, when a change cannot be made, the
// result does not load or fails Verify, or the file changed while it worked. Once
// the file is written the error is nil and WriteResult says what the proxy did.
func WritePluginConfig(ctx context.Context, w ConfigWrite) (WriteResult, error) {
	orig, err := os.ReadFile(w.Path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %s: %w", w.Path, err)
	}
	updated := orig
	for _, ch := range w.Changes {
		if updated, err = SetPluginConfig(updated, ch); err != nil {
			return WriteResult{}, fmt.Errorf("%s: %w", w.Path, err)
		}
	}
	if bytes.Equal(updated, orig) {
		return WriteResult{Outcome: WriteUnchanged}, nil
	}
	if err := checkLoads(updated, w.Verify); err != nil {
		return WriteResult{}, fmt.Errorf("not writing %s: %w", w.Path, err)
	}

	store := FileStore{Path: w.Path}
	if err := store.CheckUnchanged(ctx, &FetchedPipeline{Original: orig}); err != nil {
		return WriteResult{}, err
	}
	applyTime, err := store.Apply(ctx, updated)
	if err != nil {
		return WriteResult{}, err
	}
	if w.StatsURL == "" {
		return WriteResult{Outcome: WriteNotRunning}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, LocalPollDeadline)
	defer cancel()
	switch res := PollUntilReloaded(pctx, w.StatsURL, applyTime, store.Describe().UnreachableHint); res.Status {
	case PollSuccess:
		return WriteResult{Outcome: WriteReloaded}, nil
	case PollFailure:
		// Restore the original bytes to avoid crash-looping on next start
		_, rollbackErr := store.Apply(context.Background(), orig)
		rolledBack := rollbackErr == nil
		if rollbackErr != nil {
			// File still holds the refused config and must be fixed by hand
			return WriteResult{}, fmt.Errorf("restore after refused reload failed; config file must be fixed by hand: %w", rollbackErr)
		}
		return WriteResult{Outcome: WriteReloadFailed, ReloadError: res.LastError, RolledBack: rolledBack}, nil
	default:
		return WriteResult{Outcome: WriteReloadTimedOut}, nil
	}
}

// checkLoads reports whether b loads as a Cortex config and passes verify, as the
// proxy would load it. Checked from a private temp file, never beside the config:
// the proxy watches that directory, and b can hold API keys.
func checkLoads(b []byte, verify func(*config.Config) error) error {
	f, err := os.CreateTemp("", "agentop-config-check-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cfg, err := config.Load(f.Name())
	if err != nil {
		return fmt.Errorf("the result would not load: %w", err)
	}
	if verify != nil {
		return verify(cfg)
	}
	return nil
}
