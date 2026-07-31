package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newCompletionCommand prints a shell completion script. It is the one
// kubeagent command that touches nothing: no cluster client, no kubeconfig,
// no model call. Cobra generates the scripts from the command tree, so they
// stay correct as flags change.
//
// Registering this command on root preempts Cobra's own default 'completion'
// command (InitDefaultCompletionCmd skips creating one once a command named
// "completion" already exists), so the shape here — one flat verb taking a
// single shell name — is what actually runs, not Cobra's nested
// completion/bash, completion/zsh subcommand tree.
func newCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:                   "completion bash|zsh|fish|powershell",
		Short:                 "Print a shell completion script",
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		DisableFlagsInUseLine: true,
		SilenceErrors:         true,
		SilenceUsage:          true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			}
			return fmt.Errorf("unknown shell %q: want bash, zsh, fish or powershell", args[0])
		},
	}
}
