package cli

import (
	"errors"
	"os"

	"github.com/charmbracelet/huh"
)

// No test in this package may draw a real form: over the plain file the
// tests hand in as a terminal, huh fails on Linux but blocks for good on
// macOS (kqueue), which cost a CI run its whole ten minutes. Every prompt a
// test answers has its own stub; anything that falls through fails here.
func init() {
	form = func(*os.File, ...huh.Field) error { return errors.New("huh: no form in a test") }
}
