package cli

// versionLine is the one-line string printed by `kubeagent version`.
func versionLine() string {
	return "kubeagent " + version
}
