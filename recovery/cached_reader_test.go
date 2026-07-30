package recovery

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
)

type countingReaderAt struct {
	data  []byte
	reads atomic.Int64
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestCachedReaderCoalescesSmallReads(t *testing.T) {
	source := &countingReaderAt{data: bytes.Repeat([]byte{0x5A}, 4*1024*1024)}
	reader := newCachedReaderAt(source, int64(len(source.data)), 1024*1024, 2)
	buf := make([]byte, 4)
	for i := 0; i < 5000; i++ {
		offset := int64((i * 127) % (1024*1024 - len(buf)))
		if _, err := reader.ReadAt(buf, offset); err != nil {
			t.Fatal(err)
		}
	}
	if got := source.reads.Load(); got != 1 {
		t.Fatalf("expected one backing read for cached small accesses, got %d", got)
	}
}
