package recovery

import (
	"io"
	"sync"
)

type cachedBlock struct {
	data []byte
	n    int
	used uint64
}

// cachedReaderAt mantém uma pequena cache de blocos para transformar milhares
// de leituras de cabeçalhos de poucos bytes em leituras sequenciais maiores.
// Isso é especialmente importante em HDs mecânicos, onde cada seek custa muito.
type cachedReaderAt struct {
	source    io.ReaderAt
	size      int64
	blockSize int64
	maxBlocks int

	mu     sync.Mutex
	blocks map[int64]*cachedBlock
	tick   uint64
}

func newCachedReaderAt(source io.ReaderAt, size, blockSize int64, maxBlocks int) *cachedReaderAt {
	if blockSize < 64*1024 {
		blockSize = 64 * 1024
	}
	if maxBlocks < 1 {
		maxBlocks = 1
	}
	return &cachedReaderAt{
		source: source, size: size, blockSize: blockSize, maxBlocks: maxBlocks,
		blocks: make(map[int64]*cachedBlock, maxBlocks),
	}
}

func (r *cachedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}

	// Leituras grandes já são eficientes e não devem expulsar cabeçalhos úteis
	// da cache. Elas seguem diretamente para o leitor alinhado.
	if int64(len(p)) > r.blockSize*4 {
		return r.source.ReadAt(p, off)
	}

	copied := 0
	for copied < len(p) && off+int64(copied) < r.size {
		absolute := off + int64(copied)
		blockStart := absolute - absolute%r.blockSize
		block, err := r.block(blockStart)
		if block == nil || block.n == 0 {
			if copied > 0 {
				return copied, nil
			}
			return 0, err
		}
		inside := int(absolute - blockStart)
		if inside >= block.n {
			if copied > 0 {
				return copied, io.EOF
			}
			return 0, io.EOF
		}
		available := block.n - inside
		want := len(p) - copied
		if want > available {
			want = available
		}
		copy(p[copied:copied+want], block.data[inside:inside+want])
		copied += want
		if want == 0 {
			break
		}
	}
	if copied == len(p) {
		return copied, nil
	}
	return copied, io.EOF
}

func (r *cachedReaderAt) block(start int64) (*cachedBlock, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tick++
	if block, ok := r.blocks[start]; ok {
		block.used = r.tick
		return block, nil
	}

	length := r.blockSize
	if remaining := r.size - start; remaining < length {
		length = remaining
	}
	if length <= 0 {
		return nil, io.EOF
	}

	data := make([]byte, int(length))
	n, err := r.source.ReadAt(data, start)
	if n == 0 && err != nil {
		return nil, err
	}
	block := &cachedBlock{data: data[:n], n: n, used: r.tick}
	if len(r.blocks) >= r.maxBlocks {
		var oldestStart int64
		oldestUsed := ^uint64(0)
		for key, existing := range r.blocks {
			if existing.used < oldestUsed {
				oldestStart = key
				oldestUsed = existing.used
			}
		}
		delete(r.blocks, oldestStart)
	}
	r.blocks[start] = block
	return block, err
}
