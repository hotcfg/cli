// Command hotcfg is the hotcfg.run command-line tool.
//
//	go install github.com/hotcfg/cli/cmd/hotcfg@latest
package main

import (
	"context"
	"os"

	"github.com/hotcfg/cli"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.NewApp(version).Run(context.Background(), os.Args[1:]))
}
