package compat_test

// Keeps docs/pipecat-parity-matrix.md honest: every path it cites exists, statuses come from a
// closed vocabulary, test names resolve, fixture ids resolve, and every declared
// intentional_difference in the goldens is explained in the matrix.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	tickRE     = regexp.MustCompile("`([^`]+)`")
	pathRE     = regexp.MustCompile(`^[A-Za-z0-9_.\-]+(/[A-Za-z0-9_.\-]+)*/?$`)
	testNameRE = regexp.MustCompile(`\bTest[A-Za-z0-9_]+\b`)
	fileExts   = []string{".go", ".py", ".md", ".js", ".json", ".yaml", ".sh", ".onnx"}
)

func looksLikePath(tok string) bool {
	if !pathRE.MatchString(tok) {
		return false
	}
	if strings.Contains(tok, "/") {
		return true
	}
	for _, e := range fileExts {
		if strings.HasSuffix(tok, e) {
			return true
		}
	}
	return false
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestParityMatrix(t *testing.T) {
	root := filepath.Join("..", "..")
	pipecat := filepath.Join(root, "pipecat-impl", "src", "pipecat")
	raw, err := os.ReadFile(filepath.Join(root, "docs", "pipecat-parity-matrix.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// Declared differences and scenario ids from the goldens.
	ids, diffs := map[string]bool{}, []string{}
	files, _ := filepath.Glob(filepath.Join("testdata", "parity", "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var g parityGolden
		if err := json.Unmarshal(data, &g); err != nil {
			t.Fatal(err)
		}
		for _, sc := range g.Scenarios {
			id := g.Area + "/" + sc.ID
			ids[id] = true
			if sc.Diff != nil {
				diffs = append(diffs, id)
			}
		}
	}
	areas := map[string]bool{}
	for id := range ids {
		areas[strings.SplitN(id, "/", 2)[0]] = true
	}
	var wantN, wantAreas, wantDiffs int
	if _, err := fmt.Sscanf(text[strings.Index(text, "goldens are in"):], "goldens are in `tools/compat/testdata/parity/`, %d scenarios in %d areas, %d with a declared", &wantN, &wantAreas, &wantDiffs); err != nil {
		t.Fatalf("matrix intro no longer states the fixture counts: %v", err)
	}
	if wantN != len(ids) || wantAreas != len(areas) || wantDiffs != len(diffs) {
		t.Errorf("matrix says %d scenarios/%d areas/%d differences, goldens have %d/%d/%d", wantN, wantAreas, wantDiffs, len(ids), len(areas), len(diffs))
	}
	for _, id := range diffs {
		if !strings.Contains(text, "`"+id+"`") {
			t.Errorf("declared difference %s is not explained in the matrix", id)
		}
	}

	valid := map[string]bool{"parity": true, "partial": true, "intentionally different": true, "not planned": true}
	// The Default cell says whether a stock server runs the feature, so "available" is never read as "on".
	defaults := []string{"on", "off: ", "library only", "n/a"}
	rows, inMatrix := 0, false
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "|") {
			inMatrix = false
			continue
		}
		cells := strings.Split(strings.Trim(line, "| \t"), " | ")
		if cells[0] == "Feature" {
			inMatrix = true
			continue
		}
		if !inMatrix || strings.HasPrefix(cells[0], "---") {
			continue
		}
		if len(cells) != 7 {
			t.Errorf("row has %d cells, want 7: %.60s", len(cells), line)
			continue
		}
		rows++
		feature := strings.TrimSpace(cells[0])
		if !valid[strings.TrimSpace(cells[3])] {
			t.Errorf("%s: invalid status %q", feature, cells[3])
		}
		okDefault := false
		for _, d := range defaults {
			okDefault = okDefault || strings.HasPrefix(cells[4], d)
		}
		if !okDefault {
			t.Errorf("%s: default %q must start with one of %q", feature, cells[4], defaults)
		}
		for i, c := range cells {
			if i > 0 && strings.TrimSpace(c) == "" {
				t.Errorf("%s: empty cell %d", feature, i+1)
			}
		}
		for i, c := range cells {
			for _, m := range tickRE.FindAllStringSubmatch(c, -1) {
				tok := m[1]
				if ids[tok] || (strings.Contains(tok, "/") && areas[strings.SplitN(tok, "/", 2)[0]] && !exists(filepath.Join(root, tok))) {
					if !ids[tok] {
						t.Errorf("%s: unknown fixture scenario %s", feature, tok)
					}
					continue
				}
				if strings.HasPrefix(tok, "/") || !looksLikePath(tok) {
					continue
				}
				base := root
				if i == 1 { // Pipecat column
					base = pipecat
				}
				if !exists(filepath.Join(base, tok)) && !((i == 6 || i == 1) && (exists(filepath.Join(pipecat, tok)) || exists(filepath.Join(root, tok)))) {
					t.Errorf("%s: cited path %q does not exist", feature, tok)
				}
			}
		}
	}
	if rows < 30 {
		t.Errorf("only %d matrix rows parsed", rows)
	}

	// Every test name the matrix mentions must be defined somewhere.
	want := map[string]bool{}
	for _, n := range testNameRE.FindAllString(text, -1) {
		want[n] = true
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "pipecat-impl", ".worktrees", ".claude", ".git", ".venv", "third_party":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") || len(want) == 0 {
			return nil
		}
		b, _ := os.ReadFile(p)
		for n := range want {
			if strings.Contains(string(b), "func "+n+"(") {
				delete(want, n)
			}
		}
		return nil
	})
	for n := range want {
		t.Errorf("matrix mentions %s but no such test exists", n)
	}
}
