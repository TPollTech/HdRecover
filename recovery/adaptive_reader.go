package recovery

import (
	"errors"
	"io"
	"sync"
	"time"
)

// BadRange records bytes that could not be read from the source.
type BadRange struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Error  string `json:"error,omitempty"`
}

type adaptiveReaderAt struct {
	base       io.ReaderAt
	sector     int64
	minBlock   int64
	maxRetries int
	cancel     <-chan struct{}
	paused     func() bool
	mu         sync.Mutex
	bad        []BadRange
}

func newAdaptiveReaderAt(base io.ReaderAt, sector int64, mode DamageMode, cancel <-chan struct{}, paused func() bool) *adaptiveReaderAt {
	if sector < 512 || sector&(sector-1) != 0 {
		sector = 512
	}
	r := &adaptiveReaderAt{base: base, sector: sector, cancel: cancel, paused: paused}
	switch mode {
	case DamageCareful:
		r.minBlock = sector
		r.maxRetries = 2
	case DamageBalanced:
		r.minBlock = max64Local(sector, 64*1024)
		r.maxRetries = 1
	default:
		r.minBlock = 1024 * 1024
		r.maxRetries = 0
	}
	return r
}

func (r *adaptiveReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := waitControl(r.cancel, r.paused); err != nil {
		return 0, err
	}
	n, err := r.base.ReadAt(p, off)
	if err == nil || errors.Is(err, io.EOF) && n == len(p) {
		return n, err
	}
	if n > 0 {
		// Preserve the readable prefix and recursively recover the remainder.
		n2, err2 := r.readSplit(p[n:], off+int64(n))
		return n + n2, joinReadError(err, err2)
	}
	return r.readSplit(p, off)
}

func (r *adaptiveReaderAt) readSplit(p []byte, off int64) (int, error) {
	if err := waitControl(r.cancel, r.paused); err != nil {
		return 0, err
	}
	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		n, err := r.base.ReadAt(p, off)
		if err == nil || n == len(p) {
			return n, nil
		}
		if n > 0 {
			n2, err2 := r.readSplit(p[n:], off+int64(n))
			return n + n2, joinReadError(err, err2)
		}
		if attempt < r.maxRetries {
			time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
		}
	}

	if int64(len(p)) <= r.minBlock {
		for i := range p {
			p[i] = 0
		}
		r.recordBad(off, int64(len(p)), "falha persistente de leitura")
		return len(p), io.ErrUnexpectedEOF
	}

	half := alignDownLocal(int64(len(p))/2, r.sector)
	if half < r.minBlock {
		half = r.minBlock
	}
	if half <= 0 || half >= int64(len(p)) {
		half = int64(len(p)) / 2
	}
	n1, err1 := r.readSplit(p[:half], off)
	n2, err2 := r.readSplit(p[half:], off+half)
	return n1 + n2, joinReadError(err1, err2)
}

func (r *adaptiveReaderAt) recordBad(offset, length int64, message string) {
	if length <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bad) > 0 {
		last := &r.bad[len(r.bad)-1]
		if last.Offset+last.Length == offset && last.Error == message {
			last.Length += length
			return
		}
	}
	r.bad = append(r.bad, BadRange{Offset: offset, Length: length, Error: message})
}

func (r *adaptiveReaderAt) BadRanges() []BadRange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BadRange, len(r.bad))
	copy(out, r.bad)
	return out
}

func waitControl(cancel <-chan struct{}, paused func() bool) error {
	for {
		if cancelled(cancel) {
			return errCancelled
		}
		if paused == nil || !paused() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func joinReadError(a, b error) error {
	if errors.Is(a, errCancelled) || errors.Is(b, errCancelled) {
		return errCancelled
	}
	if a != nil {
		return a
	}
	return b
}

func alignDownLocal(v, alignment int64) int64 {
	if alignment <= 0 {
		return v
	}
	return v - v%alignment
}

func max64Local(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
