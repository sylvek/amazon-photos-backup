// Package backup turns the remote inventory into files on disk.
package backup

import (
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"amazon-photos-backup/internal/amazon"
)

const maxNameBytes = 200

// nodeTime is the moment a photo was taken, or failing that, uploaded.
func nodeTime(n amazon.Node) (time.Time, bool) {
	for _, s := range []string{n.Content.ContentDate, n.CreatedDate} {
		t, err := time.Parse(time.RFC3339, s)
		if err == nil && t.Year() >= 1900 && t.Year() <= 9999 {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// TargetPath is where a node belongs inside the backup directory:
// YYYY/MM/<name>, or unknown/<name> when it has no usable date. The name
// comes from a remote server and is treated as hostile: the result is always
// a plain file name directly inside its folder.
func TargetPath(n amazon.Node) string {
	dir := "unknown"
	if t, ok := nodeTime(n); ok {
		dir = filepath.Join(t.Format("2006"), t.Format("01"))
	}
	name := sanitizeName(n.Name)
	if name == "" {
		name = sanitizeName(n.ID)
		if name == "" {
			name = "file"
		}
		if ext := sanitizeExt(n.Content.Extension); ext != "" {
			name += "." + ext
		}
	}
	return filepath.Join(dir, truncateName(name))
}

func sanitizeName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return '_'
		case strings.ContainsRune(`/\:*?"<>|`, r): // separators, and characters other filesystems reject
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, ".") // no hidden files, no "." or ".."
	name = strings.TrimRight(name, " .")
	return strings.TrimSpace(name)
}

func sanitizeExt(ext string) string {
	ext = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, ext)
	if len(ext) > 10 {
		ext = ext[:10]
	}
	return ext
}

// truncateName shortens a name to maxNameBytes while keeping its extension
// and without cutting a UTF-8 sequence.
func truncateName(name string) string {
	if len(name) <= maxNameBytes {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 20 {
		ext = ""
	}
	stem := name[:len(name)-len(ext)]
	limit := maxNameBytes - len(ext)
	for len(stem) > limit {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	return stem + ext
}

// withSuffix inserts _suffix before the extension of a path.
func withSuffix(rel, suffix string) string {
	ext := filepath.Ext(rel)
	return strings.TrimSuffix(rel, ext) + "_" + suffix + ext
}

// shortID is a file-name-safe fragment of a node id, used to tell apart
// files that would otherwise share a name.
func shortID(id string) string {
	id = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "dup"
	}
	return id
}
