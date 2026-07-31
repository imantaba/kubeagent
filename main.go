// Command kubeagent is a read-only Kubernetes troubleshooting CLI.
//
// The command tree lives in internal/cli. This file exists to own the
// version symbol, because the release workflow stamps it with
// -ldflags "-X main.version=<tag>" and that target must not move.
package main

import (
	"os"

	"github.com/imantaba/kubeagent/internal/cli"
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=<tag>". Local/dev builds report "dev".
var version = "dev"

func main() { os.Exit(cli.Main(version)) }
