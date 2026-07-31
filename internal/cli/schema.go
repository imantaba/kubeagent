package cli

import (
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/schemadoc"
)

// runSchema prints the JSON Schema for one machine-readable document, or lists
// them all. Generated at runtime from the same code path that writes the
// committed files, so what the binary prints is what the binary's types are —
// there is no embedded copy that could drift.
//
// Read-only in the strongest sense: it reads Go types. No cluster connection, no
// kubeconfig, no LLM call.
func runSchema(args []string, w io.Writer) error {
	if len(args) == 0 {
		for _, d := range schemadoc.Documents {
			fmt.Fprintf(w, "  %-19s surface %-6s v%s\n", d.Name, d.Surface, d.Version)
		}
		fmt.Fprintf(w, "\nPrint one:\n  %s schema <name>\n", invokedAs)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: %s schema [name]", invokedAs)
	}
	doc, err := schemadoc.Generate(args[0])
	if err != nil {
		return err
	}
	_, err = w.Write(doc)
	return err
}
