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
	Bytes       int64 `json:"bytes"`       // bytes downloaded
}

type Syncer struct {
	Client      *amazon.Client
	Dir         string
	Index       *Index
	Concurrency int
	Log         *slog.Logger
	Attempts    int           // tries per file; default 3
	RetryBase   time.Duration // pause before the first retry; default 5s
}

type job struct {
	node amazon.Node
	rel  string // destination, relative to Dir, OS-separated
}

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

	nodes, report, err := s.Client.ListAll(ctx)
	if err != nil {
		return res, fmt.Errorf("listing library: %w", err)
	}
	res.Remote, res.Unreachable = report.Total, report.Unreachable
	if report.Unreachable > 0 {
		s.Log.Warn("some items could not be listed (API paging cap)", "unreachable", report.Unreachable, "total", report.Total)
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
			s.Index.Set(n.ID, s.entry(n, rel))
		case actDownload:
			reserved[rel] = true
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

func (s *Syncer) entry(n amazon.Node, rel string) Entry {
	return Entry{Path: filepath.ToSlash(rel), MD5: n.Content.MD5, Size: n.Content.Size, Name: n.Name, Downloaded: time.Now().UTC()}
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
			if errors.Is(err, fs.ErrNotExist) && !reserved[rel] {
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
		if reserved[rel] {
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
			return "", 0, err
		}
		if owned && owner != n.ID {
			continue
		}
		// Something is already there. It is ours only if it has exactly
		// the content we are about to download.
		if fi.Mode().IsRegular() && n.Content.MD5 != "" && fi.Size() == n.Content.Size {
			sum, err := fileMD5(filepath.Join(s.Dir, rel))
			if err != nil {
				return "", 0, err
			}
			if strings.EqualFold(sum, n.Content.MD5) {
				return rel, actAdopt, nil
			}
		}
	}
	return "", 0, errors.New("no free file name found")
}

func sameContent(e Entry, n amazon.Node) bool {
	if e.MD5 != "" && n.Content.MD5 != "" {
		return strings.EqualFold(e.MD5, n.Content.MD5)
	}
	return e.Size == n.Content.Size
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

// download runs the jobs with bounded concurrency. It returns a non-nil
// error only when the whole run must stop (expired session, cancellation).
func (s *Syncer) download(ctx context.Context, jobs []job, res *Result) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu    sync.Mutex
		fatal error
		done  int
	)
	queue := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < s.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				written, err := s.fetchWithRetry(ctx, j)
				mu.Lock()
				switch {
				case err == nil:
					res.Downloaded++
					res.Bytes += written
					s.Index.Set(j.node.ID, s.entry(j.node, j.rel))
					s.Log.Debug("downloaded", "path", j.rel, "bytes", written)
				case errors.Is(err, amazon.ErrAuth) || ctx.Err() != nil:
					if fatal == nil {
						if errors.Is(err, amazon.ErrAuth) {
							fatal = err
						} else {
							fatal = ctx.Err()
						}
						cancel()
					}
				default:
					res.Failed++
					s.Log.Error("download failed", "id", j.node.ID, "name", j.node.Name, "err", err)
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

	mu.Lock()
	defer mu.Unlock()
	return fatal
}

func (s *Syncer) fetchWithRetry(ctx context.Context, j job) (int64, error) {
	var err error
	for attempt := 0; attempt < s.Attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(s.RetryBase << (attempt - 1)):
			}
		}
		var written int64
		written, err = s.fetch(ctx, j)
		if err == nil {
			return written, nil
		}
		if errors.Is(err, amazon.ErrAuth) || ctx.Err() != nil {
			return 0, err
		}
	}
	return 0, err
}

// fetch downloads one file next to its destination, checks it against the
// size and checksum Amazon announced, and only then gives it its final name.
func (s *Syncer) fetch(ctx context.Context, j job) (written int64, err error) {
	full := filepath.Join(s.Dir, j.rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, err
	}
	part := full + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			os.Remove(part)
		}
	}()
	h := md5.New()
	written, err = s.Client.Download(ctx, j.node, io.MultiWriter(f, h))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	want := j.node.Content
	if want.Size > 0 && written != want.Size {
		return 0, fmt.Errorf("size mismatch: got %d bytes, expected %d", written, want.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); want.MD5 != "" && !strings.EqualFold(sum, want.MD5) {
		return 0, fmt.Errorf("checksum mismatch: got %s, expected %s", sum, want.MD5)
	}
	if _, statErr := os.Lstat(full); statErr == nil {
		return 0, fmt.Errorf("%s appeared during the download; not overwriting it", j.rel)
	}
	if err = os.Rename(part, full); err != nil {
		return 0, err
	}
	if t, ok := nodeTime(j.node); ok {
		os.Chtimes(full, t, t)
	}
	return written, nil
}
