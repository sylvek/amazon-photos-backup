package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/amazon/amazontest"
	"amazon-photos-backup/internal/backup"
)

type env struct {
	t     *testing.T
	fake  *amazontest.Fake
	dir   string // backup directory
	state string // state directory
}

func newEnv(t *testing.T) *env {
	return &env{t: t, fake: amazontest.New(t), dir: filepath.Join(t.TempDir(), "photos"), state: t.TempDir()}
}

// run performs one sync with a fresh client, index and syncer, the way a
// restarted daemon would.
func (e *env) run(concurrency int) (backup.Result, error) {
	e.t.Helper()
	store := amazon.NewStore(filepath.Join(e.state, "cookies.json"))
	store.Replace(e.fake.Cookies())
	client, err := amazon.NewClient(amazon.Options{BaseURL: e.fake.URL(), Store: store, Retries: 2, RetryBase: time.Millisecond})
	if err != nil {
		e.t.Fatal(err)
	}
	idx, err := backup.LoadIndex(filepath.Join(e.state, "index.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	s := &backup.Syncer{
		Client: client, Dir: e.dir, Index: idx, Concurrency: concurrency,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryBase: time.Millisecond,
	}
	return s.Run(context.Background())
}

func (e *env) mustRun() backup.Result {
	e.t.Helper()
	res, err := e.run(4)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// files returns every file under the backup directory, relative and sorted.
func (e *env) files() []string {
	e.t.Helper()
	var out []string
	filepath.WalkDir(e.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (e *env) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) index() *backup.Index {
	e.t.Helper()
	idx, err := backup.LoadIndex(filepath.Join(e.state, "index.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	return idx
}

func equal(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func TestSyncDownloadsThenSkips(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.fake.AddNode("id-b", "b.mp4", "2022-12-31T23:00:00Z", bytes.Repeat([]byte("B"), 300000))
	e.fake.AddNode("id-c", "c.jpg", "", []byte("CCC"))

	res := e.mustRun()
	if res.Remote != 3 || res.Downloaded != 3 || res.Skipped != 0 || res.Failed != 0 || res.Bytes != 300006 {
		t.Fatalf("first run: %+v", res)
	}
	if got, want := e.files(), []string{"2022/12/b.mp4", "2023/05/a.jpg", "unknown/c.jpg"}; !equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if e.read("2023/05/a.jpg") != "AAA" {
		t.Errorf("content mismatch")
	}

	res = e.mustRun()
	if res.Downloaded != 0 || res.Skipped != 3 || res.Failed != 0 {
		t.Fatalf("second run: %+v", res)
	}
	if n := e.fake.TotalDownloads(); n != 3 {
		t.Errorf("server served %d downloads over two runs, want 3", n)
	}
}

func TestSyncNameCollision(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("AAAAaaaa1111", "IMG_0001.jpg", "2023-05-01T10:00:00Z", []byte("first camera"))
	e.fake.AddNode("BBBBbbbb2222", "IMG_0001.jpg", "2023-05-20T10:00:00Z", []byte("second camera"))
	res := e.mustRun()
	if res.Downloaded != 2 {
		t.Fatalf("%+v", res)
	}
	files := e.files()
	if len(files) != 2 {
		t.Fatalf("files = %v, want two distinct files", files)
	}
	contents := []string{e.read(files[0]), e.read(files[1])}
	sort.Strings(contents)
	if !equal(contents, []string{"first camera", "second camera"}) {
		t.Errorf("one photo overwrote the other: %v", contents)
	}
	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 2 {
		t.Errorf("second run: %+v", res)
	}
}

func TestSyncChangedContentKeepsOld(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("EDITEDnode00", "a.jpg", "2023-05-01T10:00:00Z", []byte("original"))
	e.mustRun()

	edited := []byte("edited version")
	e.fake.Update(func(f *amazontest.Fake) {
		f.Nodes = nil
	})
	e.fake.AddNode("EDITEDnode00", "a.jpg", "2023-05-01T10:00:00Z", edited)

	res := e.mustRun()
	if res.Downloaded != 1 {
		t.Fatalf("after edit: %+v", res)
	}
	if got, want := e.files(), []string{"2023/05/a.jpg", "2023/05/a_EDITEDno.jpg"}; !equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if e.read("2023/05/a.jpg") != "original" || e.read("2023/05/a_EDITEDno.jpg") != "edited version" {
		t.Errorf("old version was not preserved")
	}
	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 1 {
		t.Errorf("third run: %+v", res)
	}
}

func TestSyncAdoptsExistingFile(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.fake.AddNode("id-b", "b.jpg", "2023-05-01T10:00:00Z", []byte("BBB"))
	e.mustRun()
	if err := os.Remove(filepath.Join(e.state, "index.json")); err != nil {
		t.Fatal(err)
	}
	res := e.mustRun()
	if res.Adopted != 2 || res.Downloaded != 0 {
		t.Fatalf("after losing the index: %+v", res)
	}
	if n := e.fake.TotalDownloads(); n != 2 {
		t.Errorf("files were downloaded again: %d downloads", n)
	}
	if e.index().Len() != 2 {
		t.Errorf("index not rebuilt: %d entries", e.index().Len())
	}
}

func TestSyncDoesNotOverwriteForeignFile(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("NODEid123456", "a.jpg", "2023-05-01T10:00:00Z", []byte("from amazon"))
	os.MkdirAll(filepath.Join(e.dir, "2023", "05"), 0o755)
	os.WriteFile(filepath.Join(e.dir, "2023", "05", "a.jpg"), []byte("my own file"), 0o644)
	res := e.mustRun()
	if res.Downloaded != 1 {
		t.Fatalf("%+v", res)
	}
	if e.read("2023/05/a.jpg") != "my own file" {
		t.Errorf("an existing file was overwritten")
	}
	if e.read("2023/05/a_NODEid12.jpg") != "from amazon" {
		t.Errorf("download not stored beside the existing file: %v", e.files())
	}
}

func TestSyncRedownloadsDeletedFile(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.mustRun()
	os.Remove(filepath.Join(e.dir, "2023", "05", "a.jpg"))
	if res := e.mustRun(); res.Downloaded != 1 || e.read("2023/05/a.jpg") != "AAA" {
		t.Fatalf("deleted file not restored: %+v", res)
	}
}

func TestSyncTruncatedDownloadLeavesNoFile(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("A"), 100000))
	e.fake.Truncate["id-a"] = true
	res := e.mustRun()
	if res.Failed != 1 || res.Downloaded != 0 {
		t.Fatalf("%+v", res)
	}
	if files := e.files(); len(files) != 0 {
		t.Errorf("a failed download left files behind: %v", files)
	}
	if e.index().Len() != 0 {
		t.Errorf("a failed download was indexed")
	}
	e.fake.Update(func(f *amazontest.Fake) { delete(f.Truncate, "id-a") })
	if res := e.mustRun(); res.Downloaded != 1 || res.Failed != 0 {
		t.Fatalf("retry on the next run: %+v", res)
	}
}

func TestSyncRetriesInterruptedDownload(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("A"), 100000))
	e.fake.TruncateTimes["id-a"] = 2
	res := e.mustRun()
	if res.Downloaded != 1 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if got := e.read("2023/05/a.jpg"); len(got) != 100000 {
		t.Errorf("file has %d bytes, want 100000", len(got))
	}
}

func TestSyncBadMD5Fails(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("expected"))
	e.fake.SetContent("id-a", []byte("tampered")) // same length, different bytes
	res := e.mustRun()
	if res.Failed != 1 || res.Downloaded != 0 {
		t.Fatalf("%+v", res)
	}
	if files := e.files(); len(files) != 0 {
		t.Errorf("a corrupt download was kept: %v", files)
	}
}

func TestSyncStopsOnAuthError(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"n0", "n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9"} {
		e.fake.AddNode(id, id+".jpg", "2023-05-01T10:00:00Z", []byte("content "+id))
	}
	e.fake.ExpireAfter = 3 // the session dies during the run
	res, err := e.run(1)
	if !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got err %v, want ErrAuth", err)
	}
	if res.Downloaded != 3 {
		t.Errorf("result reports %d downloads, want the 3 made before expiry", res.Downloaded)
	}
	if n := len(e.files()); n != 3 {
		t.Errorf("%d files on disk, want 3: %v", n, e.files())
	}
	if n := e.index().Len(); n != 3 {
		t.Errorf("index saved %d entries, want 3", n)
	}
}

func TestSyncSetsMtime(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2019-07-14T12:30:00Z", []byte("AAA"))
	e.mustRun()
	fi, err := os.Stat(filepath.Join(e.dir, "2019", "07", "a.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2019, 7, 14, 12, 30, 0, 0, time.UTC); !fi.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", fi.ModTime().UTC(), want)
	}
}

func TestSyncSkipsTrashed(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.fake.AddNode("id-t", "t.jpg", "2023-05-01T10:00:00Z", []byte("TTT"))
	e.fake.Update(func(f *amazontest.Fake) { f.Nodes[1].Status = "TRASH" })
	res := e.mustRun()
	if res.Downloaded != 1 || !equal(e.files(), []string{"2023/05/a.jpg"}) {
		t.Fatalf("%+v files=%v", res, e.files())
	}
}

func TestSyncForbiddenFileDoesNotStopOthers(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.fake.AddNode("id-b", "b.jpg", "2023-05-01T10:00:00Z", []byte("BBB"))
	e.fake.Forbidden["id-a"] = true
	res := e.mustRun()
	if res.Downloaded != 1 || res.Failed != 1 {
		t.Fatalf("%+v", res)
	}
	// A refusal is final: asking again would only slow the run down.
	attempts := 0
	for _, r := range e.fake.RequestLog() {
		if strings.Contains(r.Path, "/nodes/id-a/contentRedirection") {
			attempts++
		}
	}
	if attempts != 1 {
		t.Errorf("forbidden download attempted %d times, want 1", attempts)
	}
}
