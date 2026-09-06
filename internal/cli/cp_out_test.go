package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestCopyOut(t *testing.T) {
	for _, tc := range []struct {
		name, src                            string
		directory, recursive, existing, fail bool
	}{
		{name: "file", src: "source"},
		{name: "home path", src: "~/source"},
		{name: "quoted path", src: "a ' $(touch INJECTED)"},
		{name: "directory rename", src: "source", directory: true, recursive: true},
		{name: "directory trailing slash", src: "source/", directory: true, recursive: true},
		{name: "existing directory", src: "source", directory: true, recursive: true, existing: true},
		{name: "directory needs flag", src: "source", directory: true, fail: true},
		{name: "missing source", src: "missing", fail: true},
		{name: "root rejected", src: "/", recursive: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dest := t.TempDir(), filepath.Join(t.TempDir(), "destination")
			base := "source"
			if tc.name == "quoted path" {
				base = tc.src
			}
			file := filepath.Join(home, base)
			if tc.directory {
				if err := os.Mkdir(file, 0755); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(file, "data")
			}
			if err := os.WriteFile(file, []byte("binary\x00\xff"), 0755); err != nil {
				t.Fatal(err)
			}
			if tc.existing {
				if err := os.Mkdir(dest, 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := copyOut(context.Background(), &localCopyBackend{home: home}, tc.src, dest, tc.recursive, io.Discard)
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v; want failure %v", err, tc.fail)
			}
			if tc.fail {
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failed download changed destination")
				}
				return
			}
			want := dest
			if tc.existing {
				want = filepath.Join(want, base)
			}
			if tc.directory {
				want = filepath.Join(want, "data")
			}
			data, err := os.ReadFile(want)
			if err != nil || string(data) != "binary\x00\xff" {
				t.Fatalf("download = %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(home, "INJECTED")); !os.IsNotExist(err) {
				t.Fatal("source executed as code")
			}
		})
	}
}

func TestCopyCommandNamesAndCompletion(t *testing.T) {
	a := &App{ConfigDir: t.TempDir()}
	root := a.rootCmd()
	for _, name := range []string{"cp-in", "cp-out"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("missing %s: %v", name, err)
		}
		localArgs := []string{"vm"}
		guestArgs := []string{"vm", "source"}
		if name == "cp-out" {
			localArgs, guestArgs = guestArgs, localArgs
		}
		_, directive := cmd.ValidArgsFunction(cmd, localArgs, "")
		if directive != cobra.ShellCompDirectiveDefault {
			t.Fatal("local path completion disabled")
		}
		_, directive = cmd.ValidArgsFunction(cmd, guestArgs, "")
		if directive != cobra.ShellCompDirectiveNoFileComp {
			t.Fatal("unknown guest fell back to local completion")
		}
	}
	if cmd, _, _ := root.Find([]string{"cp"}); cmd != root {
		t.Fatal("old cp command still registered")
	}
}
