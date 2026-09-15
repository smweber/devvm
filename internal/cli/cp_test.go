package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
)

// Exercise the actual guest script with local processes, without a live VM.
type localCopyBackend struct {
	backend.Backend
	home       string
	stage      string
	failUpload bool
}

func (b *localCopyBackend) Kind() string { return "remote-unmanaged" }
func (b *localCopyBackend) Run(ctx context.Context, o backend.ExecOpts, argv ...string) error {
	if argv[0] == "mktemp" {
		// Mirror the real template so the caller's prefix check is exercised:
		// staging must stay out of /tmp (see copyStageDir).
		if len(argv) != 3 || argv[2] != copyStageDir+"/devvm-cp-XXXXXXXXXX" {
			return fmt.Errorf("unexpected mktemp argv %q", argv)
		}
		var err error
		b.stage, err = os.MkdirTemp(copyStageDir, "devvm-cp-")
		if err == nil {
			fmt.Fprintln(o.Stdout, b.stage)
		}
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+b.home)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
	return cmd.Run()
}
func (b *localCopyBackend) Copy(src, dst string) error {
	if b.failUpload {
		return fmt.Errorf("upload failed")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

const payload = "payload\x00\xff"

// writeTree creates a file (with parents) holding payload.
func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(payload), 0751); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopyArchive(t *testing.T) {
	const base = "source ' file"
	for _, tc := range []struct {
		name    string
		files   []string // files created under the source root
		srcs    []string // top-level entries of the source root to copy
		dst     string   // guest DEST
		opts    copyOpts
		home    []string // files pre-existing in the guest home (holding "old")
		want    []string // files (relative to home) that must hold payload afterwards
		absent  []string // paths that must not exist afterwards
		wantErr string
	}{
		{name: "default destination", files: []string{base}, dst: "~", opts: copyOpts{asDir: true}, want: []string{base}},
		{name: "relative file", files: []string{base}, dst: "renamed.txt", want: []string{"renamed.txt"}},
		{name: "quoted path", files: []string{base}, dst: "a ' $(touch INJECTED).txt", want: []string{"a ' $(touch INJECTED).txt"}},
		{name: "home expansion", files: []string{base}, dst: "~/renamed.txt", want: []string{"renamed.txt"}},
		{name: "existing directory", files: []string{base}, dst: "target", home: []string{"target/.keep"}, want: []string{"target/" + base}},
		{name: "trailing slash creates directory", files: []string{base}, dst: "new/deep/", opts: copyOpts{asDir: true}, want: []string{"new/deep/" + base}},
		{name: "target directory flag", files: []string{base, "second.txt"}, srcs: []string{base, "second.txt"}, dst: "inbox", opts: copyOpts{asDir: true},
			want: []string{"inbox/" + base, "inbox/second.txt"}},
		{name: "directory rename", files: []string{base + "/nested/data"}, dst: "target", want: []string{"target/nested/data"}},
		{name: "directory into directory", files: []string{base + "/nested/data"}, dst: "target",
			home: []string{"target/.keep"}, want: []string{"target/" + base + "/nested/data"}},
		{name: "missing parent", files: []string{base}, dst: "missing/file",
			wantErr: "no such directory", absent: []string{"missing"}},
		{name: "refuses overwrite", files: []string{base}, dst: "renamed.txt", home: []string{"renamed.txt"},
			wantErr: "refusing to overwrite (use -f):\n  "},
		{name: "refuses nested overwrite", files: []string{base + "/nested/data", base + "/nested/fresh"}, dst: "target",
			home: []string{"target/" + base + "/nested/data"}, wantErr: "/nested/data", absent: []string{"target/" + base + "/nested/fresh"}},
		{name: "refuses file over directory", files: []string{base}, dst: "target", home: []string{"target/" + base + "/inner"},
			wantErr: "refusing to overwrite"},
		{name: "force overwrites", files: []string{base}, dst: "renamed.txt", opts: copyOpts{force: true}, home: []string{"renamed.txt"}, want: []string{"renamed.txt"}},
		{name: "force merges", files: []string{base + "/nested/data"}, dst: "target", opts: copyOpts{force: true},
			home: []string{"target/" + base + "/nested/data", "target/" + base + "/keep"}, want: []string{"target/" + base + "/nested/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			writeTree(t, root, tc.files...)
			srcs := tc.srcs
			if srcs == nil {
				srcs = []string{base}
			}
			var abs []string
			for _, s := range srcs {
				abs = append(abs, filepath.Join(root, s))
			}
			for _, f := range tc.home {
				p := filepath.Join(home, f)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("old"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(t.TempDir(), "payload.tar")
			if err := writeArchive(archive, abs, io.Discard); err != nil {
				t.Fatal(err)
			}
			b := &localCopyBackend{home: home}
			var stderr bytes.Buffer
			err := copyArchive(context.Background(), b, "vm", archive, srcs, tc.dst, tc.opts, &stderr)
			if _, err := os.Stat(b.stage); !os.IsNotExist(err) {
				t.Fatalf("staging directory remains: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, "INJECTED")); !os.IsNotExist(err) {
				t.Fatal("destination executed as shell code")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.HasPrefix(err.Error(), "vm: ") {
					t.Fatalf("error = %v, want machine-prefixed %q", err, tc.wantErr)
				}
				for _, f := range tc.home { // a refusal wrote nothing
					if data, _ := os.ReadFile(filepath.Join(home, f)); string(data) != "old" {
						t.Fatalf("%s changed on a refused copy: %q", f, data)
					}
				}
				for _, p := range tc.absent {
					if _, err := os.Lstat(filepath.Join(home, p)); !os.IsNotExist(err) {
						t.Fatalf("%s exists after a refused copy", p)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("copy: %v (stderr %q)", err, stderr.String())
			}
			for _, f := range tc.want {
				data, err := os.ReadFile(filepath.Join(home, f))
				if err != nil || string(data) != payload {
					t.Fatalf("%s = %q, %v", f, data, err)
				}
			}
			if tc.name == "force merges" {
				if data, _ := os.ReadFile(filepath.Join(home, "target", base, "keep")); string(data) != "old" {
					t.Fatalf("merge clobbered an unrelated file: %q", data)
				}
			}
		})
	}
}

// -f must replace a destination symlink, never write through it: the host
// side refuses that for downloads, and the guest script keeps parity.
func TestCopyForceReplacesDestinationSymlink(t *testing.T) {
	root, home, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	const base = "note.txt"
	writeTree(t, root, base)
	victim := filepath.Join(elsewhere, "victim")
	if err := os.WriteFile(victim, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(home, base)); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "payload.tar")
	if err := writeArchive(archive, []string{filepath.Join(root, base)}, io.Discard); err != nil {
		t.Fatal(err)
	}
	b := &localCopyBackend{home: home}
	if err := copyArchive(context.Background(), b, "vm", archive, []string{base}, base, copyOpts{force: true}, io.Discard); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "old" {
		t.Fatalf("-f wrote through the destination symlink: %q", data)
	}
	if fi, err := os.Lstat(filepath.Join(home, base)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination is still a symlink (or missing): %v %v", fi, err)
	}
}

func TestCopyUploadFailureCleansStage(t *testing.T) {
	b := &localCopyBackend{home: t.TempDir(), failUpload: true}
	if err := copyArchive(context.Background(), b, "vm", "unused", []string{"file"}, "dest", copyOpts{}, io.Discard); err == nil {
		t.Fatal("expected upload failure")
	}
	if _, err := os.Stat(b.stage); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

func TestCopyInSourceChecks(t *testing.T) {
	a := &App{Stderr: io.Discard}
	root := t.TempDir()
	writeTree(t, root, "a/file.txt", "b/file.txt", "dir/inner")
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		srcs []string
		opts copyOpts
		want string
	}{
		{"directory needs -r", []string{filepath.Join(root, "dir")}, copyOpts{asDir: true}, "use -r"},
		{"basename collision", []string{filepath.Join(root, "a/file.txt"), filepath.Join(root, "b/file.txt")}, copyOpts{asDir: true}, "would both be copied as file.txt"},
		{"several need directory", []string{filepath.Join(root, "a/file.txt"), filepath.Join(root, "dir")}, copyOpts{recursive: true}, "destination directory"},
		{"missing source", []string{filepath.Join(root, "nope")}, copyOpts{asDir: true}, "no such file"},
	} {
		err := a.runCopyIn(ctx, "unused", tc.srcs, "dest", tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestCopyArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		target  string
		srcs    []string
		dst     string
		asDir   bool
		wantErr bool
	}{
		{args: []string{"vm", "a"}, srcs: []string{"a"}, dst: "~", asDir: true},
		{args: []string{"vm", "a", "b"}, srcs: []string{"a"}, dst: "b"},
		{args: []string{"vm", "a", "b/"}, srcs: []string{"a"}, dst: "b/", asDir: true},
		{args: []string{"vm", "a", "b", "c"}, wantErr: true},
		{args: []string{"vm", "a", "b", "c"}, target: "d", srcs: []string{"a", "b", "c"}, dst: "d", asDir: true},
		{args: []string{"vm", "a", ""}, wantErr: true},
	} {
		name, srcs, dst, asDir, err := copyArgs(tc.args, tc.target, "~")
		if (err != nil) != tc.wantErr {
			t.Fatalf("%v -t %q: error %v", tc.args, tc.target, err)
		}
		if err != nil {
			continue
		}
		if name != "vm" || strings.Join(srcs, ",") != strings.Join(tc.srcs, ",") || dst != tc.dst || asDir != tc.asDir {
			t.Fatalf("%v -t %q = %s %v %q %v", tc.args, tc.target, name, srcs, dst, asDir)
		}
	}
}

func TestWriteArchivePreservesSymlinksAndModes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "tree/sub/file")
	if err := os.Symlink("sub/file", filepath.Join(root, "tree/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "tree/sub/file"), 0640); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar")
	if err := writeArchive(archive, []string{filepath.Join(root, "tree")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := extractArchive(archive, map[string]string{"tree": filepath.Join(out, "tree")}, false); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(out, "tree/link")); err != nil || target != "sub/file" {
		t.Fatalf("symlink = %q, %v", target, err)
	}
	info, err := os.Stat(filepath.Join(out, "tree/sub/file"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	data, _ := os.ReadFile(filepath.Join(out, "tree/sub/file"))
	if string(data) != payload {
		t.Fatalf("content = %q", data)
	}
	// The guest side extracts with the system tar; make sure it agrees.
	guest := t.TempDir()
	if out, err := exec.Command("tar", "-xf", archive, "-C", guest, "--no-same-owner").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	if target, err := os.Readlink(filepath.Join(guest, "tree/link")); err != nil || target != "sub/file" {
		t.Fatalf("guest symlink = %q, %v", target, err)
	}
}
