// Command tag is the native Go TAG CLI entrypoint.
package main

import (
	"os"

	"github.com/tag-agent/tag/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
