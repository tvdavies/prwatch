// Command prwatch is one shared GitHub pull request poller per user per
// machine, with a blocking wait command for agents and scripts.
package main

import (
	"os"

	"github.com/tvdavies/prwatch/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() { os.Exit(cli.Main(os.Args[1:], version)) }
