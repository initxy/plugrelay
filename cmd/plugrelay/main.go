// Command plugrelay is the plugrelay LLM relay and its admin CLI. Run
// "plugrelay --help" for the commands.
package main

import (
	"context"
	"os"

	"github.com/initxy/plugrelay/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.DefaultEnv()))
}
