// Package dashboard renders the watch daemon's tracked state as one
// self-contained HTML page: the URL you hand someone who asks what is broken
// right now.
//
// The document is deliberately inert. It carries no <script>, no external
// stylesheet, font, or image, so it survives a strict Content-Security-Policy
// and performs no third-party fetch. The only dynamic behaviour is a
// <meta http-equiv="refresh"> carrying an interval and no URL — so the page
// emits no URL at all.
//
// The package imports nothing from kubeagent. That is a security property, not
// a style choice: it makes reaching internal/remediate or internal/explain
// structurally impossible rather than a rule someone has to remember, and
// imports_test.go enforces it. It is also why the view types below are defined
// here rather than reused from internal/watch, which is the caller.
//
// Render performs no cluster call and makes no model call. Those are two
// separate promises: the daemon's --explain feature does call a model, from
// the incident pipeline, never from an HTTP handler.
package dashboard

import (
	_ "embed"
	"html/template"
)

// RefreshSeconds is how often the page reloads itself. It is a constant with no
// flag behind it: a flag would be a stable surface forever, and the value buys
// nothing tunable — informers detect in roughly two seconds and the heartbeat
// is sixty, so thirty already sits between them.
const RefreshSeconds = 30

//go:embed dashboard.html.tmpl
var templateSource string

// tmpl is parsed once at package init so a malformed template fails in CI, not
// in front of an operator mid-incident. This is the same choice
// internal/htmlreport makes.
//
// html/template, never text/template: the contextual auto-escaping is this
// package's single escape boundary. Issue text, cluster names and model output
// are all free-form strings that land verbatim in a page a browser renders.
var tmpl = template.Must(template.New("dashboard").Parse(templateSource))
