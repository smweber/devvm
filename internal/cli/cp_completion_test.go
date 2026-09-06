package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func TestGuestPathCompletions(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{"project", "space dir", "literal[dir]", "$(touch INJECTED)"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir, "notes.txt"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"file.txt", ".hidden", "bad\nname"} {
		if err := os.WriteFile(filepath.Join(home, file), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		prefix  string
		want    []string
		noSpace bool
	}{
		{"pro", []string{"project/"}, true},
		{"project/", []string{"project/notes.txt"}, false},
		{"space dir/n", []string{"space dir/notes.txt"}, false},
		{"literal[dir]/", []string{"literal[dir]/notes.txt"}, false},
		{"$(touch INJECTED)/", []string{"$(touch INJECTED)/notes.txt"}, false},
		{"~/pro", []string{"~/project/"}, true},
		{"~", []string{"~/"}, true},
		{"./pro", []string{"./project/"}, true},
		{home + "/pro", []string{home + "/project/"}, true},
		{".h", []string{".hidden"}, false},
		{"file", []string{"file.txt"}, false},
		{"missing/", nil, false},
		{"bad", nil, false},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			got, directive := guestPathCompletions(context.Background(), &localCopyBackend{home: home}, tc.prefix)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
				t.Fatal("local completion enabled")
			}
			if (directive&cobra.ShellCompDirectiveNoSpace != 0) != tc.noSpace {
				t.Fatalf("unexpected spacing directive %v", directive)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(home, "INJECTED")); !os.IsNotExist(err) {
		t.Fatal("path executed as code")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, directive := guestPathCompletions(ctx, &localCopyBackend{home: home}, "")
	if len(got) != 0 || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("cancelled completion = %v, %v", got, directive)
	}
}
