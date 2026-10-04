package config

import "fmt"

// SessionArchiveConfig is `session.archive`: the session archive, which persists every session
// to disk so it survives a restart and the store's own eviction (core/session/archive).
//
// A LAPTOP FEATURE IN THIS RELEASE. The cortex binary runs it only when started from a config
// inside ~/.cortex, and the directory is always ~/.cortex/sessions; there is deliberately no
// `dir`. Raw prompts on a cluster's volume need their own decision, so a cluster cannot turn
// this on by mounting a path.
//
// Nested under `session` rather than top-level like cost_ledger, because it records through
// the session store: the dependency is structural. And because reloader.validateReloadable
// refuses any live change under `session`, changing this needs a restart with no code of its
// own.
type SessionArchiveConfig struct {
	// Enabled is a pointer so unset can follow the binary's default: on for a local install,
	// off everywhere else. false always wins.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// RetentionDays is how long a segment is kept after its last write. 0 means the default,
	// 30.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
	// MaxBytes bounds the archive's size; the oldest segments go first past it. 0 means the
	// default, 2 GiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
}

// ArchiveEnabled reports whether the archive should run, given the caller's default for an
// unset block. Safe on a nil receiver.
func (c *SessionArchiveConfig) ArchiveEnabled(defaultOn bool) bool {
	if c == nil || c.Enabled == nil {
		return defaultOn
	}
	return *c.Enabled
}

// Validate is called from the loader when session.archive is present.
func (c *SessionArchiveConfig) Validate() error {
	if c.RetentionDays < 0 {
		return fmt.Errorf("session.archive.retention_days must not be negative, got %d", c.RetentionDays)
	}
	// The cost ledger's ceiling, for its reason: retention counted back with time arithmetic
	// that normalises can wrap a cutoff into the future and delete everything.
	if c.RetentionDays > maxCostLedgerRetentionDays {
		return fmt.Errorf("session.archive.retention_days must be at most %d (ten years), got %d",
			maxCostLedgerRetentionDays, c.RetentionDays)
	}
	if c.MaxBytes < 0 {
		return fmt.Errorf("session.archive.max_bytes must not be negative, got %d", c.MaxBytes)
	}
	return nil
}
