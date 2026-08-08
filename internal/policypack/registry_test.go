package policypack

import (
	"io/fs"
	"testing"
)

// TestEveryEmbeddedPackIsRegistered closes the one gap no per-pack test can
// see.
//
// `//go:embed packs/*.yaml` compiles every file under packs/ into the binary,
// but every other test over the packs — here and in package policypack_test —
// iterates All(). A file with no registry entry is therefore invisible to all
// of them while still travelling in every release artifact: its rules are
// never loaded, never validated and never run, and nothing reports it.
//
// That is the mistake a contributed pack is most likely to make — adding the
// YAML and forgetting the five-line registry entry — which is why it is
// checked here rather than left to review.
//
// Only this direction needs checking. The reverse, a registry entry naming a
// file that is not embedded, already fails TestEveryPackLoads in
// packs_test.go, where Bytes returns false.
func TestEveryEmbeddedPackIsRegistered(t *testing.T) {
	entries, err := fs.ReadDir(files, "packs")
	if err != nil {
		t.Fatalf("reading the embedded packs directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded packs — every assertion below would pass vacuously")
	}

	// Built from the registry, keyed by embedded path, so the same file named
	// by two entries is caught too: one pack's bytes would then ship under two
	// names, and the second name's rule ids would carry the first pack's
	// prefix.
	registered := make(map[string]string, len(packs))
	for _, p := range packs {
		if other, dup := registered[p.file]; dup {
			t.Errorf("packs %q and %q both name %s — one file cannot be two packs", other, p.Name, p.file)
		}
		registered[p.file] = p.Name
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := "packs/" + e.Name()
		if _, ok := registered[path]; !ok {
			t.Errorf("%s is embedded in the binary but no registry entry names it — it ships unlisted, unloaded and untested", path)
		}
	}
}
