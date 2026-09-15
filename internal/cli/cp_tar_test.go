package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarEntry builds a crafted archive entry; body is written for regular files.
type tarEntry struct {
	name, link string
	typ        byte
	body       string
}

func craftArchive(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "crafted.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0644, ModTime: time.Now()}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0755
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return archive
}

// A download is untrusted input as far as the host is concerned: names must
// stay under the destination, and a symlink or hard link must never let a
// later entry write outside it. Every hostile shape is refused with nothing
// written; honest shapes (links inside the tree, skipped fifos) still work.
func TestArchiveGuards(t *testing.T) {
	outside := t.TempDir() // must stay empty in every hostile case
	for _, tc := range []struct {
		name     string
		entries  []tarEntry
		readErr  string // refused by readArchive (before anything is written)
		extErr   string // refused by extractArchive
		want     map[string]string
		wantWarn string
	}{
		{name: "absolute name", entries: []tarEntry{{name: "/etc/passwd", typ: tar.TypeReg, body: "x"}}, readErr: "refusing archive entry"},
		{name: "dotdot name", entries: []tarEntry{{name: "proj/../../x", typ: tar.TypeReg, body: "x"}}, readErr: "refusing archive entry"},
		{name: "symlink then write through it", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/link", typ: tar.TypeSymlink, link: outside},
			{name: "proj/link/authorized_keys", typ: tar.TypeReg, body: "evil"},
		}, extErr: "refusing to write through symlink"},
		{name: "symlink to dotdot then write through it", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/up", typ: tar.TypeSymlink, link: "../.."},
			{name: "proj/up/x", typ: tar.TypeReg, body: "evil"},
		}, extErr: "refusing to write through symlink"},
		{name: "hard link outside the tree", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/x", typ: tar.TypeLink, link: "/etc/passwd"},
		}, readErr: "hard link"},
		{name: "hard link to another top", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/x", typ: tar.TypeLink, link: "other/y"},
		}, readErr: "outside the copied tree"},
		{name: "hard link to a later entry", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/x", typ: tar.TypeLink, link: "proj/y"},
			{name: "proj/y", typ: tar.TypeReg, body: "late"},
		}, readErr: "outside the copied tree"},
		{name: "hard link inside the tree", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/y", typ: tar.TypeReg, body: "shared"},
			{name: "proj/x", typ: tar.TypeLink, link: "proj/y"},
		}, want: map[string]string{"proj/y": "shared", "proj/x": "shared"}},
		{name: "fifo skipped with a note", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/pipe", typ: tar.TypeFifo},
			{name: "proj/f", typ: tar.TypeReg, body: "ok"},
		}, want: map[string]string{"proj/f": "ok"}, wantWarn: "skipping proj/pipe"},
		{name: "symlink itself is fine", entries: []tarEntry{
			{name: "proj/", typ: tar.TypeDir},
			{name: "proj/link", typ: tar.TypeSymlink, link: "/etc/hostname"},
			{name: "proj/f", typ: tar.TypeReg, body: "ok"},
		}, want: map[string]string{"proj/f": "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := craftArchive(t, tc.entries...)
			var warn bytes.Buffer
			entries, err := readArchive(archive, &warn)
			if tc.readErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.readErr) {
					t.Fatalf("readArchive error = %v, want %q", err, tc.readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readArchive: %v", err)
			}
			if tc.wantWarn != "" && !strings.Contains(warn.String(), tc.wantWarn) {
				t.Fatalf("warning = %q, want %q", warn.String(), tc.wantWarn)
			}
			dst := t.TempDir()
			roots := map[string]string{}
			for _, e := range entries {
				roots[e.name] = filepath.Join(dst, e.name)
			}
			err = extractArchive(archive, roots, false)
			if tc.extErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.extErr) {
					t.Fatalf("extractArchive error = %v, want %q", err, tc.extErr)
				}
				if got, _ := os.ReadDir(outside); len(got) != 0 {
					t.Fatalf("wrote outside the destination: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractArchive: %v", err)
			}
			for rel, body := range tc.want {
				data, err := os.ReadFile(filepath.Join(dst, rel))
				if err != nil || string(data) != body {
					t.Errorf("%s = %q, %v; want %q", rel, data, err, body)
				}
			}
			if _, err := os.Lstat(filepath.Join(dst, "proj/pipe")); tc.wantWarn != "" && err == nil {
				t.Error("skipped fifo was created")
			}
		})
	}
	if got, _ := os.ReadDir(outside); len(got) != 0 {
		t.Fatalf("something wrote outside the destination: %v", got)
	}
}
