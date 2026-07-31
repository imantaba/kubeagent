package cli

import "strings"

// Normalize rewrites a leading single-dash long flag to the double-dash form
// pflag requires, preserving the standard library's parsing for command lines
// written against every kubeagent release before this one. The standard flag
// package treats -flag and --flag alike; pflag reads a single dash as a
// shorthand cluster, so -kubeconfig would parse as -k -u -b -e -c … and fail.
//
// It rewrites only names the target command actually registers as long flags,
// which is what makes it safe: an unregistered -xyz is left alone so pflag
// reports it in pflag's own words rather than kubeagent silently inventing a
// flag. Registered shorthands (-n, -h), a bare -, anything after a bare --,
// and any argument in a value position are all left alone.
//
// It returns a new slice; the input is not modified.
func Normalize(args []string, isLongFlag func(string) bool) []string {
	if args == nil {
		return nil
	}
	out := make([]string, 0, len(args))
	expectValue := false
	for _, a := range args {
		switch {
		case expectValue:
			// The previous element was a long flag with no =, so this is its
			// value however much it looks like a flag.
			expectValue = false
		case a == "--":
			out = append(out, args[len(out):]...)
			return out
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			name, rest, hasEq := strings.Cut(a[1:], "=")
			if isLongFlag(name) {
				if hasEq {
					a = "--" + name + "=" + rest
				} else {
					a = "--" + name
					expectValue = true
				}
			}
		case strings.HasPrefix(a, "--"):
			name, _, hasEq := strings.Cut(a[2:], "=")
			expectValue = !hasEq && isLongFlag(name)
		}
		out = append(out, a)
	}
	return out
}
