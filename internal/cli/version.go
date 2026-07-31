package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// versionLine is the one-line string printed by `kubeagent version`.
func versionLine() string {
	return "kubeagent " + version
}

// newVersionCommand builds `kubeagent version`.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		Short:         "Print the version",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stdout, versionLine())
			return nil
		},
	}
}
