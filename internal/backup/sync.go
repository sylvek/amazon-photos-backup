package backup

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amazon-photos-backup/internal/amazon"
)

// Result summarises one synchronisation.
type Result struct {
	Remote      int   `json:"remote"`      // items Amazon reports
	Downloaded  int   `json:"downloaded"`  // new files written
	Skipped     int   `json:"skipped"`     // already backed up
	Adopted     int   `json:"adopted"`     // found on disk with the right checksum
	Failed      int   `json:"failed"`      // could not be downloaded this time
	Unreachable int   `json:"unreachable"` // could not even be listed
	Mismatched  int   `json:"mismatched"`  // saved although Amazon's checksum disagrees with what it serves
	Bytes       int64 `json:"bytes"`       // bytes downloaded
}

type Syncer struct {
	Client      *amazon.Client
	Dir         string
	Index       *Index
	Concurrency int
	Log         *slog.Logger
	Attempts    int           // tries per file once data has started flowing; default 3
	RetryBase   time.Duration // pause before the first retry; default 5s
	// FailureBurst is how many downloads in a row may fail, among items
	// that never failed before, until the run is stopped as a systemic
	// problem rather than a few bad files; default 20.
	FailureBurst int
}

type job struct {
	node amazon.Node
	rel  string // destination, relative to Dir, OS-separated
}

// localError marks a failure of the backup directory itself (disk full,
// read-only, permissions). No amount of retrying other files helps.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// mismatchError is a complete download whose bytes are not the ones Amazon
// announced.
type mismatchError struct {
	sum  string
	size int64
	want amazon.ContentProperties
}

func (e *mismatchError) Error() string {
	return fmt.Sprintf("content mismatch: got %d bytes with md5 %s, Amazon announced %d bytes with md5 %s", e.size, e.sum, e.want.Size, e.want.MD5)
}

var errBurst = errors.New("too many downloads failed in a row")

// Run lists the remote library and downloads what is missing locally. It
// never deletes or overwrites a file. An error wrapping amazon.ErrAuth means
// the session expired; files downloaded before that are kept and indexed.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	if s.Concurrency <= 0 {
		s.Concurrency = 4
	}
	if s.Attempts <= 0 {
		s.Attempts = 3
	}
	if s.RetryBase <= 0 {
		s.RetryBase = 5 * time.Second
	}
	if s.FailureBurst <= 0 {
		s.FailureBurst = 20
	}

	nodes, report, err := s.Client.ListAll(ctx)
	if err != nil {
		return res, fmt.Errorf("listing library: %w", err)
	}
	res.Remote, res.Unreachable = report.Total, report.Unreachable
	for _, skipped := range report.SliceErrors {
		s.Log.Warn("Amazon refused to list a date range", "detail", skipped)
	}
	if report.Malformed > 0 {
		s.Log.Warn("some items came in a form this program cannot read", "count", report.Malformed)
	}
	if report.Unreachable > 0 {
		s.Log.Warn("some items could not be listed", "unreachable", report.Unreachable, "total", report.Total)
	}

	// Decide what to do with every item first, single-threaded, so that two
	// items can never be assigned the same destination.
	var jobs []job
	reserved := map[string]bool{}
	for _, n := range nodes {
		if (n.Kind != "" && n.Kind != "FILE") || (n.Status != "" && n.Status != "AVAILABLE") {
			continue
		}
		rel, action, err := s.plan(n, reserved)
		var local *localError
		if errors.As(err, &local) {
			return res, fmt.Errorf("cannot read the backup directory: %w", err)
		}
		if err != nil {
			s.Log.Error("cannot place item", "id", n.ID, "name", n.Name, "err", err)
			res.Failed++
			continue
		}
		switch action {
		case actSkip:
			res.Skipped++
		case actAdopt:
			res.Adopted++
			fi, _ := os.Lstat(filepath.Join(s.Dir, rel))
			var size int64
			if fi != nil {
				size = fi.Size()
			}
			s.Index.Set(n.ID, s.entry(n, rel, size, ""))
		case actDownload:
			reserved[pathKey(rel)] = true
			jobs = append(jobs, job{node: n, rel: rel})
		}
	}
	if err := s.Index.Save(); err != nil {
		return res, fmt.Errorf("saving index: %w", err)
	}
	s.Log.Info("inventory done", "remote", res.Remote, "to_download", len(jobs), "already_saved", res.Skipped, "adopted", res.Adopted)

	fatal := s.download(ctx, jobs, &res)
	if err := s.Index.Save(); err != nil && fatal == nil {
		fatal = fmt.Errorf("saving index: %w", err)
	}
	return res, fatal
}

type action int

const (
	actSkip action = iota
	actAdopt
	actDownload
)

func (s *Syncer) entry(n amazon.Node, rel string, size int64, localMD5 string) Entry {
	return Entry{
		Path: filepath.ToSlash(rel), MD5: n.Content.MD5, Size: size, RemoteSize: n.Content.Size,
		LocalMD5: localMD5, Name: n.Name, Downloaded: time.Now().UTC(),
	}
}

// plan picks the destination of a node and says whether it still has to be
// downloaded.
func (s *Syncer) plan(n amazon.Node, reserved map[string]bool) (string, action, error) {
	candidate := TargetPath(n)
	if e, ok := s.Index.Get(n.ID); ok {
		rel := filepath.FromSlash(e.Path)
		if sameContent(e, n) {
			fi, err := os.Lstat(filepath.Join(s.Dir, rel))
			if err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
				return rel, actSkip, nil
			}
			if errors.Is(err, fs.ErrNotExist) && !reserved[pathKey(rel)] {
				return rel, actDownload, nil // the file was removed locally: restore it
			}
		}
		// The content changed on Amazon, or our file was altered. Either
		// way the existing file stays, and this version goes beside it.
	}
	suffix := shortID(n.ID)
	for attempt := 0; attempt < 1000; attempt++ {
		rel := candidate
		switch {
		case attempt == 1:
			rel = withSuffix(candidate, suffix)
		case attempt > 1:
			rel = withSuffix(candidate, fmt.Sprintf("%s_%d", suffix, attempt))
		}
		if reserved[pathKey(rel)] {
			continue
		}
		owner, owned := s.Index.Owner(filepath.ToSlash(rel))
		fi, err := os.Lstat(filepath.Join(s.Dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if owned && owner != n.ID {
				continue // promised to another item whose file is missing
			}
			return rel, actDownload, nil
		case err != nil:
			return "", 0, &localError{err}
		}
		if owned && owner != n.ID {
			continue
		}
		// Something is already there. It is ours only if it has exactly
		// the content we are about to download.
		sizeFits := n.Content.Size == 0 || fi.Size() == n.Content.Size
		if fi.Mode().IsRegular() && n.Content.MD5 != "" && sizeFits {
			sum, err := fileMD5(filepath.Join(s.Dir, rel))
			if err != nil {
				return "", 0, &localError{err}
			}
			if strings.EqualFold(sum, n.Content.MD5) {
				return rel, actAdopt, nil
			}
		}
	}
	return "", 0, errors.New("no free file name found")
}

// sameContent says whether the remote item is still the version we saved.
// It compares what Amazon announces now with what it announced then, never
// with what is on disk: the two can legitimately differ (see mismatchError).
func sameContent(e Entry, n amazon.Node) bool {
	switch {
	case e.MD5 != "" && n.Content.MD5 != "":
		return strings.EqualFold(e.MD5, n.Content.MD5)
	case e.RemoteSize > 0 && n.Content.Size > 0:
		return e.RemoteSize == n.Content.Size
	}
	return true // nothing to compare: assume unchanged rather than fetch it forever
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type fetched struct {
	written    int64
	sum        string
	mismatched bool
}

// download runs the jobs with bounded concurrency. It returns a non-nil
// error only when the whole run must stop: expired session, cancellation,
// unusable backup directory, or downloads failing wholesale.
func (s *Syncer) download(parent context.Context, jobs []job, res *Result) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Items that failed in earlier runs go last: whatever is wrong with
	// them must not stand between the run and the files that do work.
	knownBad := map[string]bool{}
	for _, j := range jobs {
		if s.Index.Failures(j.node.ID) > 0 {
			knownBad[j.node.ID] = true
		}
	}
	sort.SliceStable(jobs, func(a, b int) bool { return !knownBad[jobs[a].node.ID] && knownBad[jobs[b].node.ID] })

	var (
		mu       sync.Mutex
		fatal    error
		done     int
		streak   int // consecutive failures among items that never failed before
		lastFail error
	)
	stop := func(reason error) {
		if fatal == nil {
			fatal = reason
			cancel()
		}
	}
	queue := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < s.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				if ctx.Err() != nil {
					continue
				}
				got, err := s.fetchWithRetry(ctx, j)
				mu.Lock()
				var local *localError
				switch {
				case err == nil:
					streak = 0
					res.Downloaded++
					res.Bytes += got.written
					localMD5 := ""
					if got.mismatched {
						res.Mismatched++
						localMD5 = got.sum
						s.Log.Warn("saved a file whose content differs from the checksum Amazon announced; two downloads returned identical bytes",
							"path", j.rel, "announced_md5", j.node.Content.MD5, "received_md5", got.sum)
					}
					s.Index.Set(j.node.ID, s.entry(j.node, j.rel, got.written, localMD5))
					s.Log.Debug("downloaded", "path", j.rel, "bytes", got.written)
				case errors.Is(err, amazon.ErrAuth):
					stop(err)
				case errors.As(err, &local):
					stop(fmt.Errorf("cannot write to the backup directory: %w", err))
				case ctx.Err() != nil:
					stop(ctx.Err())
				default:
					res.Failed++
					lastFail = err
					s.Index.MarkFailed(j.node.ID)
					s.Log.Error("download failed", "id", j.node.ID, "name", j.node.Name, "err", err)
					if !knownBad[j.node.ID] {
						if streak++; streak >= s.FailureBurst {
							stop(errBurst)
						}
					}
				}
				done++
				if done%50 == 0 {
					if err := s.Index.Save(); err != nil {
						s.Log.Error("saving index", "err", err)
					}
					s.Log.Info("progress", "done", done, "of", len(jobs), "downloaded", res.Downloaded, "failed", res.Failed)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if errors.Is(fatal, errBurst) {
		// Either the session died in a way Amazon reports as a refusal, or
		// downloads are failing wholesale. Ask Amazon which.
		if err := s.Client.Check(parent); errors.Is(err, amazon.ErrAuth) {
			return err
		}
		return fmt.Errorf("stopped after %d downloads failed in a row; last error: %w", s.FailureBurst, lastFail)
	}
	return fatal
}

func (s *Syncer) fetchWithRetry(ctx context.Context, j job) (fetched, error) {
	accept := "" // checksum of a previous complete-but-unexpected download
	for attempt := 0; ; attempt++ {
		got, err := s.fetch(ctx, j, accept)
		if err == nil {
			return got, nil
		}
		var mismatch *mismatchError
		var local *localError
		switch {
		case errors.Is(err, amazon.ErrAuth), errors.As(err, &local), amazon.Permanent(err), ctx.Err() != nil:
			return got, err
		case errors.As(err, &mismatch):
			// Download it once more. Identical bytes mean this is simply
			// what Amazon serves, and they are kept; different bytes mean
			// corruption, and repeating will not make it better.
			if accept != "" {
				return got, err
			}
			accept = mismatch.sum
			continue
		case got.written == 0:
			// Nothing arrived: the client already retried the request.
			return got, err
		case attempt+1 >= s.Attempts:
			return got, err
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-time.After(s.RetryBase << attempt):
		}
	}
}

// trackingWriter remembers the first error of the underlying writer, to
// tell a failing disk from a failing network.
type trackingWriter struct {
	w   io.Writer
	err error
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil && t.err == nil {
		t.err = err
	}
	return n, err
}

// fetch downloads one file into a private temporary file beside its
// destination, checks it against what Amazon announced, and only then gives
// it its final name, without ever replacing an existing file.
//
// accept, when set, is the checksum a previous download of this item had:
// a download matching it is kept even though it contradicts the metadata.
func (s *Syncer) fetch(ctx context.Context, j job, accept string) (got fetched, err error) {
	full := filepath.Join(s.Dir, j.rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return got, &localError{err}
	}
	// The name starts with a dot, which no backed-up file can (see
	// sanitizeName), so the temporary file never collides with one.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".dl-*")
	if err != nil {
		return got, &localError{err}
	}
	defer os.Remove(tmp.Name())

	h := md5.New()
	file := &trackingWriter{w: tmp}
	got.written, err = s.Client.Download(ctx, j.node, io.MultiWriter(file, h))
	if file.err != nil {
		tmp.Close()
		return got, &localError{file.err}
	}
	if err != nil {
		tmp.Close()
		return got, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return got, &localError{err}
	}
	if err := tmp.Close(); err != nil {
		return got, &localError{err}
	}

	want := j.node.Content
	got.sum = hex.EncodeToString(h.Sum(nil))
	asAnnounced := (want.Size == 0 || got.written == want.Size) && (want.MD5 == "" || strings.EqualFold(got.sum, want.MD5))
	if !asAnnounced {
		if accept == "" || got.sum != accept {
			return got, &mismatchError{sum: got.sum, size: got.written, want: want}
		}
		got.mismatched = true
	}

	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return got, &localError{err}
	}
	if err := publish(tmp.Name(), full); err != nil {
		return got, err
	}
	if t, ok := nodeTime(j.node); ok {
		os.Chtimes(full, t, t)
	}
	return got, nil
}

// publish gives a finished download its final name. A hard link fails if
// the name is taken, which makes "never overwrite" atomic; filesystems
// without hard links fall back to check-then-rename.
func publish(tmp, full string) error {
	err := os.Link(tmp, full)
	if err == nil {
		return nil // the caller removes tmp
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s appeared during the download; not overwriting it", full)
	}
	if _, statErr := os.Lstat(full); statErr == nil {
		return fmt.Errorf("%s appeared during the download; not overwriting it", full)
	}
	if err := os.Rename(tmp, full); err != nil {
		return &localError{err}
	}
	return nil
}
