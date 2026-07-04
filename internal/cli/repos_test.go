package cli

import "testing"

func TestNormalizeRepo(t *testing.T) {
	cases := map[string]string{
		// GitHub URLs fold down to owner/repo shorthand.
		"https://github.com/foo/bar":         "foo/bar",
		"https://github.com/foo/bar.git":     "foo/bar",
		"git@github.com:foo/bar.git":         "foo/bar",
		"ssh://git@github.com/foo/bar.git":   "foo/bar",
		"  https://github.com/foo/bar.git  ": "foo/bar",
		// Already-shorthand stays put.
		"foo/bar": "foo/bar",
		// Non-GitHub remotes are preserved verbatim.
		"ssh://git@gitlab.com/x/y.git": "ssh://git@gitlab.com/x/y.git",
		"https://gitlab.com/a/b/c.git": "https://gitlab.com/a/b/c.git",
		"git@bitbucket.org:team/r.git": "git@bitbucket.org:team/r.git",
	}
	for in, want := range cases {
		if got := normalizeRepo(in); got != want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
