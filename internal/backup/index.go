package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry records where a remote item was saved and which content it had.
type Entry struct {
	Path       string `json:"path"` // relative to the backup directory, slash-separated
	MD5        string `json:"md5"`  // checksum announced by Amazon
	Size       int64  `json:"size"` // bytes on disk
	RemoteSize int64  `json:"remote_size,omitempty"`
	// LocalMD5 is set only when Amazon consistently served bytes that do
	// not match the checksum it announced.
	LocalMD5   string    `json:"local_md5,omitempty"`
	Name       string    `json:"name"`
	Downloaded time.Time `json:"downloaded"`
}

// Index maps remote node ids to local files. It is what makes the backup
// incremental; losing it is harmless, since files are re-adopted by checksum.
type Index struct {
	path string

	mu       sync.Mutex
	entries  map[string]Entry
	owners   map[string]string // case-folded path → node id
	failures map[string]int    // node id → runs in which its download failed
	dirty    bool
}

type indexFile struct {
	Version  int              `json:"version"`
	Entries  map[string]Entry `json:"entries"`
	Failures map[string]int   `json:"failures,omitempty"`
}

// pathKey folds case, so that two names a case-insensitive disk would
// merge are treated as the same destination everywhere.
func pathKey(path string) string { return strings.ToLower(path) }

// LoadIndex reads the index, or starts an empty one if the file is missing.
func LoadIndex(path string) (*Index, error) {
	idx := &Index{path: path, entries: map[string]Entry{}, owners: map[string]string{}, failures: map[string]int{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for id, e := range f.Entries {
		idx.entries[id] = e
		idx.owners[pathKey(e.Path)] = id
	}
	for id, n := range f.Failures {
		idx.failures[id] = n
	}
	return idx, nil
}

func (i *Index) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.entries)
}

func (i *Index) Get(id string) (Entry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[id]
	return e, ok
}

// Owner returns the node whose file lives at path, if any.
func (i *Index) Owner(path string) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	id, ok := i.owners[pathKey(path)]
	return id, ok
}

// Set records a saved file; the item is no longer considered failing.
func (i *Index) Set(id string, e Entry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries[id] = e
	i.owners[pathKey(e.Path)] = id
	delete(i.failures, id)
	i.dirty = true
}

// Failures is the number of runs in which this item's download failed
// since it last succeeded.
func (i *Index) Failures(id string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failures[id]
}

func (i *Index) MarkFailed(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failures[id]++
	i.dirty = true
}

// Save writes the index atomically if it changed.
func (i *Index) Save() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.dirty {
		return nil
	}
	data, err := json.Marshal(indexFile{Version: 1, Entries: i.entries, Failures: i.failures})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(i.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(i.path), ".index-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), i.path); err != nil {
		return err
	}
	i.dirty = false
	return nil
}
