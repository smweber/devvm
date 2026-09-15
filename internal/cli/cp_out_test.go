package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCopyOut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		srcs     []string // guest paths
		files    []string // files created in the guest home
		dst      string   // relative to a host root; "" means the root itself (an existing directory)
		opts     copyOpts
		existing []string // host files pre-existing (holding "old"), relative to the host root
		want     []string // host files that must hold payload afterwards
		wantErr  string
	}{
		{name: "file", srcs: []string{"source"}, files: []string{"source"}, dst: "destination", want: []string{"destination"}},
		{name: "default destination", srcs: []string{"source"}, files: []string{"source"}, opts: copyOpts{asDir: true}, want: []string{"source"}},
		{name: "home path", srcs: []string{"~/source"}, files: []string{"source"}, dst: "destination", want: []string{"destination"}},
		{name: "quoted path", srcs: []string{"a ' $(touch INJECTED)"}, files: []string{"a ' $(touch INJECTED)"}, dst: "destination", want: []string{"destination"}},
		{name: "directory rename", srcs: []string{"source"}, files: []string{"source/data"}, dst: "destination", opts: copyOpts{recursive: true}, want: []string{"destination/data"}},
		{name: "directory trailing slash", srcs: []string{"source/"}, files: []string{"source/data"}, dst: "destination", opts: copyOpts{recursive: true}, want: []string{"destination/data"}},
		{name: "existing directory", srcs: []string{"source"}, files: []string{"source/data"}, dst: "", opts: copyOpts{recursive: true}, want: []string{"source/data"}},
		{name: "trailing slash creates directory", srcs: []string{"source"}, files: []string{"source"}, dst: "new/deep/", opts: copyOpts{asDir: true}, want: []string{"new/deep/source"}},
		{name: "several sources", srcs: []string{"a.txt", "sub/b.txt"}, files: []string{"a.txt", "sub/b.txt"}, dst: "inbox", opts: copyOpts{asDir: true}, want: []string{"inbox/a.txt", "inbox/b.txt"}},
		{name: "basename collision", srcs: []string{"a/f", "b/f"}, files: []string{"a/f", "b/f"}, dst: "inbox", opts: copyOpts{asDir: true}, wantErr: "would both be copied as f"},
		{name: "missing parent", srcs: []string{"source"}, files: []string{"source"}, dst: "missing/file", wantErr: "no such directory"},
		{name: "refuses overwrite", srcs: []string{"source"}, files: []string{"source"}, dst: "destination", existing: []string{"destination"}, wantErr: "refusing to overwrite (use -f):\n  "},
		{name: "refuses nested overwrite", srcs: []string{"source"}, files: []string{"source/data", "source/fresh"}, dst: "", opts: copyOpts{recursive: true},
			existing: []string{"source/data"}, wantErr: "source/data"},
		{name: "force overwrites", srcs: []string{"source"}, files: []string{"source"}, dst: "destination", opts: copyOpts{force: true}, existing: []string{"destination"}, want: []string{"destination"}},
		{name: "force merges", srcs: []string{"source"}, files: []string{"source/data"}, dst: "", opts: copyOpts{recursive: true, force: true},
			existing: []string{"source/data", "source/keep"}, want: []string{"source/data"}},
		{name: "directory needs flag", srcs: []string{"source"}, files: []string{"source/data"}, dst: "destination", wantErr: "use -r"},
		{name: "missing source", srcs: []string{"missing"}, dst: "destination", wantErr: "missing or has an unsupported"},
		{name: "root rejected", srcs: []string{"/"}, dst: "destination", opts: copyOpts{recursive: true}, wantErr: "filesystem root"},
		{name: "several need directory", srcs: []string{"a.txt", "b.txt"}, files: []string{"a.txt", "b.txt"}, dst: "destination", wantErr: "destination directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			writeTree(t, home, tc.files...)
			for _, f := range tc.existing {
				p := filepath.Join(root, f)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("old"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			dst := filepath.Join(root, tc.dst)
			if strings.HasSuffix(tc.dst, "/") {
				dst += "/"
			}
			var stderr bytes.Buffer
			err := copyOut(context.Background(), &localCopyBackend{home: home}, "vm", tc.srcs, dst, tc.opts, &stderr)
			if _, err := os.Stat(filepath.Join(home, "INJECTED")); !os.IsNotExist(err) {
				t.Fatal("source executed as code")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v; want %q", err, tc.wantErr)
				}
				for _, f := range tc.existing { // a refusal wrote nothing
					if data, _ := os.ReadFile(filepath.Join(root, f)); string(data) != "old" {
						t.Fatalf("%s changed on a refused copy: %q", f, data)
					}
				}
				if tc.dst != "" && len(tc.existing) == 0 {
					if _, err := os.Lstat(dst); !os.IsNotExist(err) {
						t.Fatal("failed download changed destination")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("copy: %v (stderr %q)", err, stderr.String())
			}
			for _, f := range tc.want {
				data, err := os.ReadFile(filepath.Join(root, f))
				if err != nil || string(data) != payload {
					t.Fatalf("%s = %q, %v", f, data, err)
				}
			}
			if tc.name == "force merges" {
				if data, _ := os.ReadFile(filepath.Join(root, "source/keep")); string(data) != "old" {
					t.Fatalf("merge clobbered an unrelated file: %q", data)
				}
			}
			if !strings.Contains(stderr.String(), "vm:"+strings.TrimSuffix(tc.srcs[0], "/")) {
				t.Fatalf("no per-file report in %q", stderr.String())
			}
		})
	}
}

func TestCopyOutDefaultIsWorkingDirectory(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeTree(t, home, "notes.txt")
	// go.mod's floor is 1.23, so no testing.Chdir.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if err := copyOut(context.Background(), &localCopyBackend{home: home}, "vm", []string{"notes.txt"}, ".", copyOpts{asDir: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(cwd, "notes.txt")); err != nil || string(data) != payload {
		t.Fatalf("download = %q, %v", data, err)
	}
}

func TestCopyCommandNamesAndCompletion(t *testing.T) {
	a := &App{ConfigDir: t.TempDir()}
	root := a.rootCmd()
	// Without -t: slot 2 and 3 alternate local/guest; nothing completes past
	// them. With -t, every slot after NAME is a source of the same kind. An
	// unknown machine must never fall back to local completion for a guest slot.
	const local, guest, none = cobra.ShellCompDirectiveDefault, cobra.ShellCompDirectiveNoFileComp, cobra.ShellCompDirectiveNoFileComp
	for _, tc := range []struct {
		cmd    string
		target bool
		want   []cobra.ShellCompDirective // for slots after NAME
	}{
		{"cp-in", false, []cobra.ShellCompDirective{local, guest, none}},
		{"cp-in", true, []cobra.ShellCompDirective{local, local, local}},
		{"cp-out", false, []cobra.ShellCompDirective{guest, local, none}},
		{"cp-out", true, []cobra.ShellCompDirective{guest, guest, guest}},
	} {
		cmd, _, err := root.Find([]string{tc.cmd})
		if err != nil || cmd.Name() != tc.cmd {
			t.Fatalf("missing %s: %v", tc.cmd, err)
		}
		if tc.target {
			if err := cmd.Flags().Set("target-directory", "dir"); err != nil {
				t.Fatal(err)
			}
		} else {
			cmd.Flags().Set("target-directory", "")
			cmd.Flags().Lookup("target-directory").Changed = false
		}
		args := []string{"vm"}
		for slot, want := range tc.want {
			_, directive := cmd.ValidArgsFunction(cmd, args, "")
			if directive != want {
				t.Errorf("%s -t=%v slot %d: directive %v, want %v", tc.cmd, tc.target, slot+2, directive, want)
			}
			args = append(args, "arg")
		}
	}
	if cmd, _, _ := root.Find([]string{"cp"}); cmd != root {
		t.Fatal("old cp command still registered")
	}
	// The -t value completes guest paths for cp-in (once NAME is known) and
	// host directories for cp-out.
	cpIn, _, _ := root.Find([]string{"cp-in"})
	if fn, ok := cpIn.GetFlagCompletionFunc("target-directory"); !ok {
		t.Fatal("cp-in -t has no completion")
	} else if _, d := fn(cpIn, nil, ""); d != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("cp-in -t before NAME: %v", d)
	}
	cpOut, _, _ := root.Find([]string{"cp-out"})
	if fn, ok := cpOut.GetFlagCompletionFunc("target-directory"); !ok {
		t.Fatal("cp-out -t has no completion")
	} else if _, d := fn(cpOut, []string{"vm"}, ""); d != cobra.ShellCompDirectiveFilterDirs {
		t.Fatalf("cp-out -t: %v", d)
	}
}
