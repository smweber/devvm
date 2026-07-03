// Command devvm is the host-side CLI for managing persistent dev boxes across
// the smol and ssh backends. See internal/cli for the command tree.
package main

import (
	"os"

	"github.com/smweber/devvm/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
