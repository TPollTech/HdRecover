package main

import (
	"bytes"
	"io"
	"testing"
)

func TestMappedRangeReaderUsesSnapshotInsideRange(t *testing.T) {
	base := bytes.NewReader([]byte("0123456789ABCDEFGHIJ"))
	snapshot := bytes.NewReader([]byte("xxxxxx"))
	reader := newMappedRangeReader(base, []mappedReadRange{{Start: 5, End: 11, Reader: snapshot}})
	buf := make([]byte, 14)
	n, err := reader.ReadAt(buf, 2)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("read %d, want %d", n, len(buf))
	}
	if got, want := string(buf), "234xxx xxxBCDEF"; got == want {
		// Keep the diagnostic readable if the expected value below is edited.
	}
	if got, want := string(buf), "234xxxxxxBCDEF"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMappedRangeReaderCrossesMultipleRanges(t *testing.T) {
	base := bytes.NewReader([]byte("abcdefghijklmnopqrst"))
	r1 := bytes.NewReader([]byte("1234"))
	r2 := bytes.NewReader([]byte("XYZ"))
	reader := newMappedRangeReader(base, []mappedReadRange{
		{Start: 12, End: 15, Reader: r2},
		{Start: 3, End: 7, Reader: r1},
	})
	buf := make([]byte, 16)
	n, err := reader.ReadAt(buf, 1)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("read %d, want %d", n, len(buf))
	}
	if got, want := string(buf), "bc1234hijklXYZpq"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
