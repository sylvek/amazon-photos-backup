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

func TestSyncConsistentMismatchIsKeptOnce(t *testing.T) {
	// Amazon announces one checksum but reliably serves other bytes. Throwing
	// the file away would mean never backing it up and fetching it forever.
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("announced"))
	e.fake.SetContent("id-a", []byte("what amazon really serves"))
	res := e.mustRun()
	if res.Downloaded != 1 || res.Mismatched != 1 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if e.read("2023/05/a.jpg") != "what amazon really serves" {
		t.Errorf("served bytes were not kept")
	}
	if n := e.fake.DownloadCount("id-a"); n != 2 {
		t.Errorf("%d downloads, want 2 (one to notice, one to confirm)", n)
	}
	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 1 {
		t.Fatalf("second run: %+v", res)
	}
	if n := e.fake.DownloadCount("id-a"); n != 2 {
		t.Errorf("file fetched again on the next run: %d downloads", n)
	}
}

func TestSyncInconsistentDownloadsFail(t *testing.T) {
	// Two downloads that disagree with the checksum and with each other
	// are corruption, not a metadata quirk.
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("expected"))
	e.fake.Flaky["id-a"] = true
	res := e.mustRun()
	if res.Failed != 1 || res.Downloaded != 0 {
		t.Fatalf("%+v", res)
	}
	if files := e.files(); len(files) != 0 {
		t.Errorf("a corrupt download was kept: %v", files)
	}
	if n := e.fake.DownloadCount("id-a"); n != 2 {
		t.Errorf("%d downloads, want exactly 2", n)
	}
}

func TestSyncPartSiblingNotDestroyed(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-part", "IMG.jpg.part", "2023-05-01T10:00:00Z", []byte("a real file that happens to end in .part"))
	e.mustRun()
	e.fake.AddNode("id-img", "IMG.jpg", "2023-05-02T10:00:00Z", []byte("the photo"))
	// Even if Amazon lost the first item, our copy must survive the second download.
	e.fake.Update(func(f *amazontest.Fake) { f.Nodes = f.Nodes[1:] })
	e.mustRun()
	if got := e.read("2023/05/IMG.jpg.part"); got != "a real file that happens to end in .part" {
		t.Errorf("backed-up file was damaged while downloading its sibling: %q", got)
	}
	if e.read("2023/05/IMG.jpg") != "the photo" {
		t.Errorf("sibling not downloaded")
	}
}

func TestSyncForeignPartFileUntouched(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("A"), 50000))
	os.MkdirAll(filepath.Join(e.dir, "2023", "05"), 0o755)
	os.WriteFile(filepath.Join(e.dir, "2023", "05", "a.jpg.part"), []byte("someone else's file"), 0o644)
	e.fake.Truncate["id-a"] = true
	e.mustRun() // fails
	if e.read("2023/05/a.jpg.part") != "someone else's file" {
		t.Fatalf("a failed download deleted or changed an unrelated file")
	}
	e.fake.Update(func(f *amazontest.Fake) { delete(f.Truncate, "id-a") })
	if res := e.mustRun(); res.Downloaded != 1 {
		t.Fatalf("%+v", res)
	}
	if e.read("2023/05/a.jpg.part") != "someone else's file" {
		t.Errorf("a successful download deleted or changed an unrelated file")
	}
	for _, f := range e.files() {
		if strings.Contains(f, ".dl-") {
			t.Errorf("temporary file left behind: %s", f)
		}
	}
}

func TestSyncCaseOnlyCollisionGetsDistinctNames(t *testing.T) {
	// On a case-insensitive disk (macOS, Windows hosts) these two names are
	// one file. They must be told apart before any byte is written.
	e := newEnv(t)
	e.fake.AddNode("AAAAaaaa1111", "IMG_0001.JPG", "2023-05-01T10:00:00Z", []byte("upper"))
	e.fake.AddNode("BBBBbbbb2222", "IMG_0001.jpg", "2023-05-02T10:00:00Z", []byte("lower"))
	if res := e.mustRun(); res.Downloaded != 2 {
		t.Fatalf("%+v", res)
	}
	files := e.files()
	if len(files) != 2 || strings.EqualFold(files[0], files[1]) {
		t.Fatalf("names differ only by case: %v", files)
	}
	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 2 {
		t.Errorf("second run: %+v", res)
	}
}

func TestSyncItemWithoutSizeNotRedownloaded(t *testing.T) {
	e := newEnv(t)
	e.fake.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	e.fake.AddNode("id-b", "b.jpg", "2023-05-01T10:00:00Z", []byte("BBB"))
	e.fake.Update(func(f *amazontest.Fake) {
		f.Nodes[0].Content.Size = 0 // size unknown
		f.Nodes[1].Content.Size = 0 // neither size nor checksum
		f.Nodes[1].Content.MD5 = ""
	})
	if res := e.mustRun(); res.Downloaded != 2 {
		t.Fatalf("%+v", res)
	}
	for i := 0; i < 2; i++ {
		if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 2 {
			t.Fatalf("run %d: %+v", i+2, res)
		}
	}
	if got, want := e.files(), []string{"2023/05/a.jpg", "2023/05/b.jpg"}; !equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestSyncLocalWriteErrorIsFatal(t *testing.T) {
	// A backup directory that cannot be written (full disk, read-only
	// volume) must stop the run loudly, not count every file as "failed".
	e := newEnv(t)
	for _, id := range []string{"n0", "n1", "n2", "n3", "n4", "n5"} {
		e.fake.AddNode(id, id+".jpg", "2023-05-01T10:00:00Z", []byte("content "+id))
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(blocker, nil, 0o644)
	e.dir = filepath.Join(blocker, "photos")
	_, err := e.run(1)
	if err == nil || errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want a fatal local error", err)
	}
	if n := countDownloadRequests(e.fake); n > 1 {
		t.Errorf("%d download requests made although nothing can be written", n)
	}
}

func countDownloadRequests(f *amazontest.Fake) int {
	n := 0
	for _, r := range f.RequestLog() {
		if strings.HasSuffix(r.Path, "/contentRedirection") {
			n++
		}
	}
	return n
}

func addForbidden(e *env, prefix string, n int, date string) {
	for i := 0; i < n; i++ {
		id := prefix + string(rune('a'+i/26)) + string(rune('a'+i%26))
		e.fake.AddNode(id, id+".jpg", date, []byte("content "+id))
		e.fake.Forbidden[id] = true
	}
}

func TestSyncBreakerAbortsOnSystemicFailure(t *testing.T) {
	e := newEnv(t)
	addForbidden(e, "bad", 60, "2023-05-01T10:00:00Z")
	_, err := e.run(1)
	if err == nil || errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want an error: every download is being refused", err)
	}
	if n := countDownloadRequests(e.fake); n != 20 {
		t.Errorf("%d download requests before giving up, want 20", n)
	}
}

func TestSyncBreakerDetectsDeadSession(t *testing.T) {
	// The session dies mid-run, and Amazon answers 403 instead of 401.
	e := newEnv(t)
	for i := 0; i < 60; i++ {
		id := "n" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		e.fake.AddNode(id, id+".jpg", "2023-05-01T10:00:00Z", []byte("content "+id))
	}
	e.fake.DownloadAuthAs403 = true
	e.fake.ExpireAfter = 2
	res, err := e.run(1)
	if !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if res.Downloaded != 2 {
		t.Errorf("%+v", res)
	}
	if n := countDownloadRequests(e.fake); n != 22 {
		t.Errorf("%d download requests, want 22 (2 good + 20 refused)", n)
	}
}

func TestSyncKnownBadItemsDoNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	addForbidden(e, "bad", 25, "2024-01-01T10:00:00Z") // newest, so tried first
	for _, id := range []string{"g0", "g1", "g2", "g3", "g4"} {
		e.fake.AddNode(id, id+".jpg", "2020-05-01T10:00:00Z", []byte("content "+id))
	}
	if _, err := e.run(1); err == nil {
		t.Fatalf("first run should trip on 20 refusals in a row")
	}
	// Items that already failed are tried last and are not held against the run.
	res, err := e.run(1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Downloaded != 5 || res.Failed != 25 {
		t.Fatalf("second run: %+v, want the 5 good files saved", res)
	}
	// Once Amazon serves them again, they are picked up and forgotten as bad.
	e.fake.Update(func(f *amazontest.Fake) { f.Forbidden = map[string]bool{} })
	if res, err := e.run(1); err != nil || res.Downloaded != 25 || res.Failed != 0 {
		t.Fatalf("after recovery: %+v err=%v", res, err)
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
