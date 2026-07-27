package tail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collector records every line it is handed, which is what the incremental
// contract is about: each line reaches the scanner exactly once.
type collector struct {
	lines []string
	want  int
	reads int
}

func (c *collector) Consume(buffer []byte, startsMidLine bool) int64 {
	c.reads++
	return Lines(buffer, startsMidLine, func(line []byte) {
		c.lines = append(c.lines, string(line))
	})
}

func (c *collector) Complete() bool {
	return c.want > 0 && len(c.lines) >= c.want
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func scanned(t *testing.T, reader *Reader, path string, scanner **collector) []string {
	t.Helper()
	result, err := reader.Scan(context.Background(), path, 0, func() Scanner {
		*scanner = &collector{}
		return *scanner
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.(*collector).lines
}

func TestReaderFoldsInOnlyWhatWasAppended(t *testing.T) {
	path := writeLog(t, "one", "two")
	var scanner *collector
	reader := &Reader{}

	if got := scanned(t, reader, path, &scanner); len(got) != 2 {
		t.Fatalf("first scan = %q, want both lines", got)
	}
	appendLog(t, path, "three")
	got := scanned(t, reader, path, &scanner)

	want := []string{"one", "two", "three"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scan = %q, want %q", got, want)
	}
	if scanner.reads != 2 {
		t.Fatalf("scanner rebuilt: reads = %d, want the first scanner extended",
			scanner.reads)
	}
}

func TestReaderSkipsTheReadEntirelyWhenNothingWasAppended(t *testing.T) {
	path := writeLog(t, "one")
	var scanner *collector
	reader := &Reader{}

	scanned(t, reader, path, &scanner)
	scanned(t, reader, path, &scanner)

	if scanner.reads != 1 {
		t.Fatalf("reads = %d, want the unchanged log left alone", scanner.reads)
	}
	if len(scanner.lines) != 1 {
		t.Fatalf("lines = %q, want the line counted once", scanner.lines)
	}
}

// A log is appended to while a record is half-written, so a read can land in
// the middle of a line. That fragment must be handed over whole on the next
// read, not split across two of them and lost.
func TestReaderDeliversARecordThatWasHalfWrittenWhenItWasRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, []byte("one\ntw"), 0o600); err != nil {
		t.Fatal(err)
	}
	var scanner *collector
	reader := &Reader{}

	if got := scanned(t, reader, path, &scanner); len(got) != 1 {
		t.Fatalf("first scan = %q, want only the complete line", got)
	}
	appendLog(t, path, "o")
	got := scanned(t, reader, path, &scanner)

	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("scan = %q, want the split record delivered whole", got)
	}
}

func TestReaderRescansALogThatShrank(t *testing.T) {
	path := writeLog(t, "one", "two", "three")
	var scanner *collector
	reader := &Reader{}
	scanned(t, reader, path, &scanner)

	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := scanned(t, reader, path, &scanner)

	if strings.Join(got, ",") != "fresh" {
		t.Fatalf("scan = %q, want the replaced log read from scratch", got)
	}
}

func TestReaderRescansWhenTheCallerAsksForADifferentFile(t *testing.T) {
	first := writeLog(t, "a")
	second := writeLog(t, "b")
	var scanner *collector
	reader := &Reader{}

	scanned(t, reader, first, &scanner)
	got := scanned(t, reader, second, &scanner)

	if strings.Join(got, ",") != "b" {
		t.Fatalf("scan = %q, want only the second log", got)
	}
}

func TestReaderRescansWhenTheLimitChanges(t *testing.T) {
	path := writeLog(t, "a", "b")
	built := 0
	reader := &Reader{}
	build := func() Scanner {
		built++
		return &collector{}
	}
	for _, limit := range []int{4, 4, 8} {
		if _, err := reader.Scan(context.Background(), path, limit, build); err != nil {
			t.Fatal(err)
		}
	}
	if built != 2 {
		t.Fatalf("scanners built = %d, want one per distinct limit", built)
	}
}

// A cold scan starts small and widens. Whatever it settles on, the offset it
// remembers has to line up with a record boundary or the next read starts
// mid-line.
func TestReaderResumesCorrectlyAfterWideningItsColdWindow(t *testing.T) {
	var lines []string
	filler := strings.Repeat("x", 4096)
	for range 400 {
		lines = append(lines, filler)
	}
	lines = append(lines, "last")
	path := writeLog(t, lines...)

	var scanner *collector
	reader := &Reader{}
	result, err := reader.Scan(context.Background(), path, 0, func() Scanner {
		scanner = &collector{want: 3}
		return scanner
	})
	if err != nil {
		t.Fatal(err)
	}
	before := len(result.(*collector).lines)

	appendLog(t, path, "after")
	got := scanned(t, reader, path, &scanner)

	if len(got) != before+1 {
		t.Fatalf("lines = %d, want %d after one append", len(got), before+1)
	}
	if got[len(got)-1] != "after" {
		t.Fatalf("last line = %q, want the appended record", got[len(got)-1])
	}
}

func TestLinesLeavesATrailingFragmentForTheNextRead(t *testing.T) {
	var seen []string
	consumed := Lines([]byte("a\nb\nhalf"), false, func(line []byte) {
		seen = append(seen, string(line))
	})
	if strings.Join(seen, ",") != "a,b" {
		t.Fatalf("lines = %q, want the complete ones only", seen)
	}
	if consumed != 4 {
		t.Fatalf("consumed = %d, want the offset past the last newline", consumed)
	}
}

func TestLinesDropsTheFragmentAWindowOpensOn(t *testing.T) {
	var seen []string
	Lines([]byte("alf\nb\n"), true, func(line []byte) {
		seen = append(seen, string(line))
	})
	if strings.Join(seen, ",") != "b" {
		t.Fatalf("lines = %q, want the leading fragment dropped", seen)
	}
}
