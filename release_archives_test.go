package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// These tests execute scripts/build-release-archives.sh itself. The script is
// what the release workflow runs, so the script is what must be tested: a Go
// reimplementation of its tar invocation would keep passing while the real
// script rotted.

const archiveTestVersion = "v9.9.9"

// A fixed epoch, so the test asserts an exact mtime rather than "whatever the
// build happened to stamp". 2023-11-14T22:13:20Z.
const archiveTestEpoch = 1700000000

// buildArchives runs the real script into outdir for two platforms. Two is
// enough to exercise the cross-platform loop while keeping a double build
// fast; linux/amd64 must be present because the script also produces the
// unversioned copy from it.
func buildArchives(t *testing.T, outdir string) {
	t.Helper()
	cmd := exec.Command("scripts/build-release-archives.sh", archiveTestVersion, outdir)
	cmd.Env = append(os.Environ(),
		"SOURCE_DATE_EPOCH="+strconv.Itoa(archiveTestEpoch),
		"RELEASE_PLATFORMS=linux/amd64 linux/arm64",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-release-archives.sh: %v\n%s", err, out)
	}
}

func TestReleaseArchives(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	buildArchives(t, dirA)
	buildArchives(t, dirB)

	// Same tag, same bytes. Without this a verifier who rebuilds the release
	// gets a mismatch and cannot tell tampering from timestamps.
	t.Run("checksums are identical across builds", func(t *testing.T) {
		sumsA := readArchiveFile(t, filepath.Join(dirA, "SHA256SUMS"))
		sumsB := readArchiveFile(t, filepath.Join(dirB, "SHA256SUMS"))
		if string(sumsA) != string(sumsB) {
			t.Errorf("SHA256SUMS differs between two builds of the same tree:\nfirst:\n%s\nsecond:\n%s", sumsA, sumsB)
		}
	})

	archive := filepath.Join(dirA, "kubeagent_"+archiveTestVersion+"_linux_amd64.tar.gz")

	// Two builds on one machine agree even when every entry carries
	// uid=1000 uname=ubuntu — the archive would still be unreproducible for
	// everyone else. These assertions are what actually pin that down.
	t.Run("tar entries carry no builder identity or clock", func(t *testing.T) {
		want := time.Unix(archiveTestEpoch, 0)
		for _, h := range tarHeaders(t, archive) {
			if h.Uid != 0 || h.Gid != 0 {
				t.Errorf("%s: uid/gid = %d/%d, want 0/0", h.Name, h.Uid, h.Gid)
			}
			if h.Uname != "" || h.Gname != "" {
				t.Errorf("%s: uname/gname = %q/%q, want empty", h.Name, h.Uname, h.Gname)
			}
			if !h.ModTime.Equal(want) {
				t.Errorf("%s: mtime = %s, want %s (SOURCE_DATE_EPOCH)", h.Name, h.ModTime.UTC(), want.UTC())
			}
		}
	})

	t.Run("entries are sorted and complete", func(t *testing.T) {
		var names []string
		for _, h := range tarHeaders(t, archive) {
			names = append(names, h.Name)
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("entries are not in sorted order: %v", names)
		}
		// Apache-2.0 section 4(d): NOTICE travels with the redistribution.
		for _, want := range []string{"kubeagent", "README.md", "LICENSE", "NOTICE"} {
			if !containsString(names, want) {
				t.Errorf("archive is missing %s; entries: %v", want, names)
			}
		}
	})

	// tar -czf lets gzip stamp its own header. gzip -n does not.
	t.Run("gzip header carries no timestamp", func(t *testing.T) {
		head := make([]byte, 10)
		f, err := os.Open(archive)
		if err != nil {
			t.Fatalf("open archive: %v", err)
		}
		defer f.Close()
		if _, err := io.ReadFull(f, head); err != nil {
			t.Fatalf("read gzip header: %v", err)
		}
		if mtime := binary.LittleEndian.Uint32(head[4:8]); mtime != 0 {
			t.Errorf("gzip header MTIME = %d, want 0 (gzip -n)", mtime)
		}
		const flagFNAME = 0x08
		if head[3]&flagFNAME != 0 {
			t.Errorf("gzip header FLG = %#x, want the FNAME bit clear (gzip -n)", head[3])
		}
	})
}

func readArchiveFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func tarHeaders(t *testing.T, archive string) []*tar.Header {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open %s: %v", archive, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()

	var headers []*tar.Header
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		headers = append(headers, h)
	}
	return headers
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
