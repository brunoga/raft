package raft_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// completeProgram matches a fenced Go block in Markdown that is a whole
// program rather than a fragment.
var completeProgram = regexp.MustCompile("(?s)```go\n(package [^`]*?)```")

// TestREADME_CompleteProgramsCompile builds every self-contained Go program in
// README.md against this module.
//
// The README's quick start is the first code anyone runs, and it had drifted:
// it implemented StateMachine with the []byte-based Snapshot and Restore that
// the streaming io.Writer/io.Reader pair replaced, so it did not compile at
// all. Nothing caught it, because documentation is not built.
//
// This builds it. A signature change that leaves the README behind fails here
// instead of failing the first person to copy it.
func TestREADME_CompleteProgramsCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module in a temp dir; skipped in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	programs := completeProgram.FindAllStringSubmatch(string(readme), -1)
	if len(programs) == 0 {
		t.Fatal("found no complete Go programs in README.md; the extraction is broken, " +
			"which would make this test pass no matter what the README said")
	}

	for i, m := range programs {
		src := m[1]
		t.Run(programName(src, i), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			// A major-version suffix in the path constrains the version that
			// may be required against it: v0.0.0 is what a replaced module is
			// usually pinned at, and /v2 refuses it.
			goMod := "module readmecheck\n\ngo 1.26\n\nrequire github.com/brunoga/raft/v2 v2.0.0\n\n" +
				"replace github.com/brunoga/raft/v2 => " + root + "\n"
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}
			// The module graph of the replaced module still needs checksums;
			// reuse this module's rather than resolving them again.
			if sum, rerr := os.ReadFile(filepath.Join(root, "go.sum")); rerr == nil {
				if werr := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o600); werr != nil {
					t.Fatalf("write go.sum: %v", werr)
				}
			}

			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
			if out, berr := cmd.CombinedOutput(); berr != nil {
				t.Errorf("README program does not compile: %v\n%s\n--- source ---\n%s",
					berr, out, src)
			}
		})
	}
}

// programName labels a subtest with the program's first declared type, so a
// failure says which example broke rather than just its position.
func programName(src string, i int) string {
	for line := range strings.Lines(src) {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "type "); ok {
			if f := strings.Fields(name); len(f) > 0 {
				return f[0]
			}
		}
	}
	return "program" + string(rune('0'+i))
}
