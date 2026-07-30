package recovery

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const candidateRecordSize = 16

type candidateStore struct {
	path   string
	file   *os.File
	writer *bufio.Writer
	count  int64
}

func openCandidateStore(path string, appendExisting bool) (*candidateStore, error) {
	flags := os.O_CREATE | os.O_RDWR
	if appendExisting {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	count := int64(0)
	if st != nil {
		count = st.Size() / candidateRecordSize
	}
	return &candidateStore{path: path, file: f, writer: bufio.NewWriterSize(f, 1024*1024), count: count}, nil
}

func (s *candidateStore) Append(items []scanCandidate) error {
	var rec [candidateRecordSize]byte
	for _, c := range items {
		binary.LittleEndian.PutUint64(rec[0:8], uint64(c.offset))
		binary.LittleEndian.PutUint32(rec[8:12], uint32(c.kind))
		binary.LittleEndian.PutUint32(rec[12:16], 0)
		if _, err := s.writer.Write(rec[:]); err != nil {
			return err
		}
		s.count++
	}
	return nil
}

func (s *candidateStore) Flush() error {
	if s.writer != nil {
		return s.writer.Flush()
	}
	return nil
}

func (s *candidateStore) Close() error {
	var first error
	if s.writer != nil {
		if err := s.writer.Flush(); err != nil {
			first = err
		}
		s.writer = nil
	}
	if s.file != nil {
		if err := s.file.Close(); first == nil {
			first = err
		}
		s.file = nil
	}
	return first
}

func (s *candidateStore) Count() int64 { return s.count }

func sortCandidateFile(inputPath, outputPath, tempDir string, cancel <-chan struct{}, paused func() bool, progress func(done, total int64)) (int64, error) {
	in, err := os.Open(inputPath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	total := st.Size() / candidateRecordSize
	if total == 0 {
		out, err := os.Create(outputPath)
		if err == nil {
			err = out.Close()
		}
		return 0, err
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return 0, err
	}

	const recordsPerChunk = 250000
	reader := bufio.NewReaderSize(in, 4*1024*1024)
	chunks := make([]string, 0, int(total/recordsPerChunk)+1)
	var readCount int64
	for readCount < total {
		if err := waitControl(cancel, paused); err != nil {
			return 0, err
		}
		want := int64(recordsPerChunk)
		if total-readCount < want {
			want = total - readCount
		}
		items := make([]scanCandidate, 0, want)
		for int64(len(items)) < want {
			c, err := readCandidate(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return 0, err
			}
			items = append(items, c)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].offset == items[j].offset {
				return items[i].kind < items[j].kind
			}
			return items[i].offset < items[j].offset
		})
		items = dedupeCandidates(items)
		chunkPath := filepath.Join(tempDir, fmt.Sprintf("candidates_%05d.bin", len(chunks)))
		if err := writeCandidateSlice(chunkPath, items); err != nil {
			return 0, err
		}
		chunks = append(chunks, chunkPath)
		readCount += want
		if progress != nil {
			progress(readCount, total)
		}
	}

	if len(chunks) == 1 {
		_ = os.Remove(outputPath)
		if err := os.Rename(chunks[0], outputPath); err != nil {
			return 0, err
		}
		return countCandidateFile(outputPath)
	}

	count, err := mergeCandidateChunks(chunks, outputPath, cancel, paused, progress, total)
	for _, p := range chunks {
		_ = os.Remove(p)
	}
	return count, err
}

func writeCandidateSlice(path string, items []scanCandidate) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1024*1024)
	var rec [candidateRecordSize]byte
	for _, c := range items {
		binary.LittleEndian.PutUint64(rec[:8], uint64(c.offset))
		binary.LittleEndian.PutUint32(rec[8:12], uint32(c.kind))
		if _, err := w.Write(rec[:]); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func countCandidateFile(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size() / candidateRecordSize, nil
}

type mergeSource struct {
	file   *os.File
	reader *bufio.Reader
	value  scanCandidate
	index  int
}

type candidateHeap []*mergeSource

func (h candidateHeap) Len() int { return len(h) }
func (h candidateHeap) Less(i, j int) bool {
	if h[i].value.offset == h[j].value.offset {
		return h[i].value.kind < h[j].value.kind
	}
	return h[i].value.offset < h[j].value.offset
}
func (h candidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(x any)   { *h = append(*h, x.(*mergeSource)) }
func (h *candidateHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func mergeCandidateChunks(chunks []string, outputPath string, cancel <-chan struct{}, paused func() bool, progress func(done, total int64), total int64) (int64, error) {
	out, err := os.Create(outputPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	writer := bufio.NewWriterSize(out, 4*1024*1024)
	defer writer.Flush()

	sources := make([]*mergeSource, 0, len(chunks))
	h := candidateHeap{}
	for i, path := range chunks {
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		s := &mergeSource{file: f, reader: bufio.NewReaderSize(f, 1024*1024), index: i}
		c, err := readCandidate(s.reader)
		if err == nil {
			s.value = c
			sources = append(sources, s)
			heap.Push(&h, s)
		} else {
			_ = f.Close()
		}
	}
	defer func() {
		for _, s := range sources {
			_ = s.file.Close()
		}
	}()
	heap.Init(&h)

	var previous scanCandidate
	havePrevious := false
	var written, consumed int64
	for h.Len() > 0 {
		if err := waitControl(cancel, paused); err != nil {
			return written, err
		}
		s := heap.Pop(&h).(*mergeSource)
		c := s.value
		consumed++
		if !havePrevious || c.offset != previous.offset || c.kind != previous.kind {
			if err := writeCandidate(writer, c); err != nil {
				return written, err
			}
			previous = c
			havePrevious = true
			written++
		}
		next, err := readCandidate(s.reader)
		if err == nil {
			s.value = next
			heap.Push(&h, s)
		} else if !errors.Is(err, io.EOF) {
			return written, err
		}
		if progress != nil && consumed%50000 == 0 {
			progress(consumed, total)
		}
	}
	if err := writer.Flush(); err != nil {
		return written, err
	}
	return written, nil
}

func readCandidate(r io.Reader) (scanCandidate, error) {
	var rec [candidateRecordSize]byte
	_, err := io.ReadFull(r, rec[:])
	if err != nil {
		return scanCandidate{}, err
	}
	return scanCandidate{offset: int64(binary.LittleEndian.Uint64(rec[:8])), kind: int(int32(binary.LittleEndian.Uint32(rec[8:12])))}, nil
}

func writeCandidate(w io.Writer, c scanCandidate) error {
	var rec [candidateRecordSize]byte
	binary.LittleEndian.PutUint64(rec[:8], uint64(c.offset))
	binary.LittleEndian.PutUint32(rec[8:12], uint32(c.kind))
	_, err := w.Write(rec[:])
	return err
}

type candidateIterator struct {
	file   *os.File
	reader *bufio.Reader
}

func openCandidateIterator(path string) (*candidateIterator, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &candidateIterator{file: f, reader: bufio.NewReaderSize(f, 4*1024*1024)}, nil
}

func (it *candidateIterator) Next() (scanCandidate, error) { return readCandidate(it.reader) }
func (it *candidateIterator) Close() error                 { return it.file.Close() }
