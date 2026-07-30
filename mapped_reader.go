package main

import (
	"errors"
	"io"
	"sort"
)

type mappedReadRange struct {
	Start  int64
	End    int64
	Reader io.ReaderAt
	Label  string
}

type mappedRangeReader struct {
	base   io.ReaderAt
	ranges []mappedReadRange
}

func newMappedRangeReader(base io.ReaderAt, ranges []mappedReadRange) io.ReaderAt {
	copied := append([]mappedReadRange(nil), ranges...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].Start < copied[j].Start })
	return &mappedRangeReader{base: base, ranges: copied}
}

func (m *mappedRangeReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for total < len(p) {
		absolute := off + int64(total)
		idx := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].End > absolute })
		if idx < len(m.ranges) && absolute >= m.ranges[idx].Start && absolute < m.ranges[idx].End {
			r := m.ranges[idx]
			count := len(p) - total
			if remaining := r.End - absolute; int64(count) > remaining {
				count = int(remaining)
			}
			n, err := r.Reader.ReadAt(p[total:total+count], absolute-r.Start)
			total += n
			if err != nil {
				if errors.Is(err, io.EOF) && n == count {
					continue
				}
				return total, err
			}
			if n != count {
				return total, io.ErrUnexpectedEOF
			}
			continue
		}

		count := len(p) - total
		if idx < len(m.ranges) && m.ranges[idx].Start > absolute {
			if gap := m.ranges[idx].Start - absolute; int64(count) > gap {
				count = int(gap)
			}
		}
		n, err := m.base.ReadAt(p[total:total+count], absolute)
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) && n == count {
				continue
			}
			return total, err
		}
		if n != count {
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}
