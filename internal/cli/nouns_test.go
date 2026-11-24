package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A noun refuses a word it does not know, in one line naming its commands,
// so a script calling a removed leaf (`keys dedupe`) fails; a bare noun and
// --help still print help and succeed, and completion still lists leaves.
func TestNounUnknownSubcommand(t *testing.T) {
	a := newTestApp(t)
	for noun, leaves := range map[string]string{
		"keys":      "add, list, rm",
		"ports":     "add, rm, list, up, down",
		"config":    "path, show, edit, check, add, rm",
		"providers": "token, types",
	} {
		err := runTree(t, a, noun, "dedupe")
		if err == nil || !strings.Contains(err.Error(), `unknown command "dedupe" for "devvm `+noun+`"`) ||
			!strings.Contains(err.Error(), leaves) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s dedupe: err = %v, want one line naming %s", noun, err, leaves)
		}
		for _, argv := range [][]string{{noun}, {noun, "--help"}} {
			if err := runTree(t, a, argv...); err != nil {
				t.Errorf("%q: err = %v, want help", argv, err)
			}
			out := a.Stdout.(*bytes.Buffer).String()
			if !strings.Contains(out, "devvm "+noun+" [command]") || strings.Contains(out, "devvm "+noun+" [flags]") {
				t.Errorf("%q: usage = %q, want only the [command] line", argv, out)
			}
		}
		if err := runTree(t, a, "__complete", noun, ""); err != nil {
			t.Errorf("__complete %s: %v", noun, err)
		}
		if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, strings.Split(leaves, ", ")[0]+"\t") {
			t.Errorf("__complete %s = %q, want its leaves", noun, out)
		}
	}
	// A leaf's own usage is untouched.
	if err := runTree(t, a, "keys", "rm", "--help"); err != nil || !strings.Contains(a.Stdout.(*bytes.Buffer).String(), "devvm keys rm NAME PATTERN [flags]") {
		t.Errorf("keys rm --help: err = %v, out = %q", err, a.Stdout)
	}
}
