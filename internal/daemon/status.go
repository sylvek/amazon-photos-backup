package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"amazon-photos-backup/internal/backup"
)

const (
	StateStarting     = "starting"
	StateSyncing      = "syncing"
	StateOK           = "ok"            // last run saved everything
	StateDegraded     = "degraded"      // last run finished, but some items failed
	StateError        = "error"         // last run could not complete
	StateAuthRequired = "auth_required" // Amazon rejected the session: login needed
)

// Status is what the daemon knows about itself; it is written to
// status.json for the status and healthcheck commands.
type Status struct {
	State        string        `json:"state"`
	Running      bool          `json:"running"` // a backup run is in progress right now
	Started      time.Time     `json:"started"`
	Updated      time.Time     `json:"updated"`
	LastRunStart time.Time     `json:"last_run_start"`
	LastSuccess  time.Time     `json:"last_success"`
	LastError    string        `json:"last_error,omitempty"`
	LastResult   backup.Result `json:"last_result"`
	// Notified is the failure state a notification was last sent for, so
	// that one outage produces one message, even across restarts.
	Notified string `json:"notified,omitempty"`
}

func LoadStatus(cfg Config) (Status, error) {
	var st Status
	data, err := os.ReadFile(cfg.statusPath())
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func saveStatus(cfg Config, st Status) error {
	if st.Updated.IsZero() {
		st.Updated = time.Now().UTC()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cfg.StateDir, ".status-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(cfg.StateDir, "status.json"))
}

// Summary is the state as a person should read it. After a failure the
// state stays "error" (or "auth_required") until a run succeeds, so that
// the healthcheck does not turn green during every retry; the summary says
// when such a retry is in progress.
func (s Status) Summary() string {
	switch {
	case s.State == StateSyncing && s.LastSuccess.IsZero():
		return "syncing (first backup)"
	case s.Running && (s.State == StateError || s.State == StateAuthRequired):
		return s.State + " (previous run failed; a new run is running)"
	}
	return s.State
}

// Healthcheck returns an error when the backup needs attention: the session
// must be renewed, or no run has succeeded for three intervals. A long first
// synchronisation is healthy.
func Healthcheck(cfg Config, now time.Time) error {
	st, err := LoadStatus(cfg)
	if err != nil {
		return fmt.Errorf("no status available: %w", err)
	}
	switch st.State {
	case StateAuthRequired:
		return errors.New("Amazon session expired: run the login command")
	case StateError:
		since := st.LastSuccess
		if since.IsZero() {
			since = st.Started
		}
		if limit := 3 * cfg.Interval; now.Sub(since) > limit {
			return fmt.Errorf("no successful backup for %s: %s", now.Sub(since).Round(time.Minute), st.LastError)
		}
	}
	return nil
}
