// Command fam is the Fleet Agent Manager.
package main

import (
	"os"

	"github.com/gregor-tokarev/fam/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
