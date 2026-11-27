package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/backup"
)

// Login reads cookies copied from a browser, checks them against Amazon and
// saves them. Nothing is written unless Amazon accepts them, so a mistyped
// paste cannot replace a working session.
func Login(cfg Config, r io.Reader, w io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	cookies, err := amazon.ParseCookies(string(data))
	if err != nil {
		return err
	}
	if missing := amazon.MissingCookies(cookies); len(missing) > 0 {
		return fmt.Errorf("required cookies are missing: %s. Copy the full Cookie header of a request "+
			"from the browser's Network tab (document.cookie does not include the HttpOnly ones)", strings.Join(missing, ", "))
	}
	base, err := cfg.baseURL(cookies)
	if err != nil {
		return err
	}
	store := amazon.NewStore(cfg.cookiePath())
	store.Replace(cookies)
	client, err := amazon.NewClient(amazon.Options{BaseURL: base, Store: store, UserAgent: cfg.UserAgent, Retries: 2})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := client.Root(ctx); err != nil {
		if errors.Is(err, amazon.ErrAuth) {
			return fmt.Errorf("Amazon rejected these cookies; nothing was saved: %w", err)
		}
		return fmt.Errorf("could not check the cookies with Amazon; nothing was saved: %w", err)
	}
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "Session accepted by %s. %d cookies saved to %s.\n", base, store.Len(), cfg.cookiePath())
	return nil
}

// ErrBusy means another process is already backing up into the same state
// directory.
var ErrBusy = errors.New("another backup is already running on this data directory")

// lockState takes an exclusive, non-blocking lock for the duration of one
// cycle, so that the daemon and a manual `once` never write the same files.
func lockState(cfg Config) (release func(), err error) {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(cfg.StateDir+"/lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Daemon runs backup cycles forever.
type Daemon struct {
	Config   Config
	Log      *slog.Logger
	AuthPoll time.Duration // how often to look for new cookies while logged out; default 30s

	store  *amazon.Store
	status Status
}

// Once performs a single backup cycle and records its outcome.
func Once(ctx context.Context, cfg Config, log *slog.Logger) (backup.Result, error) {
	d := &Daemon{Config: cfg, Log: log}
	d.init()
	return d.runCycle(ctx)
}

// Run loops until ctx is cancelled: back up, sleep, repeat. When the session
// expires it waits for new cookies and resumes by itself.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	return (&Daemon{Config: cfg, Log: log}).Run(ctx)
}

func (d *Daemon) init() {
	if d.AuthPoll <= 0 {
		d.AuthPoll = 30 * time.Second
	}
	d.store = amazon.NewStore(d.Config.cookiePath())
	prev, _ := LoadStatus(d.Config)
	d.status = Status{
		State:       StateStarting,
		Started:     time.Now().UTC(),
		LastSuccess: prev.LastSuccess,
		LastResult:  prev.LastResult,
		LastError:   prev.LastError,
		Notified:    prev.Notified,
	}
	// A failure that was already reported stays reported across a restart.
	if prev.State == StateAuthRequired || prev.State == StateError {
		d.status.State = prev.State
	}
}

func (d *Daemon) saveStatus() {
	d.status.Updated = time.Now().UTC()
	if err := saveStatus(d.Config, d.status); err != nil {
		d.Log.Error("cannot write status file", "err", err)
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	d.init()
	d.saveStatus()
	d.Log.Info("backup daemon started", "backup_dir", d.Config.BackupDir, "interval", d.Config.Interval.String())
	for {
		_, err := d.runCycle(ctx)
		if ctx.Err() != nil {
			d.Log.Info("shutting down")
			return nil
		}
		switch {
		case errors.Is(err, ErrBusy):
			d.Log.Warn("another backup is running on this data directory; waiting")
			sleep(ctx, min(d.Config.Interval, time.Minute))
		case errors.Is(err, amazon.ErrAuth):
			d.waitForCookies(ctx)
		case err != nil:
			sleep(ctx, min(d.Config.Interval, 15*time.Minute))
		default:
			sleep(ctx, d.Config.Interval)
		}
		if ctx.Err() != nil {
			d.Log.Info("shutting down")
			return nil
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// waitForCookies returns as soon as the login command has written new
// cookies. It also returns after one interval, so that a refusal that was
// only temporary on Amazon's side heals without anyone logging in again.
func (d *Daemon) waitForCookies(ctx context.Context) {
	deadline := time.After(d.Config.Interval)
	tick := time.NewTicker(d.AuthPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-tick.C:
			changed, err := d.store.ReloadIfChanged()
			if err != nil {
				d.Log.Warn("cannot read cookie file", "err", err)
				continue
			}
			if changed {
				d.Log.Info("new cookies detected, resuming")
				return
			}
		}
	}
}

// runCycle runs one cycle and records the outcome in the status file.
func (d *Daemon) runCycle(ctx context.Context) (backup.Result, error) {
	release, err := lockState(d.Config)
	if err != nil {
		// Not ours to report on: the status file belongs to whoever holds
		// the lock.
		return backup.Result{}, err
	}
	defer release()

	// While a failure is outstanding the state keeps saying so; "syncing"
	// would hide it from the healthcheck during every retry.
	if d.status.State != StateAuthRequired && d.status.State != StateError {
		d.status.State = StateSyncing
	}
	d.status.LastRunStart = time.Now().UTC()
	d.status.Running = true
	d.saveStatus()

	res, err := d.cycle(ctx)
	d.status.Running = false
	if ctx.Err() != nil {
		d.saveStatus()
		return res, ctx.Err()
	}
	switch {
	case errors.Is(err, amazon.ErrAuth):
		d.Log.Error("Amazon session is not valid: run the login command with fresh cookies", "err", err)
		d.fail(StateAuthRequired, err, res,
			"Amazon Photos backup: login required",
			"The Amazon session is no longer accepted. The backup is paused until new cookies are provided with the login command.")
	case err != nil:
		d.Log.Error("backup cycle failed", "err", err)
		d.fail(StateError, err, res,
			"Amazon Photos backup: error",
			"The backup could not complete: "+truncate(err.Error(), 300))
	default:
		d.status.State = StateOK
		d.status.LastError = ""
		if res.Failed > 0 || res.Unreachable > 0 {
			d.status.State = StateDegraded
			d.status.LastError = fmt.Sprintf("%d items failed to download, %d could not be listed", res.Failed, res.Unreachable)
		}
		if res.Mismatched > 0 {
			d.Log.Warn("some files were saved although their content differs from Amazon's checksum", "count", res.Mismatched)
		}
		d.status.LastSuccess = time.Now().UTC()
		d.status.LastResult = res
		d.status.Notified = ""
		d.saveStatus()
		d.Log.Info("backup cycle finished", "state", d.status.State, "remote", res.Remote, "downloaded", res.Downloaded,
			"already_saved", res.Skipped, "adopted", res.Adopted, "failed", res.Failed, "unreachable", res.Unreachable, "bytes", res.Bytes)
	}
	return res, err
}

func (d *Daemon) fail(state string, err error, res backup.Result, title, message string) {
	d.status.State = state
	d.status.LastError = truncate(err.Error(), 500)
	if res.Downloaded > 0 {
		d.status.LastResult = res
	}
	// Remembered only once delivered: an alert lost to a webhook hiccup is
	// sent again at the next attempt instead of never.
	if d.status.Notified != state && d.notify(title, message) {
		d.status.Notified = state
	}
	d.saveStatus()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// cycle is one pass: restore the session from disk, check it, back up, and
// write back whatever cookies Amazon refreshed.
func (d *Daemon) cycle(ctx context.Context) (backup.Result, error) {
	var res backup.Result
	// Always start from the file: it holds the latest login, and reloading
	// discards cookies a refusal may have cleared in memory.
	if err := d.store.Load(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("no cookies saved yet: %w", amazon.ErrAuth)
		}
		return res, fmt.Errorf("reading cookies: %w", err)
	}
	cookies := d.store.Snapshot()
	if missing := amazon.MissingCookies(cookies); len(missing) > 0 {
		return res, fmt.Errorf("saved cookies are incomplete (%s): %w", strings.Join(missing, ", "), amazon.ErrAuth)
	}
	base, err := d.Config.baseURL(cookies)
	if err != nil {
		return res, err
	}
	client, err := amazon.NewClient(amazon.Options{BaseURL: base, Store: d.store, UserAgent: d.Config.UserAgent})
	if err != nil {
		return res, err
	}

	if err := client.KeepAlive(ctx); err != nil {
		if errors.Is(err, amazon.ErrAuth) || ctx.Err() != nil {
			return res, err
		}
		d.Log.Warn("session keep-alive failed, continuing", "err", err)
	}
	if _, err := client.Root(ctx); err != nil {
		return res, err
	}

	idx, err := backup.LoadIndex(d.Config.indexPath())
	if err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", d.Config.indexPath(), time.Now().Unix())
		d.Log.Warn("index unreadable; setting it aside and rebuilding from the files on disk", "err", err, "moved_to", aside)
		if err := os.Rename(d.Config.indexPath(), aside); err != nil {
			return res, err
		}
		if idx, err = backup.LoadIndex(d.Config.indexPath()); err != nil {
			return res, err
		}
	}
	syncer := &backup.Syncer{Client: client, Dir: d.Config.BackupDir, Index: idx, Concurrency: d.Config.Concurrency, Log: d.Log}
	res, err = syncer.Run(ctx)
	if !errors.Is(err, amazon.ErrAuth) {
		if serr := d.store.Save(); serr != nil {
			d.Log.Error("cannot save refreshed cookies", "err", serr)
		}
	}
	return res, err
}

// notify posts a plain-text message to NOTIFY_URL (ntfy-compatible) and
// reports whether it was delivered. The message never contains cookies.
func (d *Daemon) notify(title, message string) bool {
	if d.Config.NotifyURL == "" {
		return true // nothing to deliver
	}
	req, err := http.NewRequest(http.MethodPost, d.Config.NotifyURL, strings.NewReader(message))
	if err != nil {
		d.Log.Error("notification failed", "err", err)
		return false
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		d.Log.Error("notification failed; it will be sent again at the next attempt", "err", err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		d.Log.Error("notification rejected; it will be sent again at the next attempt", "status", resp.StatusCode)
		return false
	}
	return true
}
