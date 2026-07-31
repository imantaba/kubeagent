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
// lookup reports two things about a candidate long-flag name: whether the
// target command registers it at all (registered), and, only when it does,
// whether it consumes the next argument as its value (takesValue) — false
// for a boolean or count flag, which pflag lets stand alone with no
// following token. Normalize only treats the token after a rewritten flag as
// that flag's value when takesValue is true; a boolean or count flag written
// without = is rewritten in place and the very next token is then free to be
// read as the start of a new flag, exactly as it would be after any other
// flag boundary. This holds symmetrically whether the flag being rewritten
// was itself written with one dash or two — a boolean given as --explain
// must not swallow a single-dash flag that follows it any more than -explain
// does.
//
// It returns a new slice; the input is not modified.
func Normalize(args []string, lookup func(name string) (registered, takesValue bool)) []string {
	if args == nil {
		return nil
	}
	out := make([]string, 0, len(args))
	expectValue := false
	for _, a := range args {
		switch {
		case expectValue:
			// The previous element was a long flag that takes a value and was
			// written without =, so this is its value however much it looks
			// like a flag.
			expectValue = false
		case a == "--":
			out = append(out, args[len(out):]...)
			return out
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			name, rest, hasEq := strings.Cut(a[1:], "=")
			if registered, takesValue := lookup(name); registered {
				if hasEq {
					a = "--" + name + "=" + rest
				} else {
					a = "--" + name
					expectValue = takesValue
				}
			}
		case strings.HasPrefix(a, "--"):
			name, _, hasEq := strings.Cut(a[2:], "=")
			registered, takesValue := lookup(name)
			expectValue = !hasEq && registered && takesValue
		}
		out = append(out, a)
	}
	return out
}
