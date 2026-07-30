package recovery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Partition describes a partition or a whole-disk scan range.
type Partition struct {
	Index      int    `json:"index"`
	Scheme     string `json:"scheme"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Start      int64  `json:"start"`
	Size       int64  `json:"size"`
	FileSystem string `json:"file_system"`
	Bootable   bool   `json:"bootable"`
	Deleted    bool   `json:"deleted"`
}

func (p Partition) End() int64 { return p.Start + p.Size }

func (p Partition) Label() string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.Type
	}
	if name == "" {
		name = "Partição"
	}
	fs := strings.TrimSpace(p.FileSystem)
	if fs == "" {
		fs = "desconhecido"
	}
	return fmt.Sprintf("%s %d — %s — %s — %s", p.Scheme, p.Index, name, fs, humanBytes(p.Size))
}

// DetectPartitions parses GPT/MBR metadata without mounting or writing to the disk.
func DetectPartitions(r io.ReaderAt, diskSize, sectorSize int64) ([]Partition, error) {
	if diskSize <= 0 {
		return nil, fmt.Errorf("tamanho de disco inválido")
	}
	if sectorSize < 512 || sectorSize > 1024*1024 || sectorSize&(sectorSize-1) != 0 {
		sectorSize = 512
	}

	mbr := make([]byte, 512)
	if _, err := r.ReadAt(mbr, 0); err != nil && err != io.EOF {
		return nil, err
	}
	if len(mbr) < 512 || mbr[510] != 0x55 || mbr[511] != 0xAA {
		return []Partition{{Index: 0, Scheme: "RAW", Type: "Disco inteiro", Name: "Disco inteiro", Start: 0, Size: diskSize, FileSystem: detectFileSystem(r, 0)}}, nil
	}

	protective := false
	mbrParts := make([]Partition, 0, 4)
	for i := 0; i < 4; i++ {
		e := mbr[446+i*16 : 446+(i+1)*16]
		partType := e[4]
		startLBA := uint64(binary.LittleEndian.Uint32(e[8:12]))
		sectors := uint64(binary.LittleEndian.Uint32(e[12:16]))
		if partType == 0 || sectors == 0 {
			continue
		}
		if partType == 0xEE {
			protective = true
		}
		start, ok1 := mulInt64(startLBA, uint64(sectorSize))
		size, ok2 := mulInt64(sectors, uint64(sectorSize))
		if !ok1 || !ok2 || start < 0 || size <= 0 || start >= diskSize {
			continue
		}
		if start+size > diskSize {
			size = diskSize - start
		}
		mbrParts = append(mbrParts, Partition{
			Index:      len(mbrParts) + 1,
			Scheme:     "MBR",
			Type:       mbrTypeName(partType),
			Start:      start,
			Size:       size,
			FileSystem: detectFileSystem(r, start),
			Bootable:   e[0] == 0x80,
		})
	}

	if protective {
		if gpt, err := detectGPT(r, diskSize, sectorSize); err == nil && len(gpt) > 0 {
			return gpt, nil
		}
	}
	if len(mbrParts) > 0 {
		return mbrParts, nil
	}
	return []Partition{{Index: 0, Scheme: "RAW", Type: "Disco inteiro", Name: "Disco inteiro", Start: 0, Size: diskSize, FileSystem: detectFileSystem(r, 0)}}, nil
}

func detectGPT(r io.ReaderAt, diskSize, sectorSize int64) ([]Partition, error) {
	header := make([]byte, sectorSize)
	if _, err := r.ReadAt(header, sectorSize); err != nil && err != io.EOF {
		return nil, err
	}
	if len(header) < 92 || !bytes.Equal(header[:8], []byte("EFI PART")) {
		return nil, fmt.Errorf("cabeçalho GPT ausente")
	}
	entryLBA := binary.LittleEndian.Uint64(header[72:80])
	entryCount := binary.LittleEndian.Uint32(header[80:84])
	entrySize := binary.LittleEndian.Uint32(header[84:88])
	if entryCount == 0 || entrySize < 128 || entrySize > 4096 {
		return nil, fmt.Errorf("tabela GPT inválida")
	}
	if entryCount > 4096 {
		entryCount = 4096
	}
	tableOffset, ok := mulInt64(entryLBA, uint64(sectorSize))
	if !ok || tableOffset < 0 || tableOffset >= diskSize {
		return nil, fmt.Errorf("offset GPT inválido")
	}
	tableBytes := int64(entryCount) * int64(entrySize)
	if tableBytes > 16*1024*1024 {
		return nil, fmt.Errorf("tabela GPT grande demais")
	}
	table := make([]byte, tableBytes)
	if _, err := r.ReadAt(table, tableOffset); err != nil && err != io.EOF {
		return nil, err
	}

	parts := make([]Partition, 0, entryCount)
	for i := uint32(0); i < entryCount; i++ {
		e := table[int64(i)*int64(entrySize) : int64(i+1)*int64(entrySize)]
		if allZeroBytes(e[:16]) {
			continue
		}
		first := binary.LittleEndian.Uint64(e[32:40])
		last := binary.LittleEndian.Uint64(e[40:48])
		if last < first {
			continue
		}
		start, ok1 := mulInt64(first, uint64(sectorSize))
		sectors := last - first + 1
		size, ok2 := mulInt64(sectors, uint64(sectorSize))
		if !ok1 || !ok2 || size <= 0 || start < 0 || start >= diskSize {
			continue
		}
		if start+size > diskSize {
			size = diskSize - start
		}
		name := decodeUTF16LE(e[56:minIntLocal(len(e), 128)])
		parts = append(parts, Partition{
			Index:      len(parts) + 1,
			Scheme:     "GPT",
			Type:       gptTypeName(e[:16]),
			Name:       name,
			Start:      start,
			Size:       size,
			FileSystem: detectFileSystem(r, start),
		})
	}
	return parts, nil
}

func detectFileSystem(r io.ReaderAt, start int64) string {
	buf := make([]byte, 4096)
	_, _ = r.ReadAt(buf, start)
	if len(buf) >= 11 && string(buf[3:11]) == "NTFS    " {
		return "NTFS"
	}
	if len(buf) >= 11 && string(buf[3:11]) == "EXFAT   " {
		return "exFAT"
	}
	if len(buf) >= 90 && (string(buf[82:90]) == "FAT32   " || string(buf[54:62]) == "FAT16   ") {
		return strings.TrimSpace(string(buf[82:90]))
	}
	if len(buf) >= 1082 && buf[1080] == 0x53 && buf[1081] == 0xEF {
		return "ext"
	}
	return ""
}

func mbrTypeName(t byte) string {
	switch t {
	case 0x07:
		return "NTFS/exFAT"
	case 0x0B, 0x0C:
		return "FAT32"
	case 0x05, 0x0F:
		return "Estendida"
	case 0x82:
		return "Linux swap"
	case 0x83:
		return "Linux"
	case 0xEE:
		return "GPT protetora"
	default:
		return fmt.Sprintf("Tipo 0x%02X", t)
	}
}

func gptTypeName(guid []byte) string {
	if len(guid) < 16 {
		return "GPT"
	}
	// GUIDs are stored with mixed endian fields. Compare raw bytes for common types.
	known := map[string]string{
		"a2a0d0ebe5b9334487c068b6b72699c7": "Dados básicos Microsoft",
		"28732ac11ff8d211ba4b00a0c93ec93b": "Sistema EFI",
		"16e3c9e35c0bb84d817df92df00215ae": "Reservada Microsoft",
		"af3dc60f838472478e793d69d8477de4": "Linux filesystem",
	}
	if name, ok := known[fmt.Sprintf("%x", guid[:16])]; ok {
		return name
	}
	return "Partição GPT"
}

func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		v := binary.LittleEndian.Uint16(b[i : i+2])
		if v == 0 {
			break
		}
		u = append(u, v)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

func mulInt64(a, b uint64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > uint64(^uint64(0)>>1)/b {
		return 0, false
	}
	v := a * b
	if v > uint64(^uint64(0)>>1) {
		return 0, false
	}
	return int64(v), true
}

func allZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func minIntLocal(a, b int) int {
	if a < b {
		return a
	}
	return b
}
