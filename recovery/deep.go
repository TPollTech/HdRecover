package recovery

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func recoverDeep(opts Options) (Result, error) {
	started := time.Now()
	outDir, session, sessionPath, resumed, err := prepareSession(opts)
	if err != nil {
		return Result{}, err
	}

	source, err := openSource(opts.SourcePath)
	if err != nil {
		return Result{OutputDir: outDir}, fmt.Errorf("não foi possível abrir o disco. Execute como administrador: %w", err)
	}
	defer source.Close()
	aligned := newAlignedReaderAt(source, opts.SourceSize, opts.SectorSize)
	adaptive := newAdaptiveReaderAt(aligned, aligned.sector, opts.DamageMode, opts.Cancel, opts.Paused)
	reader := newCachedReaderAt(adaptive, opts.SourceSize, 2*1024*1024, 8)

	kinds := enabledKinds(opts.Categories)
	if len(kinds) == 0 {
		return Result{OutputDir: outDir}, errors.New("selecione ao menos um tipo de arquivo")
	}
	folders := make(map[string]string)
	for _, kind := range kinds {
		folder := filepath.Join(outDir, kind.folder)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return Result{OutputDir: outDir}, err
		}
		folders[kind.folder] = folder
	}

	rangeStart, rangeSize := normalizeRange(opts)
	scanRanges := []ByteRange{{Start: rangeStart, Size: rangeSize}}
	if opts.OnlyFreeSpace {
		if !strings.EqualFold(opts.Partition.FileSystem, "NTFS") {
			return Result{OutputDir: outDir}, errors.New("a opção de espaço livre exige uma partição NTFS")
		}
		freeRanges, freeErr := NTFSFreeRangesFromPath(opts.SourcePath, opts.SourceSize, opts.SectorSize, opts.Partition, opts.Cancel, opts.Paused)
		if freeErr != nil {
			return Result{OutputDir: outDir}, freeErr
		}
		scanRanges = freeRanges
		logf(opts, fmt.Sprintf("$Bitmap NTFS: %d região(ões) livres, total de %s para examinar.", len(scanRanges), humanBytes(totalRangeBytes(scanRanges))))
	}
	scanTotal := totalRangeBytes(scanRanges)
	if scanTotal <= 0 {
		return Result{OutputDir: outDir}, errors.New("não há espaço selecionado para analisar")
	}
	chunkSize, workers := performanceSettings(opts.Performance)
	groups := buildScanGroups(kinds)
	logf(opts, fmt.Sprintf("Verificação profunda: intervalo %s até %s (%s).", humanBytes(rangeStart), humanBytes(rangeStart+rangeSize), humanBytes(scanTotal)))
	logf(opts, fmt.Sprintf("Leitura adaptativa: %s. Blocos: %s. Análise: %d núcleo(s).", opts.DamageMode.String(), humanBytes(int64(chunkSize)), workers))
	if resumed {
		logf(opts, "Sessão interrompida localizada; retomando do último ponto seguro.")
	}

	scanStarted := time.Now()
	candidateCount := session.CandidateCount
	bytesScanned := session.ScanOffset
	if session.Phase != "sorted" && session.Phase != "extracting" {
		appendExisting := resumed && session.ScanOffset > 0
		store, err := openCandidateStore(session.CandidatesPath, appendExisting)
		if err != nil {
			return Result{OutputDir: outDir}, err
		}
		if store.Count() > candidateCount {
			candidateCount = store.Count()
		}
		startAt := session.ScanOffset
		if startAt > 128 {
			startAt -= 128
		}
		bytesScanned, err = scanRangesToStore(opts, adaptive, kinds, groups, chunkSize, workers, scanRanges, startAt, store, &session, sessionPath)
		candidateCount = store.Count()
		closeErr := store.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			result := Result{OutputDir: outDir, Candidates: int(candidateCount), BytesScanned: bytesScanned, ReadErrors: len(adaptive.BadRanges()), BadRanges: adaptive.BadRanges(), Duration: time.Since(started), ScanDuration: time.Since(scanStarted), Resumed: resumed}
			_ = writeAllReports(outDir, result, err)
			return result, err
		}
		session.Phase = "sorting"
		session.ScanOffset = scanTotal
		session.CandidateCount = candidateCount
		_ = saveSession(sessionPath, session)
	}

	if session.Phase != "sorted" && session.Phase != "extracting" {
		logf(opts, fmt.Sprintf("Ordenando %d candidato(s) em índice temporário no disco.", candidateCount))
		sortedCount, err := sortCandidateFile(session.CandidatesPath, session.SortedPath, filepath.Join(outDir, ".sort"), opts.Cancel, opts.Paused, func(done, total int64) {
			if opts.Progress != nil {
				opts.Progress(Status{Phase: "sorting", BytesScanned: done, TotalBytes: total, CandidatesFound: int(candidateCount), Current: "Ordenando candidatos sem carregar tudo na memória"})
			}
		})
		if err != nil {
			result := Result{OutputDir: outDir, Candidates: int(candidateCount), BytesScanned: bytesScanned, ReadErrors: len(adaptive.BadRanges()), BadRanges: adaptive.BadRanges(), Duration: time.Since(started), ScanDuration: time.Since(scanStarted), Resumed: resumed}
			_ = writeAllReports(outDir, result, err)
			return result, err
		}
		candidateCount = sortedCount
		session.Phase = "sorted"
		session.CandidateCount = sortedCount
		_ = saveSession(sessionPath, session)
	}

	scanDuration := time.Since(scanStarted)
	logf(opts, fmt.Sprintf("Índice concluído: %d candidato(s) único(s).", candidateCount))
	it, err := openCandidateIterator(session.SortedPath)
	if err != nil {
		return Result{OutputDir: outDir}, err
	}
	defer it.Close()

	processed := int64(0)
	for processed < session.Processed {
		if _, err := it.Next(); err != nil {
			break
		}
		processed++
	}
	filesFound := session.FilesFound
	recovered := loadPreviousRecovered(outDir)
	hashes := make(map[string]string, len(recovered))
	for _, item := range recovered {
		if item.HashSHA256 != "" && item.DuplicateOf == "" {
			hashes[item.HashSHA256] = item.Path
		}
	}

	extractStarted := time.Now()
	lastProgress := time.Time{}
	var skipUntil int64
	for {
		if err := waitControl(opts.Cancel, opts.Paused); err != nil {
			result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: int(candidateCount), BytesScanned: bytesScanned, ReadErrors: len(adaptive.BadRanges()), BadRanges: adaptive.BadRanges(), Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted), RecoveredFiles: recovered, Resumed: resumed}
			_ = writeAllReports(outDir, result, err)
			return result, err
		}
		candidate, nextErr := it.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return Result{OutputDir: outDir}, nextErr
		}
		processed++
		if candidate.kind < 0 || candidate.kind >= len(kinds) || candidate.offset < rangeStart || candidate.offset >= rangeStart+rangeSize || candidate.offset < skipUntil {
			reportExtractionProgress(opts, bytesScanned, len(adaptive.BadRanges()), int(candidateCount), int(processed), filesFound, &lastProgress, false)
			continue
		}
		kind := kinds[candidate.kind]
		length, ext, carveErr := kind.carveLen(reader, candidate.offset, rangeStart+rangeSize, opts.Cancel)
		if carveErr != nil || length <= 0 || opts.MinFileSize > 0 && length < opts.MinFileSize {
			continue
		}
		if ext == "" {
			ext = kind.ext
		}
		folder := folders[kind.folder]
		baseName := fmt.Sprintf("%s_%06d_offset_%012X", kind.name, filesFound+1, candidate.offset)
		tempPath := filepath.Join(folder, baseName+".part")
		finalPath := filepath.Join(folder, baseName+"."+ext)
		hash, copyErr := copyRangeHashed(reader, tempPath, candidate.offset, length, opts.Cancel, opts.Paused, func(copied int64) {
			if opts.Progress != nil {
				opts.Progress(Status{Phase: PhaseExtracting, BytesScanned: bytesScanned, TotalBytes: scanTotal, CandidatesFound: int(candidateCount), CandidatesProcessed: int(processed - 1), TotalCandidates: int(candidateCount), FilesFound: filesFound, ReadErrors: len(adaptive.BadRanges()), Current: "Copiando arquivo recuperável", CurrentFile: filepath.Base(finalPath), CurrentFileBytes: copied, CurrentFileSize: length})
			}
		})
		if copyErr != nil {
			_ = os.Remove(tempPath)
			if errors.Is(copyErr, errCancelled) {
				continue
			}
			continue
		}
		if kind.name == "arquivo" {
			if detected := classifyZip(tempPath); detected != "" {
				ext = detected
				finalPath = filepath.Join(folder, baseName+"."+ext)
			}
		} else if kind.name == "office" {
			if detected := classifyOLE(tempPath); detected != "" {
				ext = detected
				finalPath = filepath.Join(folder, baseName+"."+ext)
			}
		}
		duplicateOf := ""
		if opts.SkipDuplicates && hash != "" {
			if existing, ok := hashes[hash]; ok {
				duplicateOf = existing
				_ = os.Remove(tempPath)
			}
		}
		uniqueFinalPath := finalPath
		if duplicateOf == "" {
			uniqueFinalPath = uniquePath(finalPath)
			if err := os.Rename(tempPath, uniqueFinalPath); err != nil {
				_ = os.Remove(tempPath)
				continue
			}
			if hash != "" {
				hashes[hash] = uniqueFinalPath
			}
			filesFound++
		} else {
			uniqueFinalPath = duplicateOf
		}
		integrity, notes := validateRecoveredFile(uniqueFinalPath, ext, length, duplicateOf != "")
		recovered = append(recovered, RecoveredFile{Path: uniqueFinalPath, Extension: ext, Category: kind.folder, Size: length, SourceOffset: candidate.offset, Integrity: integrity, HashSHA256: hash, DuplicateOf: duplicateOf, Method: "Assinatura profunda", Deleted: true, Notes: notes})
		if duplicateOf == "" {
			logf(opts, fmt.Sprintf("Recuperado: %s (%s, %s)", filepath.Base(uniqueFinalPath), humanBytes(length), integrity))
		}
		if candidate.offset+length > skipUntil {
			skipUntil = candidate.offset + length
		}
		session.Phase = "extracting"
		session.Processed = processed
		session.FilesFound = filesFound
		session.ReadErrors = len(adaptive.BadRanges())
		if processed%50 == 0 || duplicateOf == "" {
			_ = saveSession(sessionPath, session)
			partial := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: int(candidateCount), BytesScanned: bytesScanned, ReadErrors: len(adaptive.BadRanges()), BadRanges: adaptive.BadRanges(), Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted), RecoveredFiles: recovered, Resumed: resumed}
			_ = writeJSONReport(outDir, partial, nil)
		}
		reportExtractionProgress(opts, bytesScanned, len(adaptive.BadRanges()), int(candidateCount), int(processed), filesFound, &lastProgress, true)
	}

	result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: int(candidateCount), BytesScanned: scanTotal, ReadErrors: len(adaptive.BadRanges()), BadRanges: adaptive.BadRanges(), Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted), RecoveredFiles: recovered, Resumed: resumed}
	session.Completed = true
	session.Phase = "completed"
	session.Processed = candidateCount
	session.FilesFound = filesFound
	session.ReadErrors = len(result.BadRanges)
	_ = saveSession(sessionPath, session)
	_ = writeAllReports(outDir, result, nil)
	appendHistory(opts.Destination, result, opts.Mode, opts.SourcePath)
	_ = os.Remove(session.CandidatesPath)
	_ = os.Remove(session.SortedPath)
	_ = os.RemoveAll(filepath.Join(outDir, ".sort"))
	return result, nil
}

func scanRangesToStore(opts Options, reader io.ReaderAt, kinds []fileKind, groups []scanGroup, chunkSize, workers int, ranges []ByteRange, resumeAt int64, store *candidateStore, session *SessionState, sessionPath string) (int64, error) {
	total := totalRangeBytes(ranges)
	var cumulative int64
	processed := resumeAt
	for _, scanRange := range ranges {
		if scanRange.Size <= 0 {
			continue
		}
		if resumeAt >= cumulative+scanRange.Size {
			cumulative += scanRange.Size
			continue
		}
		localResume := int64(0)
		if resumeAt > cumulative {
			localResume = resumeAt - cumulative
		}
		var err error
		processed, err = scanDiskToStore(opts, reader, kinds, groups, chunkSize, workers, scanRange.Start, scanRange.Size, localResume, cumulative, total, store, session, sessionPath)
		if err != nil {
			return processed, err
		}
		cumulative += scanRange.Size
		resumeAt = cumulative
	}
	return total, nil
}

func scanDiskToStore(opts Options, reader io.ReaderAt, kinds []fileKind, groups []scanGroup, chunkSize, workers int, rangeStart, rangeSize, resumeAt, progressBase, totalPlan int64, store *candidateStore, session *SessionState, sessionPath string) (int64, error) {
	const overlap = 128
	jobs := make(chan scanJob, workers)
	results := make(chan scanResult, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for job := range jobs {
				found := scanChunk(job.data, job.baseOffset, kinds, groups)
				results <- scanResult{candidates: found, newBytes: job.newBytes, buffer: job.buffer}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	bufferPool := sync.Pool{New: func() any { return make([]byte, chunkSize+overlap) }}
	errCh := make(chan error, 1)
	var processed int64 = progressBase + resumeAt
	lastProgress := time.Time{}
	go func() {
		var collectErr error
		for result := range results {
			if collectErr == nil {
				collectErr = store.Append(result.candidates)
			}
			processed += result.newBytes
			bufferPool.Put(result.buffer[:cap(result.buffer)])
			now := time.Now()
			if opts.Progress != nil && (lastProgress.IsZero() || now.Sub(lastProgress) >= 250*time.Millisecond || processed >= totalPlan) {
				opts.Progress(Status{Phase: PhaseScanning, BytesScanned: processed, TotalBytes: totalPlan, CandidatesFound: int(store.Count()), ReadErrors: 0, Current: "Analisando assinaturas e salvando índice de retomada"})
				lastProgress = now
			}
			if session != nil && (now.Sub(session.LastUpdate) >= 2*time.Second || processed >= totalPlan) {
				session.Phase = "scanning"
				session.ScanOffset = processed
				session.CandidateCount = store.Count()
				_ = store.Flush()
				_ = saveSession(sessionPath, *session)
			}
		}
		errCh <- collectErr
	}()

	offset := resumeAt
	carry := 0
	tail := make([]byte, overlap)
	var scanErr error
	for offset < rangeSize {
		if err := waitControl(opts.Cancel, opts.Paused); err != nil {
			scanErr = err
			break
		}
		raw := bufferPool.Get().([]byte)
		if cap(raw) < chunkSize+overlap {
			raw = make([]byte, chunkSize+overlap)
		}
		raw = raw[:chunkSize+overlap]
		if carry > 0 {
			copy(raw[:carry], tail[:carry])
		}
		toRead := int64(chunkSize)
		if rangeSize-offset < toRead {
			toRead = rangeSize - offset
		}
		n, readErr := reader.ReadAt(raw[carry:carry+int(toRead)], rangeStart+offset)
		if n <= 0 {
			bufferPool.Put(raw)
			if readErr != nil {
				offset += min64(toRead, 1024*1024)
				continue
			}
			break
		}
		total := carry + n
		baseOffset := rangeStart + offset - int64(carry)
		nextCarry := minInt(overlap, total)
		copy(tail[:nextCarry], raw[total-nextCarry:total])
		jobs <- scanJob{baseOffset: baseOffset, newBytes: int64(n), data: raw[:total], buffer: raw}
		offset += int64(n)
		carry = nextCarry
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	close(jobs)
	collectErr := <-errCh
	if scanErr != nil {
		return processed, scanErr
	}
	if collectErr != nil {
		return processed, collectErr
	}
	return processed, nil
}

func copyRangeHashed(r io.ReaderAt, path string, start, length int64, cancel <-chan struct{}, paused func() bool, progress func(int64)) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	writer := bufio.NewWriterSize(f, 8*1024*1024)
	h := sha256.New()
	multi := io.MultiWriter(writer, h)
	buf := make([]byte, 8*1024*1024)
	var copied int64
	for copied < length {
		if err := waitControl(cancel, paused); err != nil {
			return "", err
		}
		want := int64(len(buf))
		if length-copied < want {
			want = length - copied
		}
		n, err := r.ReadAt(buf[:want], start+copied)
		if n > 0 {
			if _, werr := multi.Write(buf[:n]); werr != nil {
				return "", werr
			}
			copied += int64(n)
			if progress != nil {
				progress(copied)
			}
		}
		if err != nil && n == 0 {
			return "", err
		}
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadPreviousRecovered(outDir string) []RecoveredFile {
	path := filepath.Join(outDir, "RELATORIO_DA_RECUPERACAO.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var payload struct {
		Result Result `json:"result"`
	}
	if jsonErr := json.Unmarshal(data, &payload); jsonErr != nil {
		return nil
	}
	return payload.Result.RecoveredFiles
}
