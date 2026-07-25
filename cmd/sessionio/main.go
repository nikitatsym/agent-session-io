package main

import (
	"context"
	"io"
	"log"
	"os"

	"charm.land/fang/v2"
	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/nikitatsym/agent-session-io/internal/cli"
)

func main() {
	info := buildinfo.Current()
	root := cli.NewRoot(info)
	err := fang.Execute(
		context.Background(),
		root,
		fang.WithVersion(info.Version),
		fang.WithCommit(info.Commit),
		fang.WithNotifySignal(os.Interrupt),
		fang.WithErrorHandler(deferErrorToMain),
	)
	if err != nil {
		reportError(err)
		os.Exit(cli.ExitCode(err))
	}
}

func reportError(err error) {
	if cli.ErrorReported(err) {
		// The command already wrote its machine error record to stdout.
		return
	}
	log.SetFlags(0)
	log.Print(err)
}

// Fang returns this error to main, where reportError prints it once.
func deferErrorToMain(io.Writer, fang.Styles, error) {}
