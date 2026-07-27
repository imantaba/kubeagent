package gitops

import (
	"regexp"
	"strings"
)

// revisionWithheld is what a revision renders as when it is not a bare commit SHA.
const revisionWithheld = "(revision withheld)"

// hexRevision matches a git object name and nothing else. Uppercase is excluded:
// git writes lowercase, and accepting anything wider widens the leak surface.
var hexRevision = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ShortRevision reduces a reconciler-reported revision to a short commit SHA, or
// withholds it entirely. It is the only path by which revision-derived text
// reaches the report.
//
// Flux publishes revisions as "<ref>@sha1:<hash>", where <ref> is arbitrary user
// text — a branch name, a tag, sometimes a path. Argo CD reports a bare SHA, but
// the same field has held tags and chart versions. Anything that is not a plain
// lowercase hex SHA of 7-40 characters is withheld rather than guessed at.
func ShortRevision(raw string) string {
	s := raw
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if !hexRevision.MatchString(s) {
		return revisionWithheld
	}
	return s[:7]
}
