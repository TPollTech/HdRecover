package recovery

import "fmt"

// DetectPartitionsFromPath opens the source read-only and parses its partition table.
func DetectPartitionsFromPath(path string, size, sector int64) ([]Partition, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, fmt.Errorf("não foi possível abrir a origem: %w", err)
	}
	defer f.Close()
	aligned := newAlignedReaderAt(f, size, sector)
	return DetectPartitions(aligned, size, sector)
}
