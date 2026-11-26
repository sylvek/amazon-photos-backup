package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"amazon-photos-backup/internal/amazon"
)

func node(id, name, contentDate, created string) amazon.Node {
	return amazon.Node{ID: id, Name: name, CreatedDate: created,
		Content: amazon.ContentProperties{ContentDate: contentDate, Extension: "jpg"}}
}

func TestTargetPathSanitizes(t *testing.T) {
	const d = "2023-05-01T10:00:00.000Z"
	cases := []struct {
		n    amazon.Node
		want string
	}{
		{node("id1", "IMG_1.jpg", d, "2024-01-01T00:00:00Z"), "2023/05/IMG_1.jpg"},
		{node("id1", "IMG_1.jpg", "", "2021-12-31T23:59:59Z"), "2021/12/IMG_1.jpg"},
		{node("id1", "IMG_1.jpg", "", ""), "unknown/IMG_1.jpg"},
		{node("id1", "IMG_1.jpg", "garbage", "also garbage"), "unknown/IMG_1.jpg"},
		{node("id1", "IMG_1.jpg", "0001-01-01T00:00:00Z", ""), "unknown/IMG_1.jpg"},
		{node("id1", "../../etc/passwd", d, ""), "2023/05/_.._etc_passwd"},
		{node("id1", `..\..\windows\win.ini`, d, ""), "2023/05/_.._windows_win.ini"},
		{node("id1", "/abs/path.jpg", d, ""), "2023/05/_abs_path.jpg"},
		{node("id1", "a\x00b\nc.jpg", d, ""), "2023/05/a_b_c.jpg"},
		{node("id1", ".hidden.jpg", d, ""), "2023/05/hidden.jpg"},
		{node("abc123", "", d, ""), "2023/05/abc123.jpg"},
		{node("abc123", "..", d, ""), "2023/05/abc123.jpg"},
		{node("abc123", "   ", d, ""), "2023/05/abc123.jpg"},
		{node("id1", "photo: été <1>.jpg", d, ""), "2023/05/photo_ été _1_.jpg"},
		{node("id1", "bad\xffutf8.jpg", d, ""), "2023/05/bad_utf8.jpg"},
	}
	for _, c := range cases {
		got := filepath.ToSlash(TargetPath(c.n))
		if got != c.want {
			t.Errorf("TargetPath(name=%q) = %q, want %q", c.n.Name, got, c.want)
		}
	}
}

func TestTargetPathNeverEscapes(t *testing.T) {
	names := []string{"..", ".", "../x", "a/../../b", "....//", "\\..\\", "/", "", "\x00", "~", "con"}
	for _, name := range names {
		rel := TargetPath(node("someid", name, "2023-05-01T10:00:00Z", ""))
		full := filepath.Join("/backup", rel)
		if !strings.HasPrefix(full, "/backup/2023/05/") || filepath.Dir(full) != "/backup/2023/05" {
			t.Errorf("name %q resolves to %q, outside its month folder", name, full)
		}
		if base := filepath.Base(full); base == "" || base == "." || strings.HasPrefix(base, ".") {
			t.Errorf("name %q gives unusable file name %q", name, base)
		}
	}
}

func TestTargetPathTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("é", 300) + ".jpeg"
	rel := TargetPath(node("id1", long, "2023-05-01T10:00:00Z", ""))
	base := filepath.Base(rel)
	if len(base) > 200 {
		t.Errorf("file name is %d bytes, want at most 200", len(base))
	}
	if !strings.HasSuffix(base, ".jpeg") {
		t.Errorf("extension lost: %q", base)
	}
	if strings.ContainsRune(base, '�') || strings.Contains(base, "_") {
		t.Errorf("truncation cut a character in half: %q", base)
	}
}

func TestWithSuffix(t *testing.T) {
	cases := map[string]string{
		"2023/05/IMG_1.jpg": "2023/05/IMG_1_abcd1234.jpg",
		"2023/05/noext":     "2023/05/noext_abcd1234",
		"2023/05/a.tar.gz":  "2023/05/a.tar_abcd1234.gz",
	}
	for in, want := range cases {
		if got := filepath.ToSlash(withSuffix(filepath.FromSlash(in), "abcd1234")); got != want {
			t.Errorf("withSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
