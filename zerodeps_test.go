package teamdating

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestZeroDependencies enforces DD-13 inside `go test ./...`.
//
// ★ Why this lives in the test binary and not only in scripts/check-zero-deps.sh:
//
//	a gate nobody runs is not a gate. The shell script is the CI / pre-commit
//	entry point, but the ONE path this project is guaranteed to walk before a
//	commit is `go test ./...`. A dependency creeping in produces no error of its
//	own — the build stays green, every test passes, and go.mod quietly grows a
//	require line. So the check has to sit where it cannot be skipped.
//
// ★ Note also what would have made this test worthless: asserting on a command
//
//	whose failure and whose success share an output. `go list -m all | wc -l`
//	is exactly that shape — the pipeline's status belongs to wc, so a go list
//	that blows up still reports a plausible count. Every check below inspects
//	the error value itself before reading the output.
func TestZeroDependencies(t *testing.T) {
	goBin := findGo(t)

	// 1. The module graph must contain only this module.
	out, err := exec.Command(goBin, "list", "-m", "all").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -m all failed: %v\n%s", err, out)
	}
	mods := nonEmptyLines(string(out))
	if len(mods) != 1 {
		t.Errorf("module graph holds %d modules, want only this one:\n%s", len(mods), strings.Join(mods, "\n"))
	}

	// 2. go.mod must carry no require directive.
	gomod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for i, line := range strings.Split(string(gomod), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "require") {
			t.Errorf("go.mod:%d has a require directive: %s", i+1, strings.TrimSpace(line))
		}
	}

	// 3. A non-empty go.sum means an external module was fetched at some point.
	if info, err := os.Stat("go.sum"); err == nil && info.Size() > 0 {
		t.Errorf("go.sum is non-empty (%d bytes) — an external module was pulled in", info.Size())
	}

	// 4. The declared surface (1-3) can be clean while the code still imports
	//    something external via vendor/ or a replace. Check the real dep graph.
	out, err = exec.Command(goBin, "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./... failed: %v\n%s", err, out)
	}
	var external []string
	for _, pkg := range nonEmptyLines(string(out)) {
		if pkg == "teamdating" || strings.HasPrefix(pkg, "teamdating/") {
			continue
		}
		// Standard-library import paths have no dot in their first segment;
		// a module path always starts with a domain, which does.
		if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
			external = append(external, pkg)
		}
	}
	if len(external) > 0 {
		t.Errorf("dependency graph reaches non-stdlib packages:\n%s", strings.Join(external, "\n"))
	}
}

// findGo locates the toolchain. An empty `command -v go` has two causes —
// absent, or present-but-not-on-PATH — and they share one output, so PATH
// missing it is not evidence of absence. On this host Go 1.24 sits in
// /usr/local/go/bin, which is not on PATH by default.
func findGo(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	for _, cand := range []string{"/usr/local/go/bin/go", "/usr/lib/go/bin/go", filepath.Join(os.Getenv("HOME"), "go/bin/go")} {
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand
		}
	}
	t.Skip("go toolchain not found on PATH or at the usual absolute entries — cannot measure, which is not the same as passing")
	return ""
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
