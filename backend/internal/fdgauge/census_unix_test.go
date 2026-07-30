//go:build unix

package fdgauge

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var regCount = regexp.MustCompile(`reg=(\d+)`)

func regs(t *testing.T) int {
	t.Helper()
	c, err := census()
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	m := regCount.FindStringSubmatch(c)
	if m == nil {
		t.Fatalf("census %q carries no reg count", c)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("census %q reg count: %v", c, err)
	}
	return n
}

// The census must see descriptors this process actually holds: opening three
// files raises the regular-file count by at least three.
func TestCensusSeesOpenFiles(t *testing.T) {
	before := regs(t)

	dir := t.TempDir()
	for i := range 3 {
		name := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		h, err := os.Open(name)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer h.Close()
	}

	if after := regs(t); after < before+3 {
		t.Fatalf("reg count went %d -> %d, want at least +3", before, after)
	}
}
