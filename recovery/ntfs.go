package recovery

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	ntfsAttrEnd      = 0xFFFFFFFF
	ntfsAttrFileName = 0x30
	ntfsAttrData     = 0x80
)

type ntfsVolume struct {
	start       int64
	size        int64
	bytesSector int64
	clusterSize int64
	recordSize  int64
	mftLCN      int64
	mftRuns     []ntfsRun
	mftSize     int64
}

type ntfsRun struct {
	VCN      int64
	LCN      int64
	Clusters int64
	Sparse   bool
}

type ntfsData struct {
	resident  []byte
	runs      []ntfsRun
	realSize  int64
	allocated int64
	flags     uint16
}

type ntfsEntry struct {
	record     uint64
	parent     uint64
	name       string
	namespace  byte
	inUse      bool
	directory  bool
	data       ntfsData
	created    time.Time
	modified   time.Time
	deleted    bool
	parseNotes string
}

func recoverQuickNTFS(opts Options, reader io.ReaderAt, outDir string, session *SessionState, sessionPath string) (Result, error) {
	started := time.Now()
	rangeStart, rangeSize := normalizeRange(opts)
	vol, err := openNTFSVolume(reader, rangeStart, rangeSize)
	if err != nil {
		return Result{OutputDir: outDir}, err
	}
	logf(opts, fmt.Sprintf("NTFS detectado: cluster %s, registro MFT %s.", humanBytes(vol.clusterSize), humanBytes(vol.recordSize)))

	entries, invalid, err := scanMFT(opts, reader, vol, session, sessionPath)
	if err != nil {
		return Result{OutputDir: outDir, Duration: time.Since(started)}, err
	}
	logf(opts, fmt.Sprintf("MFT analisada: %d registro(s), %d registro(s) inválido(s).", len(entries), invalid))

	byRecord := make(map[uint64]*ntfsEntry, len(entries))
	for i := range entries {
		entry := &entries[i]
		byRecord[entry.record] = entry
	}

	deleted := make([]*ntfsEntry, 0)
	for i := range entries {
		e := &entries[i]
		if !e.inUse && !e.directory && e.name != "" && e.data.realSize > 0 && categoryAllowed(opts.Categories, filepath.Ext(e.name)) {
			if opts.MinFileSize > 0 && e.data.realSize < opts.MinFileSize {
				continue
			}
			deleted = append(deleted, e)
		}
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].record < deleted[j].record })
	logf(opts, fmt.Sprintf("Foram encontrados %d arquivo(s) excluído(s) com metadados recuperáveis.", len(deleted)))

	root := filepath.Join(outDir, "Recuperacao_Rapida_NTFS")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Result{OutputDir: outDir}, err
	}

	hashes := make(map[string]string)
	recovered := make([]RecoveredFile, 0, len(deleted))
	filesFound := 0
	for i, entry := range deleted {
		if err := waitControl(opts.Cancel, opts.Paused); err != nil {
			result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: len(deleted), RecoveredFiles: recovered, Duration: time.Since(started)}
			_ = writeAllReports(outDir, result, err)
			return result, err
		}

		originalPath := buildNTFSPath(entry, byRecord)
		dir, fileName := splitSafeOriginalPath(root, originalPath, entry.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		finalPath := uniquePath(filepath.Join(dir, fileName))
		tempPath := finalPath + ".part"
		hash, writeErr := writeNTFSData(reader, vol, entry.data, tempPath, opts.Cancel, opts.Paused, func(done int64) {
			if opts.Progress != nil {
				opts.Progress(Status{Phase: "quick_ntfs", BytesScanned: int64(i + 1), TotalBytes: int64(len(deleted)), CandidatesFound: len(deleted), CandidatesProcessed: i, TotalCandidates: len(deleted), FilesFound: filesFound, Current: "Recuperando pela MFT", CurrentFile: fileName, CurrentFileBytes: done, CurrentFileSize: entry.data.realSize})
			}
		})
		if writeErr != nil {
			_ = os.Remove(tempPath)
			continue
		}

		duplicateOf := ""
		if opts.SkipDuplicates && hash != "" {
			if existing, ok := hashes[hash]; ok {
				duplicateOf = existing
				_ = os.Remove(tempPath)
			}
		}
		storedPath := finalPath
		if duplicateOf == "" {
			if err := os.Rename(tempPath, finalPath); err != nil {
				_ = os.Remove(tempPath)
				continue
			}
			if hash != "" {
				hashes[hash] = finalPath
			}
			filesFound++
		} else {
			storedPath = duplicateOf
		}
		integrity, notes := validateRecoveredFile(storedPath, filepath.Ext(fileName), entry.data.realSize, duplicateOf != "")
		meta := RecoveredFile{
			Path: storedPath, OriginalName: entry.name, OriginalPath: originalPath,
			Extension: strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), "."), Category: categoryForExtension(filepath.Ext(fileName)),
			Size: entry.data.realSize, Integrity: integrity, HashSHA256: hash, DuplicateOf: duplicateOf,
			Method: "MFT NTFS", Deleted: true, CreatedAt: entry.created, ModifiedAt: entry.modified, Notes: strings.TrimSpace(entry.parseNotes + " " + notes),
		}
		recovered = append(recovered, meta)
		if duplicateOf == "" {
			logf(opts, fmt.Sprintf("MFT: %s (%s, %s)", originalPath, humanBytes(entry.data.realSize), integrity))
		}
		if session != nil && (i%50 == 0 || i+1 == len(deleted)) {
			session.Phase = "quick_extract"
			session.Processed = int64(i + 1)
			session.FilesFound = filesFound
			_ = saveSession(sessionPath, *session)
		}
	}

	result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: len(deleted), BytesScanned: vol.mftSize, Duration: time.Since(started), ScanDuration: time.Since(started), RecoveredFiles: recovered}
	if session != nil {
		session.Completed = true
		session.Phase = "completed"
		session.FilesFound = filesFound
		_ = saveSession(sessionPath, *session)
	}
	_ = writeAllReports(outDir, result, nil)
	return result, nil
}

func openNTFSVolume(r io.ReaderAt, start, size int64) (ntfsVolume, error) {
	boot := make([]byte, 512)
	if _, err := r.ReadAt(boot, start); err != nil && err != io.EOF {
		return ntfsVolume{}, err
	}
	if len(boot) < 80 || string(boot[3:11]) != "NTFS    " {
		return ntfsVolume{}, errors.New("a partição selecionada não possui um boot sector NTFS válido")
	}
	bps := int64(binary.LittleEndian.Uint16(boot[11:13]))
	spc := int64(boot[13])
	if bps < 512 || bps > 65536 || bps&(bps-1) != 0 || spc <= 0 {
		return ntfsVolume{}, errors.New("geometria NTFS inválida")
	}
	cluster := bps * spc
	mftLCN := int64(binary.LittleEndian.Uint64(boot[48:56]))
	clustersPerRecord := int8(boot[64])
	var recordSize int64
	if clustersPerRecord < 0 {
		shift := -clustersPerRecord
		if shift > 30 {
			return ntfsVolume{}, errors.New("tamanho de registro MFT inválido")
		}
		recordSize = int64(1) << shift
	} else {
		recordSize = int64(clustersPerRecord) * cluster
	}
	if recordSize < 512 || recordSize > 64*1024 || mftLCN <= 0 {
		return ntfsVolume{}, errors.New("parâmetros MFT inválidos")
	}
	vol := ntfsVolume{start: start, size: size, bytesSector: bps, clusterSize: cluster, recordSize: recordSize, mftLCN: mftLCN}
	record0 := make([]byte, recordSize)
	if _, err := r.ReadAt(record0, start+mftLCN*cluster); err != nil && err != io.EOF {
		return ntfsVolume{}, err
	}
	if err := applyNTFSFixup(record0, bps); err != nil {
		return ntfsVolume{}, fmt.Errorf("registro $MFT inválido: %w", err)
	}
	entry, err := parseMFTRecord(record0, 0, vol)
	if err != nil || len(entry.data.runs) == 0 || entry.data.realSize <= 0 {
		// Conservative contiguous fallback. It still handles common small volumes.
		availableClusters := (size - mftLCN*cluster) / cluster
		if availableClusters < 1 {
			return ntfsVolume{}, errors.New("$MFT fora da partição")
		}
		clusters := min64(availableClusters, 1024*1024*1024/cluster)
		vol.mftRuns = []ntfsRun{{VCN: 0, LCN: mftLCN, Clusters: clusters}}
		vol.mftSize = clusters * cluster
		return vol, nil
	}
	vol.mftRuns = entry.data.runs
	vol.mftSize = entry.data.realSize
	if vol.mftSize > size {
		vol.mftSize = size
	}
	return vol, nil
}

func scanMFT(opts Options, r io.ReaderAt, vol ntfsVolume, session *SessionState, sessionPath string) ([]ntfsEntry, int, error) {
	totalRecords := vol.mftSize / vol.recordSize
	if totalRecords <= 0 {
		return nil, 0, errors.New("MFT vazia")
	}
	if totalRecords > 20_000_000 {
		totalRecords = 20_000_000
	}
	entries := make([]ntfsEntry, 0, minIntLocal(int(totalRecords), 500000))
	buf := make([]byte, vol.recordSize)
	invalid := 0
	consecutiveInvalid := 0
	lastUpdate := time.Time{}
	for record := int64(0); record < totalRecords; record++ {
		if err := waitControl(opts.Cancel, opts.Paused); err != nil {
			return entries, invalid, err
		}
		for i := range buf {
			buf[i] = 0
		}
		if err := readNTFSStreamAt(r, vol, vol.mftRuns, record*vol.recordSize, buf); err != nil {
			invalid++
			consecutiveInvalid++
			if consecutiveInvalid > 8192 && record > 16384 {
				break
			}
			continue
		}
		if err := applyNTFSFixup(buf, vol.bytesSector); err != nil {
			invalid++
			consecutiveInvalid++
			if consecutiveInvalid > 8192 && record > 16384 {
				break
			}
			continue
		}
		entry, err := parseMFTRecord(buf, uint64(record), vol)
		if err != nil {
			invalid++
			consecutiveInvalid++
			continue
		}
		consecutiveInvalid = 0
		entries = append(entries, entry)
		now := time.Now()
		if opts.Progress != nil && (lastUpdate.IsZero() || now.Sub(lastUpdate) >= 250*time.Millisecond || record+1 == totalRecords) {
			opts.Progress(Status{Phase: "quick_ntfs_scan", BytesScanned: (record + 1) * vol.recordSize, TotalBytes: vol.mftSize, CandidatesFound: len(entries), Current: "Lendo registros da MFT"})
			lastUpdate = now
		}
		if session != nil && record%4096 == 0 {
			session.Phase = "quick_scan"
			session.ScanOffset = record * vol.recordSize
			session.CandidateCount = int64(len(entries))
			_ = saveSession(sessionPath, *session)
		}
	}
	return entries, invalid, nil
}

func applyNTFSFixup(record []byte, bytesPerSector int64) error {
	if len(record) < 48 || string(record[:4]) != "FILE" {
		return errors.New("assinatura FILE ausente")
	}
	usaOffset := int(binary.LittleEndian.Uint16(record[4:6]))
	usaCount := int(binary.LittleEndian.Uint16(record[6:8]))
	if usaOffset < 0 || usaCount < 2 || usaOffset+usaCount*2 > len(record) || bytesPerSector <= 0 {
		return errors.New("fixup inválido")
	}
	sequence := binary.LittleEndian.Uint16(record[usaOffset : usaOffset+2])
	for i := 1; i < usaCount; i++ {
		end := i*int(bytesPerSector) - 2
		if end < 0 || end+2 > len(record) {
			return errors.New("fixup fora do registro")
		}
		if binary.LittleEndian.Uint16(record[end:end+2]) != sequence {
			return errors.New("fixup não confere")
		}
		copy(record[end:end+2], record[usaOffset+i*2:usaOffset+i*2+2])
	}
	return nil
}

func parseMFTRecord(record []byte, fallbackRecord uint64, vol ntfsVolume) (ntfsEntry, error) {
	if len(record) < 48 || string(record[:4]) != "FILE" {
		return ntfsEntry{}, errors.New("registro FILE inválido")
	}
	flags := binary.LittleEndian.Uint16(record[22:24])
	recordNum := fallbackRecord
	if len(record) >= 48 {
		n := binary.LittleEndian.Uint32(record[44:48])
		if n != 0 || fallbackRecord == 0 {
			recordNum = uint64(n)
		}
	}
	entry := ntfsEntry{record: recordNum, inUse: flags&1 != 0, directory: flags&2 != 0, deleted: flags&1 == 0}
	attrOffset := int(binary.LittleEndian.Uint16(record[20:22]))
	if attrOffset < 24 || attrOffset >= len(record) {
		return ntfsEntry{}, errors.New("offset de atributo inválido")
	}
	bestNamespace := byte(255)
	for pos := attrOffset; pos+16 <= len(record); {
		attrType := binary.LittleEndian.Uint32(record[pos : pos+4])
		if attrType == ntfsAttrEnd {
			break
		}
		length := int(binary.LittleEndian.Uint32(record[pos+4 : pos+8]))
		if length < 16 || pos+length > len(record) {
			break
		}
		nonResident := record[pos+8] != 0
		nameLen := int(record[pos+9])
		nameOffset := int(binary.LittleEndian.Uint16(record[pos+10 : pos+12]))
		attrFlags := binary.LittleEndian.Uint16(record[pos+12 : pos+14])
		named := nameLen > 0 && nameOffset > 0

		if attrType == ntfsAttrFileName && !nonResident {
			valueLen := int(binary.LittleEndian.Uint32(record[pos+16 : pos+20]))
			valueOff := int(binary.LittleEndian.Uint16(record[pos+20 : pos+22]))
			if valueLen >= 66 && valueOff >= 0 && valueOff+valueLen <= length {
				v := record[pos+valueOff : pos+valueOff+valueLen]
				nameChars := int(v[64])
				namespace := v[65]
				if 66+nameChars*2 <= len(v) && fileNameNamespaceRank(namespace) < fileNameNamespaceRank(bestNamespace) {
					entry.parent = binary.LittleEndian.Uint64(v[:8]) & 0x0000FFFFFFFFFFFF
					entry.name = decodeNTFSName(v[66 : 66+nameChars*2])
					entry.namespace = namespace
					bestNamespace = namespace
					entry.created = filetimeToTime(binary.LittleEndian.Uint64(v[8:16]))
					entry.modified = filetimeToTime(binary.LittleEndian.Uint64(v[16:24]))
				}
			}
		}
		if attrType == ntfsAttrData && !named {
			if nonResident {
				if length >= 64 {
					runOff := int(binary.LittleEndian.Uint16(record[pos+32 : pos+34]))
					realSize := int64(binary.LittleEndian.Uint64(record[pos+48 : pos+56]))
					allocSize := int64(binary.LittleEndian.Uint64(record[pos+40 : pos+48]))
					if runOff > 0 && runOff < length {
						runs, err := decodeRunList(record[pos+runOff : pos+length])
						if err == nil {
							entry.data = ntfsData{runs: runs, realSize: realSize, allocated: allocSize, flags: attrFlags}
						}
					}
				}
			} else if length >= 24 {
				valueLen := int(binary.LittleEndian.Uint32(record[pos+16 : pos+20]))
				valueOff := int(binary.LittleEndian.Uint16(record[pos+20 : pos+22]))
				if valueLen >= 0 && valueOff >= 0 && valueOff+valueLen <= length {
					data := append([]byte(nil), record[pos+valueOff:pos+valueOff+valueLen]...)
					entry.data = ntfsData{resident: data, realSize: int64(valueLen), allocated: int64(valueLen), flags: attrFlags}
				}
			}
		}
		pos += length
	}
	if entry.name == "" && recordNum < 16 {
		entry.name = fmt.Sprintf("$Metarquivo_%d", recordNum)
	}
	return entry, nil
}

func decodeRunList(data []byte) ([]ntfsRun, error) {
	runs := make([]ntfsRun, 0, 8)
	var currentLCN int64
	var currentVCN int64
	for pos := 0; pos < len(data); {
		header := data[pos]
		pos++
		if header == 0 {
			break
		}
		lenBytes := int(header & 0x0F)
		offBytes := int(header >> 4)
		if lenBytes == 0 || lenBytes > 8 || offBytes > 8 || pos+lenBytes+offBytes > len(data) {
			return nil, errors.New("runlist inválida")
		}
		clusters := decodeUnsignedLE(data[pos : pos+lenBytes])
		pos += lenBytes
		if clusters <= 0 {
			return nil, errors.New("runlist com tamanho zero")
		}
		run := ntfsRun{VCN: currentVCN, Clusters: clusters}
		if offBytes == 0 {
			run.Sparse = true
			run.LCN = -1
		} else {
			delta := decodeSignedLE(data[pos : pos+offBytes])
			pos += offBytes
			currentLCN += delta
			if currentLCN < 0 {
				return nil, errors.New("LCN negativo")
			}
			run.LCN = currentLCN
		}
		runs = append(runs, run)
		currentVCN += clusters
	}
	if len(runs) == 0 {
		return nil, errors.New("runlist vazia")
	}
	return runs, nil
}

func decodeUnsignedLE(b []byte) int64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	if v > uint64(^uint64(0)>>1) {
		return -1
	}
	return int64(v)
}

func decodeSignedLE(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var v int64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | int64(b[i])
	}
	bits := uint(len(b) * 8)
	if b[len(b)-1]&0x80 != 0 && bits < 64 {
		v |= ^int64(0) << bits
	}
	return v
}

func readNTFSStreamAt(r io.ReaderAt, vol ntfsVolume, runs []ntfsRun, logicalOffset int64, p []byte) error {
	remaining := int64(len(p))
	written := int64(0)
	for remaining > 0 {
		vcn := logicalOffset / vol.clusterSize
		inCluster := logicalOffset % vol.clusterSize
		var run *ntfsRun
		for i := range runs {
			if vcn >= runs[i].VCN && vcn < runs[i].VCN+runs[i].Clusters {
				run = &runs[i]
				break
			}
		}
		if run == nil {
			return io.ErrUnexpectedEOF
		}
		clustersLeft := run.VCN + run.Clusters - vcn
		available := clustersLeft*vol.clusterSize - inCluster
		amount := min64(remaining, available)
		if run.Sparse {
			for i := int64(0); i < amount; i++ {
				p[written+i] = 0
			}
		} else {
			physical := vol.start + (run.LCN+(vcn-run.VCN))*vol.clusterSize + inCluster
			n, err := r.ReadAt(p[written:written+amount], physical)
			if n < int(amount) {
				return err
			}
		}
		logicalOffset += amount
		written += amount
		remaining -= amount
	}
	return nil
}

func writeNTFSData(r io.ReaderAt, vol ntfsVolume, data ntfsData, path string, cancel <-chan struct{}, paused func() bool, progress func(int64)) (string, error) {
	if data.flags&0x4000 != 0 {
		return "", errors.New("arquivo NTFS criptografado")
	}
	if data.flags&0x0001 != 0 {
		return "", errors.New("arquivo NTFS compactado ainda não suportado")
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	writer := io.MultiWriter(f, h)
	if data.resident != nil {
		if _, err := writer.Write(data.resident); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	if len(data.runs) == 0 || data.realSize <= 0 {
		return "", errors.New("dados NTFS ausentes")
	}
	buf := make([]byte, 4*1024*1024)
	var done int64
	for done < data.realSize {
		if err := waitControl(cancel, paused); err != nil {
			return "", err
		}
		want := int64(len(buf))
		if data.realSize-done < want {
			want = data.realSize - done
		}
		if err := readNTFSStreamAt(r, vol, data.runs, done, buf[:want]); err != nil {
			return "", err
		}
		if _, err := writer.Write(buf[:want]); err != nil {
			return "", err
		}
		done += want
		if progress != nil {
			progress(done)
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func buildNTFSPath(entry *ntfsEntry, byRecord map[uint64]*ntfsEntry) string {
	parts := []string{sanitizeName(entry.name)}
	seen := map[uint64]bool{entry.record: true}
	parent := entry.parent
	for depth := 0; depth < 64 && parent > 5; depth++ {
		if seen[parent] {
			break
		}
		seen[parent] = true
		p := byRecord[parent]
		if p == nil || p.name == "" {
			break
		}
		parts = append(parts, sanitizeName(p.name))
		parent = p.parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return filepath.Join(parts...)
}

func splitSafeOriginalPath(root, originalPath, fallback string) (string, string) {
	clean := filepath.Clean(originalPath)
	parts := strings.Split(clean, string(filepath.Separator))
	if len(parts) == 0 {
		return root, sanitizeName(fallback)
	}
	fileName := sanitizeName(parts[len(parts)-1])
	if fileName == "" {
		fileName = sanitizeName(fallback)
	}
	dir := root
	for _, part := range parts[:len(parts)-1] {
		part = sanitizeName(part)
		if part != "" && part != "." && part != ".." {
			dir = filepath.Join(dir, part)
		}
	}
	return dir, fileName
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		default:
			if r < 32 {
				return '_'
			}
			return r
		}
	}, name)
	name = strings.TrimRight(name, ". ")
	if len([]rune(name)) > 180 {
		r := []rune(name)
		name = string(r[:180])
	}
	return name
}

func decodeNTFSName(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(u))
}

func fileNameNamespaceRank(ns byte) int {
	switch ns {
	case 1, 3:
		return 0
	case 0:
		return 1
	case 2:
		return 2
	default:
		return 3
	}
}

func filetimeToTime(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	const ticksBetween1601And1970 = 116444736000000000
	if v < ticksBetween1601And1970 {
		return time.Time{}
	}
	nanos := int64(v-ticksBetween1601And1970) * 100
	return time.Unix(0, nanos).UTC()
}

func categoryAllowed(c Categories, ext string) bool {
	cat := categoryForExtension(ext)
	switch cat {
	case "Fotos":
		return c.Images
	case "Vídeos":
		return c.Videos
	case "Áudios":
		return c.Audio
	case "Compactados":
		return c.Archives
	default:
		return c.Documents
	}
}

func categoryForExtension(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	switch ext {
	case "jpg", "jpeg", "png", "gif", "bmp", "webp", "heic", "heif", "avif", "tif", "tiff", "raw", "dng":
		return "Fotos"
	case "mp4", "mov", "avi", "mkv", "3gp", "wmv", "webm", "mts", "m2ts":
		return "Vídeos"
	case "mp3", "wav", "flac", "aac", "ogg", "m4a", "wma":
		return "Áudios"
	case "zip", "7z", "rar", "tar", "gz", "bz2", "apk", "jar":
		return "Compactados"
	default:
		return "Documentos"
	}
}

// Keep bufio imported for compatibility with future incremental MFT indexes.
var _ = bufio.ErrInvalidUnreadByte
