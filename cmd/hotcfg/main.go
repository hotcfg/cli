// Command hotcfg is the hotcfg.run command-line tool.
//
//	go install github.com/hotcfg/cli/cmd/hotcfg@latest
package main

import (
	"context"
	"os"
	"runtime/debug"
	"strings"

	"github.com/hotcfg/cli"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
// Release builds (GoReleaser) set it; for "go install ...@v1.2.3" it is
// read from the module version recorded in the binary.
var version = "dev"

func main() {
	os.Exit(cli.NewApp(buildVersion()).Run(context.Background(), os.Args[1:]))
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return version
}
