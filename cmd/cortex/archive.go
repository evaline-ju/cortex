package main

import (
	"log/slog"
	"path/filepath"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// sessionArchiveDirName is the archive's directory under ~/.cortex, beside the cost ledger's.
const sessionArchiveDirName = "sessions"

// sessionArchiveDir is where the session archive lives: always ~/.cortex/sessions. There is no
// config for it, because in this release the archive is a laptop feature — see
// config.SessionArchiveConfig.
func sessionArchiveDir() (string, error) {
	dir, err := defaultCortexDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionArchiveDirName), nil
}

// sessionArchiveRuns decides whether the session archive runs, and says why.
//
// ON BY DEFAULT ON A LOCAL INSTALL, and only there. A laptop is where a restart costs a user their
// history and where the user can read it back (agentop's H) and erase it (X, or DELETE
// /v1/sessions): both exist, which is what the default waited for. Elsewhere it is off even when
// asked for — raw prompts on a cluster's volume need a decision of their own, and a mounted path
// is not one. false always wins.
func sessionArchiveRuns(cfg *config.Config, configPath string) (bool, string) {
	a := cfg.Session.Archive
	explicit := a != nil && a.Enabled != nil
	if explicit && !*a.Enabled {
		return false, "session.archive.enabled is false"
	}
	if !startedFromLocalInstall(configPath) {
		if explicit {
			return false, "the session archive is a laptop feature in this release and runs only from a config inside ~/.cortex"
		}
		return false, "not a local install"
	}
	if !explicit {
		return true, "on by default for a local install; set session.archive.enabled: false to turn it off"
	}
	return true, "session.archive.enabled is true on a local install"
}

// closeArchiveOnFatal finishes the archive's open segments before a fatal exit, for the reason
// closeLedgerOnFatal flushes the ledger: log.Fatalf ends in os.Exit, which runs no defers. Nil
// whenever no archive is open.
var closeArchiveOnFatal func()

// openSessionArchive opens the archive and registers it on the store when sessionArchiveRuns
// says it should, and returns nil otherwise. A failure to open is a warning, not a fatal error:
// the archive is observability, and refusing to start the proxy over it would trade a nicety for
// an outage.
func openSessionArchive(cfg *config.Config, configPath string, sessions *session.Store) *archive.Archive {
	run, why := sessionArchiveRuns(cfg, configPath)
	if !run {
		if a := cfg.Session.Archive; a != nil && a.Enabled != nil && *a.Enabled {
			slog.Warn("session archive will NOT run despite session.archive.enabled: true", "reason", why)
		} else {
			slog.Info("session archive disabled — sessions do not survive a restart or eviction", "reason", why)
		}
		return nil
	}
	dir, err := sessionArchiveDir()
	if err != nil {
		slog.Warn("session archive disabled — cannot determine where to write it", "error", err)
		return nil
	}
	var ac config.SessionArchiveConfig
	if cfg.Session.Archive != nil {
		ac = *cfg.Session.Archive
	}
	arch, err := archive.Open(dir, archive.WithRetentionDays(ac.RetentionDays), archive.WithMaxBytes(ac.MaxBytes))
	if err != nil {
		slog.Warn("session archive disabled — could not open it", "dir", dir, "error", err,
			"effect", "sessions do not survive a restart or eviction")
		return nil
	}
	sessions.AddRecorder(arch)
	closeArchiveOnFatal = func() {
		if cerr := arch.Close(); cerr != nil {
			slog.Warn("session archive: final flush failed during a fatal startup error", "error", cerr)
		}
	}
	st := arch.Stats()
	slog.Info("session archive enabled — sessions survive a restart and the store's eviction",
		"dir", dir, "retentionDays", st.RetentionDays, "maxBytes", st.MaxBytes, "bytes", st.Bytes,
		"holds", "raw prompts, completions and tool results; readable by anyone who can read this directory",
		"default", why)
	return arch
}
