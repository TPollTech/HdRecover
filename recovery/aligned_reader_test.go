package recovery

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type strictSectorReader struct {
	data   []byte
	sector int64
}

func (r strictSectorReader) ReadAt(p []byte, off int64) (int, error) {
	if off%r.sector != 0 || int64(len(p))%r.sector != 0 {
		return 0, errors.New("leitura não alinhada")
	}
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestAlignedReaderAcceptsArbitraryOffsets(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251)
	}
	reader := newAlignedReaderAt(strictSectorReader{data: data, sector: 512}, int64(len(data)), 512)

	buf := make([]byte, 777)
	n, err := reader.ReadAt(buf, 123)
	if err != nil {
		t.Fatalf("ReadAt returned error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("expected %d bytes, got %d", len(buf), n)
	}
	if !bytes.Equal(buf, data[123:123+777]) {
		t.Fatal("aligned reader returned different data")
	}
}

func TestAlignHelpers(t *testing.T) {
	if got := alignDown(1025, 512); got != 1024 {
		t.Fatalf("alignDown: expected 1024, got %d", got)
	}
	if got := alignUp(1025, 512); got != 1536 {
		t.Fatalf("alignUp: expected 1536, got %d", got)
	}
}
