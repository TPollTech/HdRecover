package recovery

import (
	"errors"
	"fmt"
	"io"
)

// ByteRange is an absolute source interval.
type ByteRange struct {
	Start int64 `json:"start"`
	Size  int64 `json:"size"`
}

func (r ByteRange) End() int64 { return r.Start + r.Size }

// NTFSFreeRangesFromPath reads $Bitmap and returns unallocated cluster ranges.
func NTFSFreeRangesFromPath(path string, sourceSize, sectorSize int64, partition Partition, cancel <-chan struct{}, paused func() bool) ([]ByteRange, error) {
	if partition.Size <= 0 {
		return nil, errors.New("partição inválida")
	}
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	aligned := newAlignedReaderAt(f, sourceSize, sectorSize)
	adaptive := newAdaptiveReaderAt(aligned, aligned.sector, DamageBalanced, cancel, paused)
	reader := newCachedReaderAt(adaptive, sourceSize, 2*1024*1024, 4)
	vol, err := openNTFSVolume(reader, partition.Start, partition.Size)
	if err != nil {
		return nil, err
	}

	record := make([]byte, vol.recordSize)
	if err := readNTFSStreamAt(reader, vol, vol.mftRuns, 6*vol.recordSize, record); err != nil {
		return nil, fmt.Errorf("não foi possível ler $Bitmap: %w", err)
	}
	if err := applyNTFSFixup(record, vol.bytesSector); err != nil {
		return nil, fmt.Errorf("registro $Bitmap inválido: %w", err)
	}
	entry, err := parseMFTRecord(record, 6, vol)
	if err != nil || entry.data.realSize <= 0 {
		return nil, errors.New("atributo de dados do $Bitmap não encontrado")
	}
	if entry.data.realSize > 512*1024*1024 {
		return nil, errors.New("$Bitmap grande demais para análise segura")
	}
	bitmap := make([]byte, entry.data.realSize)
	if entry.data.resident != nil {
		copy(bitmap, entry.data.resident)
	} else {
		if err := readNTFSStreamAt(reader, vol, entry.data.runs, 0, bitmap); err != nil && err != io.EOF {
			return nil, fmt.Errorf("não foi possível ler o conteúdo do $Bitmap: %w", err)
		}
	}

	totalClusters := partition.Size / vol.clusterSize
	maxBits := int64(len(bitmap)) * 8
	if totalClusters > maxBits {
		totalClusters = maxBits
	}
	ranges := make([]ByteRange, 0, 1024)
	var freeStart int64 = -1
	flush := func(endCluster int64) {
		if freeStart < 0 || endCluster <= freeStart {
			freeStart = -1
			return
		}
		start := partition.Start + freeStart*vol.clusterSize
		size := (endCluster - freeStart) * vol.clusterSize
		if start+size > partition.End() {
			size = partition.End() - start
		}
		if size > 0 {
			ranges = append(ranges, ByteRange{Start: start, Size: size})
		}
		freeStart = -1
	}
	for cluster := int64(0); cluster < totalClusters; cluster++ {
		if err := waitControl(cancel, paused); err != nil {
			return ranges, err
		}
		allocated := bitmap[cluster/8]&(1<<uint(cluster%8)) != 0
		if !allocated {
			if freeStart < 0 {
				freeStart = cluster
			}
		} else if freeStart >= 0 {
			flush(cluster)
		}
	}
	flush(totalClusters)
	return ranges, nil
}

func totalRangeBytes(ranges []ByteRange) int64 {
	var total int64
	for _, r := range ranges {
		if r.Size > 0 {
			total += r.Size
		}
	}
	return total
}
