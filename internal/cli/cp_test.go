package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd.Stdout, cmd.Stderr = o.Stdout, o.Stderr
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

func TestCopyArchive(t *testing.T) {
	for _, tc := range []struct {
		name, dst, want           string
		directory, existing, fail bool
	}{
		{name: "relative file", dst: "renamed.txt", want: "renamed.txt"},
		{name: "quoted path", dst: "a ' $(touch INJECTED).txt", want: "a ' $(touch INJECTED).txt"},
		{name: "home expansion", dst: "~/renamed.txt", want: "renamed.txt"},
		{name: "existing directory", dst: "target", want: "target/source ' file", existing: true},
		{name: "directory rename", dst: "target", want: "target/nested/data", directory: true},
		{name: "directory into directory", dst: "target", want: "target/source ' file/nested/data", directory: true, existing: true},
		{name: "destination failure", dst: "missing/file", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			base := "source ' file"
			src := filepath.Join(root, base)
			if tc.directory {
				if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
					t.Fatal(err)
				}
				src = filepath.Join(src, "nested/data")
			}
			if err := os.WriteFile(src, []byte("payload\x00\xff"), 0751); err != nil {
				t.Fatal(err)
			}
			if tc.existing {
				if err := os.Mkdir(filepath.Join(home, "target"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(t.TempDir(), "payload.tar")
			if out, err := exec.Command("tar", "-cf", archive, "-C", root, "--", base).CombinedOutput(); err != nil {
				t.Fatalf("tar: %v: %s", err, out)
			}
			b := &localCopyBackend{home: home}
			err := copyArchive(context.Background(), b, archive, base, tc.dst, io.Discard)
			if (err != nil) != tc.fail {
				t.Fatalf("copy error = %v, want failure %v", err, tc.fail)
			}
			if _, err := os.Stat(b.stage); !os.IsNotExist(err) {
				t.Fatalf("staging directory remains: %v", err)
			}
			if tc.fail {
				return
			}
			data, err := os.ReadFile(filepath.Join(home, tc.want))
			if err != nil || string(data) != "payload\x00\xff" {
				t.Fatalf("destination = %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(home, "INJECTED")); !os.IsNotExist(err) {
				t.Fatal("destination executed as shell code")
			}
		})
	}
}

func TestCopyUploadFailureCleansStage(t *testing.T) {
	b := &localCopyBackend{home: t.TempDir(), failUpload: true}
	if err := copyArchive(context.Background(), b, "unused", "file", "dest", io.Discard); err == nil {
		t.Fatal("expected upload failure")
	}
	if _, err := os.Stat(b.stage); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

func TestCopyRejectsDirectoryWithoutRecursive(t *testing.T) {
	a := &App{}
	if err := a.runCopy(context.Background(), "unused", t.TempDir(), "dest", false); err == nil {
		t.Fatal("expected directory rejection")
	}
}
