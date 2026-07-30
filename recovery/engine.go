package recovery

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

type Categories struct {
	Images    bool
	Documents bool
	Videos    bool
	Audio     bool
	Archives  bool
}

type PerformanceMode int

const (
	PerformanceAutomatic PerformanceMode = iota
	PerformanceCompatibility
)

const (
	PhaseScanning   = "scanning"
	PhaseExtracting = "extracting"
)

type Options struct {
	SourcePath     string
	SourceSize     int64
	SectorSize     int64
	Destination    string
	Categories     Categories
	Performance    PerformanceMode
	Mode           OperationMode
	Profile        RecoveryProfile
	DamageMode     DamageMode
	Partition      Partition
	RangeStart     int64
	RangeSize      int64
	MinFileSize    int64
	SkipDuplicates bool
	OnlyFreeSpace  bool
	Resume         bool
	ImageName      string
	Cancel         <-chan struct{}
	Paused         func() bool
	Progress       func(Status)
	Log            func(string)
}

type Status struct {
	Phase               string
	BytesScanned        int64
	TotalBytes          int64
	CandidatesFound     int
	CandidatesProcessed int
	TotalCandidates     int
	FilesFound          int
	ReadErrors          int
	Current             string
	CurrentFile         string
	CurrentFileBytes    int64
	CurrentFileSize     int64
	Paused              bool
	BadBytes            int64
}

type Result struct {
	OutputDir          string
	FilesFound         int
	Candidates         int
	BytesScanned       int64
	ReadErrors         int
	Duration           time.Duration
	ScanDuration       time.Duration
	ExtractionDuration time.Duration
	ImagePath          string
	BadRanges          []BadRange
	RecoveredFiles     []RecoveredFile
	Resumed            bool
}

type fileKind struct {
	name       string
	ext        string
	folder     string
	scanSig    []byte
	scanAdjust int
	enabled    func(Categories) bool
	detect     func([]byte, int) bool
	carveLen   func(io.ReaderAt, int64, int64, <-chan struct{}) (int64, string, error)
}

type scanCandidate struct {
	offset int64
	kind   int
}

type scanGroup struct {
	signature []byte
	adjust    int
	kinds     []int
}

type scanJob struct {
	baseOffset int64
	newBytes   int64
	data       []byte
	buffer     []byte
}

type scanResult struct {
	candidates []scanCandidate
	newBytes   int64
	buffer     []byte
}

var errCancelled = errors.New("operação cancelada")

func Recover(opts Options) (Result, error) {
	opts = applyProfileDefaults(opts)
	if opts.SourcePath == "" || opts.SourceSize <= 0 {
		return Result{}, errors.New("disco de origem inválido")
	}
	if opts.Destination == "" {
		return Result{}, errors.New("pasta de destino não informada")
	}
	rangeStart, rangeSize := normalizeRange(opts)
	if rangeSize <= 0 || rangeStart < 0 || rangeStart+rangeSize > opts.SourceSize {
		return Result{}, errors.New("intervalo de análise inválido")
	}
	opts.RangeStart, opts.RangeSize = rangeStart, rangeSize

	if opts.Mode == ModeDeepCarving {
		return recoverDeep(opts)
	}

	outDir, session, sessionPath, resumed, err := prepareSession(opts)
	if err != nil {
		return Result{}, err
	}
	source, err := openSource(opts.SourcePath)
	if err != nil {
		return Result{OutputDir: outDir}, fmt.Errorf("não foi possível abrir a origem em modo somente leitura: %w", err)
	}
	defer source.Close()
	aligned := newAlignedReaderAt(source, opts.SourceSize, opts.SectorSize)
	adaptive := newAdaptiveReaderAt(aligned, aligned.sector, opts.DamageMode, opts.Cancel, opts.Paused)
	reader := newCachedReaderAt(adaptive, opts.SourceSize, 2*1024*1024, 8)

	var result Result
	switch opts.Mode {
	case ModeQuickNTFS:
		result, err = recoverQuickNTFS(opts, reader, outDir, &session, sessionPath)
	case ModeCreateImage:
		result, err = createDiskImage(opts, reader, adaptive, outDir, &session, sessionPath)
	default:
		err = errors.New("modo de operação inválido")
	}
	result.Resumed = resumed
	if len(result.BadRanges) == 0 {
		result.BadRanges = adaptive.BadRanges()
		result.ReadErrors = len(result.BadRanges)
	}
	_ = writeAllReports(outDir, result, err)
	appendHistory(opts.Destination, result, opts.Mode, opts.SourcePath)
	return result, err
}

func applyProfileDefaults(opts Options) Options {
	switch opts.Profile {
	case ProfilePhotos:
		opts.Categories = Categories{Images: true}
	case ProfileDocuments:
		opts.Categories = Categories{Documents: true, Archives: true}
	case ProfileVideos:
		opts.Categories = Categories{Videos: true, Audio: true}
	case ProfileFormattedDrive:
		opts.Mode = ModeDeepCarving
		if opts.MinFileSize == 0 {
			opts.MinFileSize = 4 * 1024
		}
	case ProfileDamagedDrive:
		opts.DamageMode = DamageCareful
		if opts.Performance == PerformanceAutomatic {
			opts.Performance = PerformanceCompatibility
		}
	default:
		if !opts.Categories.Images && !opts.Categories.Documents && !opts.Categories.Videos && !opts.Categories.Audio && !opts.Categories.Archives {
			opts.Categories = Categories{Images: true, Documents: true, Videos: true, Audio: true, Archives: true}
		}
	}
	return opts
}

func normalizeRange(opts Options) (int64, int64) {
	start := opts.RangeStart
	size := opts.RangeSize
	if opts.Partition.Size > 0 {
		start = opts.Partition.Start
		size = opts.Partition.Size
	}
	if size <= 0 {
		size = opts.SourceSize - start
	}
	if start < 0 {
		start = 0
	}
	if start > opts.SourceSize {
		start = opts.SourceSize
	}
	if start+size > opts.SourceSize {
		size = opts.SourceSize - start
	}
	return start, size
}

func configSignature(opts Options) string {
	mask := 0
	if opts.Categories.Images {
		mask |= 1
	}
	if opts.Categories.Documents {
		mask |= 2
	}
	if opts.Categories.Videos {
		mask |= 4
	}
	if opts.Categories.Audio {
		mask |= 8
	}
	if opts.Categories.Archives {
		mask |= 16
	}
	return fmt.Sprintf("m%d-p%d-d%d-c%d-perf%d-min%d-dedup%t-free%t", opts.Mode, opts.Profile, opts.DamageMode, mask, opts.Performance, opts.MinFileSize, opts.SkipDuplicates, opts.OnlyFreeSpace)
}

func prepareSession(opts Options) (string, SessionState, string, bool, error) {
	rangeStart, rangeSize := normalizeRange(opts)
	if opts.Resume {
		if existing, path, err := FindResumableSession(opts.Destination, opts.SourcePath, opts.SourceSize, rangeStart, rangeSize, opts.Mode, configSignature(opts)); err == nil {
			if existing.OutputDir != "" {
				return existing.OutputDir, existing, path, true, nil
			}
		}
	}
	sessionID := time.Now().Format("20060102_150405")
	outDir := filepath.Join(opts.Destination, "HdRecover_"+sessionID)
	for n := 2; ; n++ {
		if _, err := os.Stat(outDir); os.IsNotExist(err) {
			break
		}
		outDir = filepath.Join(opts.Destination, fmt.Sprintf("HdRecover_%s_%d", sessionID, n))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", SessionState{}, "", false, fmt.Errorf("não foi possível criar a pasta de saída: %w", err)
	}
	state := SessionState{
		Version: 2, SessionID: sessionID, Mode: opts.Mode, SourcePath: opts.SourcePath,
		SourceSize: opts.SourceSize, SectorSize: opts.SectorSize, RangeStart: rangeStart, RangeSize: rangeSize,
		Destination: opts.Destination, OutputDir: outDir, Phase: "created",
		CandidatesPath: filepath.Join(outDir, ".candidates.bin"), SortedPath: filepath.Join(outDir, ".candidates.sorted.bin"),
		ConfigSignature: configSignature(opts),
	}
	path := filepath.Join(outDir, sessionFileName)
	if err := saveSession(path, state); err != nil {
		return "", SessionState{}, "", false, err
	}
	return outDir, state, path, false, nil
}

func recoverDeepLegacy(opts Options) (Result, error) {
	started := time.Now()
	if opts.SourcePath == "" || opts.SourceSize <= 0 {
		return Result{}, errors.New("disco de origem inválido")
	}
	if opts.Destination == "" {
		return Result{}, errors.New("pasta de destino não informada")
	}

	outDir := filepath.Join(opts.Destination, "HdRecover_"+time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("não foi possível criar a pasta de saída: %w", err)
	}

	source, err := openSource(opts.SourcePath)
	if err != nil {
		return Result{}, fmt.Errorf("não foi possível abrir o disco. Execute como administrador: %w", err)
	}
	defer source.Close()

	aligned := newAlignedReaderAt(source, opts.SourceSize, opts.SectorSize)
	reader := newCachedReaderAt(aligned, opts.SourceSize, 2*1024*1024, 8)

	kinds := enabledKinds(opts.Categories)
	if len(kinds) == 0 {
		return Result{OutputDir: outDir}, errors.New("selecione ao menos um tipo de arquivo")
	}

	folders := make(map[string]string)
	for _, kind := range kinds {
		if _, exists := folders[kind.folder]; exists {
			continue
		}
		folder := filepath.Join(outDir, kind.folder)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return Result{OutputDir: outDir}, fmt.Errorf("não foi possível criar pasta para %s: %w", kind.folder, err)
		}
		folders[kind.folder] = folder
	}

	chunkSize, workers := performanceSettings(opts.Performance)
	groups := buildScanGroups(kinds)
	logf(opts, "Iniciando análise otimizada em duas etapas, sempre em modo somente leitura.")
	logf(opts, fmt.Sprintf("Etapa 1/2: varredura sequencial com blocos de %s e %d núcleo(s) de análise.", humanBytes(int64(chunkSize)), workers))
	logf(opts, fmt.Sprintf("Setor lógico usado para leitura: %d bytes.", aligned.sector))
	logf(opts, "Arquivos recuperados serão gravados em: "+outDir)

	scanStarted := time.Now()
	candidates, bytesScanned, readErrors, err := scanDisk(opts, aligned, kinds, groups, chunkSize, workers)
	scanDuration := time.Since(scanStarted)
	if err != nil {
		result := Result{OutputDir: outDir, Candidates: len(candidates), BytesScanned: bytesScanned, ReadErrors: readErrors, Duration: time.Since(started), ScanDuration: scanDuration}
		_ = writeReport(outDir, result, err)
		return result, err
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].offset == candidates[j].offset {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].offset < candidates[j].offset
	})
	candidates = dedupeCandidates(candidates)
	logf(opts, fmt.Sprintf("Etapa 1 concluída em %s: %d candidato(s) estruturalmente válido(s).", formatDuration(scanDuration), len(candidates)))
	logf(opts, "Etapa 2/2: validando e extraindo os arquivos em ordem física para reduzir movimentos do HD.")

	extractStarted := time.Now()
	filesFound := 0
	processed := 0
	skipUntil := int64(0)
	lastProgress := time.Time{}

	for _, candidate := range candidates {
		if cancelled(opts.Cancel) {
			result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: len(candidates), BytesScanned: bytesScanned, ReadErrors: readErrors, Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted)}
			_ = writeReport(outDir, result, errCancelled)
			return result, errCancelled
		}
		processed++
		if candidate.offset < 0 || candidate.offset >= opts.SourceSize || candidate.offset < skipUntil || candidate.kind < 0 || candidate.kind >= len(kinds) {
			reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), processed, filesFound, &lastProgress, false)
			continue
		}

		kind := kinds[candidate.kind]
		length, ext, carveErr := kind.carveLen(reader, candidate.offset, opts.SourceSize, opts.Cancel)
		if carveErr != nil || length <= 0 {
			if errors.Is(carveErr, errCancelled) {
				result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: len(candidates), BytesScanned: bytesScanned, ReadErrors: readErrors, Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted)}
				_ = writeReport(outDir, result, errCancelled)
				return result, errCancelled
			}
			reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), processed, filesFound, &lastProgress, false)
			continue
		}
		if ext == "" {
			ext = kind.ext
		}

		folder := folders[kind.folder]
		baseName := fmt.Sprintf("%s_%06d_offset_%012X", kind.name, filesFound+1, candidate.offset)
		tempPath := filepath.Join(folder, baseName+".part")
		finalPath := filepath.Join(folder, baseName+"."+ext)
		copyName := baseName + "." + ext
		if err := copyRange(reader, tempPath, candidate.offset, length, opts.Cancel, func(copied int64) {
			if opts.Progress != nil {
				opts.Progress(Status{
					Phase: PhaseExtracting, BytesScanned: bytesScanned, TotalBytes: opts.SourceSize,
					CandidatesFound: len(candidates), CandidatesProcessed: processed - 1, TotalCandidates: len(candidates),
					FilesFound: filesFound, ReadErrors: readErrors, Current: "Copiando arquivo recuperável",
					CurrentFile: copyName, CurrentFileBytes: copied, CurrentFileSize: length,
				})
			}
		}); err != nil {
			_ = os.Remove(tempPath)
			if errors.Is(err, errCancelled) {
				result := Result{OutputDir: outDir, FilesFound: filesFound, Candidates: len(candidates), BytesScanned: bytesScanned, ReadErrors: readErrors, Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: time.Since(extractStarted)}
				_ = writeReport(outDir, result, errCancelled)
				return result, errCancelled
			}
			reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), processed, filesFound, &lastProgress, false)
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

		uniqueFinalPath := uniquePath(finalPath)
		if err := os.Rename(tempPath, uniqueFinalPath); err != nil {
			_ = os.Remove(tempPath)
			reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), processed, filesFound, &lastProgress, false)
			continue
		}
		filesFound++
		if candidate.offset+length > skipUntil {
			skipUntil = candidate.offset + length
		}
		logf(opts, fmt.Sprintf("Recuperado: %s (%s)", filepath.Base(uniqueFinalPath), humanBytes(length)))
		reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), processed, filesFound, &lastProgress, true)
	}

	extractionDuration := time.Since(extractStarted)
	result := Result{
		OutputDir: outDir, FilesFound: filesFound, Candidates: len(candidates), BytesScanned: bytesScanned,
		ReadErrors: readErrors, Duration: time.Since(started), ScanDuration: scanDuration, ExtractionDuration: extractionDuration,
	}
	reportExtractionProgress(opts, bytesScanned, readErrors, len(candidates), len(candidates), filesFound, &lastProgress, true)
	if readErrors > 0 {
		logf(opts, fmt.Sprintf("A leitura encontrou %d trecho(s) problemático(s). Alguns arquivos podem estar incompletos.", readErrors))
	}
	logf(opts, fmt.Sprintf("Recuperação concluída. %d arquivo(s) recuperado(s) em %s.", filesFound, formatDuration(result.Duration)))
	_ = writeReport(outDir, result, nil)
	return result, nil
}

func performanceSettings(mode PerformanceMode) (chunkSize, workers int) {
	if mode == PerformanceCompatibility {
		return 8 * 1024 * 1024, 1
	}
	workers = runtime.NumCPU() / 2
	if workers < 2 {
		workers = 2
	}
	if workers > 4 {
		workers = 4
	}
	return 32 * 1024 * 1024, workers
}

func buildScanGroups(kinds []fileKind) []scanGroup {
	groupIndex := make(map[string]int)
	groups := make([]scanGroup, 0, len(kinds))
	for i := range kinds {
		key := fmt.Sprintf("%d:%s", kinds[i].scanAdjust, string(kinds[i].scanSig))
		if existing, ok := groupIndex[key]; ok {
			groups[existing].kinds = append(groups[existing].kinds, i)
			continue
		}
		groupIndex[key] = len(groups)
		groups = append(groups, scanGroup{signature: kinds[i].scanSig, adjust: kinds[i].scanAdjust, kinds: []int{i}})
	}
	return groups
}

func scanDisk(opts Options, reader io.ReaderAt, kinds []fileKind, groups []scanGroup, chunkSize, workers int) ([]scanCandidate, int64, int, error) {
	const overlap = 128
	jobs := make(chan scanJob)
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
	go func() {
		wg.Wait()
		close(results)
	}()

	bufferPool := sync.Pool{New: func() any { return make([]byte, chunkSize+overlap) }}
	collectedCh := make(chan []scanCandidate, 1)
	var processedBytes int64
	var candidateCount int
	var progressMu sync.Mutex
	lastProgress := time.Time{}

	go func() {
		all := make([]scanCandidate, 0, 4096)
		for result := range results {
			processedBytes += result.newBytes
			candidateCount += len(result.candidates)
			all = append(all, result.candidates...)
			bufferPool.Put(result.buffer[:cap(result.buffer)])

			progressMu.Lock()
			now := time.Now()
			force := processedBytes >= opts.SourceSize
			if force || lastProgress.IsZero() || now.Sub(lastProgress) >= 200*time.Millisecond {
				lastProgress = now
				if opts.Progress != nil {
					opts.Progress(Status{
						Phase: PhaseScanning, BytesScanned: processedBytes, TotalBytes: opts.SourceSize,
						CandidatesFound: candidateCount, Current: "Analisando assinaturas em fluxo sequencial",
					})
				}
			}
			progressMu.Unlock()
		}
		collectedCh <- all
	}()

	var offset int64
	carry := 0
	tail := make([]byte, overlap)
	readErrors := 0
	lastReadErrorLog := int64(-1)
	var scanErr error

	for offset < opts.SourceSize {
		if cancelled(opts.Cancel) {
			scanErr = errCancelled
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
		if remaining := opts.SourceSize - offset; remaining < toRead {
			toRead = remaining
		}
		n, readErr := reader.ReadAt(raw[carry:carry+int(toRead)], offset)
		if n == 0 && readErr != nil {
			bufferPool.Put(raw)
			readErrors++
			skip := alignUp(1024*1024, 512)
			if lastReadErrorLog < 0 || offset-lastReadErrorLog >= 64*1024*1024 {
				logf(opts, fmt.Sprintf("Falha de leitura próximo de %.2f GB (%v). Pulando %s e continuando.", float64(offset)/(1024*1024*1024), readErr, humanBytes(skip)))
				lastReadErrorLog = offset
			}
			offset += skip
			carry = 0
			continue
		}

		total := carry + n
		baseOffset := offset - int64(carry)
		nextCarry := minInt(overlap, total)
		if nextCarry > 0 {
			copy(tail[:nextCarry], raw[total-nextCarry:total])
		}
		jobs <- scanJob{baseOffset: baseOffset, newBytes: int64(n), data: raw[:total], buffer: raw}
		offset += int64(n)
		carry = nextCarry

		if readErr != nil && readErr != io.EOF {
			readErrors++
			if lastReadErrorLog < 0 || offset-lastReadErrorLog >= 64*1024*1024 {
				logf(opts, fmt.Sprintf("Leitura parcial próximo de %.2f GB (%v).", float64(offset)/(1024*1024*1024), readErr))
				lastReadErrorLog = offset
			}
		}
		if readErr == io.EOF {
			break
		}
	}

	close(jobs)
	candidates := <-collectedCh
	if scanErr != nil {
		return candidates, processedBytes, readErrors, scanErr
	}
	if opts.Progress != nil {
		opts.Progress(Status{Phase: PhaseScanning, BytesScanned: processedBytes, TotalBytes: opts.SourceSize, CandidatesFound: len(candidates), ReadErrors: readErrors, Current: "Varredura sequencial concluída"})
	}
	return candidates, processedBytes, readErrors, nil
}

func scanChunk(data []byte, baseOffset int64, kinds []fileKind, groups []scanGroup) []scanCandidate {
	if len(data) == 0 || allZero(data) {
		return nil
	}
	found := make([]scanCandidate, 0, 64)
	// bytes.Index usa rotinas altamente otimizadas do runtime. Os blocos são
	// distribuídos entre vários núcleos, mantendo o disco em leitura sequencial.
	for _, group := range groups {
		searchFrom := 0
		for searchFrom < len(data) {
			relative := bytes.Index(data[searchFrom:], group.signature)
			if relative < 0 {
				break
			}
			match := searchFrom + relative
			candidateIndex := match + group.adjust
			if candidateIndex >= 0 && candidateIndex < len(data) {
				absolute := baseOffset + int64(candidateIndex)
				if absolute >= 0 {
					for _, kindIndex := range group.kinds {
						if kinds[kindIndex].detect(data, candidateIndex) {
							found = append(found, scanCandidate{offset: absolute, kind: kindIndex})
						}
					}
				}
			}
			searchFrom = match + 1
		}
	}
	return found
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func dedupeCandidates(in []scanCandidate) []scanCandidate {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, candidate := range in[1:] {
		previous := out[len(out)-1]
		if candidate.offset == previous.offset && candidate.kind == previous.kind {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func reportExtractionProgress(opts Options, bytesScanned int64, readErrors, total, processed, files int, last *time.Time, force bool) {
	if opts.Progress == nil {
		return
	}
	now := time.Now()
	if !force && !last.IsZero() && now.Sub(*last) < 200*time.Millisecond && processed%256 != 0 {
		return
	}
	*last = now
	opts.Progress(Status{
		Phase: PhaseExtracting, BytesScanned: bytesScanned, TotalBytes: opts.SourceSize,
		CandidatesFound: total, CandidatesProcessed: processed, TotalCandidates: total,
		FilesFound: files, ReadErrors: readErrors, Current: "Validando e extraindo arquivos",
	})
}

func writeReport(outDir string, result Result, runErr error) error {
	status := "Concluído"
	if runErr != nil {
		status = "Interrompido: " + runErr.Error()
	}
	var badBytes int64
	integrity := map[Integrity]int{}
	for _, r := range result.BadRanges {
		badBytes += r.Length
	}
	for _, f := range result.RecoveredFiles {
		integrity[f.Integrity]++
	}
	body := fmt.Sprintf(
		"HdRecover 0.4.0 - Relatório da sessão\r\n\r\n"+
			"Status: %s\r\nArquivos recuperados: %d\r\nCandidatos analisados: %d\r\nDados varridos/copied: %s\r\n"+
			"Falhas de leitura: %d\r\nBytes não lidos: %s\r\nSessão retomada: %t\r\n"+
			"Tempo da varredura: %s\r\nTempo da extração: %s\r\nTempo total: %s\r\n",
		status, result.FilesFound, result.Candidates, humanBytes(result.BytesScanned), result.ReadErrors,
		humanBytes(badBytes), result.Resumed, formatDuration(result.ScanDuration),
		formatDuration(result.ExtractionDuration), formatDuration(result.Duration),
	)
	if result.ImagePath != "" {
		body += "Imagem criada: " + result.ImagePath + "\r\n"
	}
	body += fmt.Sprintf("\r\nIntegridade estimada:\r\n- Íntegros: %d\r\n- Possivelmente íntegros: %d\r\n- Parciais: %d\r\n- Danificados: %d\r\n- Não validados: %d\r\n",
		integrity[IntegrityGood], integrity[IntegrityLikely], integrity[IntegrityPartial], integrity[IntegrityDamaged], integrity[IntegrityUnverified])
	body += "\r\nObservações:\r\n- A origem foi aberta somente para leitura.\r\n- A classificação de integridade é estrutural e não substitui abrir/testar o arquivo.\r\n- Arquivos sobrescritos, criptografados ou descartados por TRIM podem ser irrecuperáveis.\r\n- Consulte RESULTADOS.html, ARQUIVOS_RECUPERADOS.csv e o relatório JSON.\r\n"
	return os.WriteFile(filepath.Join(outDir, "RELATORIO_DA_RECUPERACAO.txt"), []byte(body), 0o644)
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	if h > 0 {
		return fmt.Sprintf("%dh %02dmin %02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dmin %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func enabledKinds(c Categories) []fileKind {
	all := []fileKind{
		{name: "foto", ext: "jpg", folder: "Fotos", scanSig: []byte{0xFF, 0xD8, 0xFF}, enabled: func(c Categories) bool { return c.Images }, detect: detectJPEG, carveLen: carveJPEG},
		{name: "imagem", ext: "png", folder: "Fotos", scanSig: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, enabled: func(c Categories) bool { return c.Images }, detect: detectPrefix([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}), carveLen: carvePNG},
		{name: "imagem", ext: "bmp", folder: "Fotos", scanSig: []byte("BM"), enabled: func(c Categories) bool { return c.Images }, detect: detectBMP, carveLen: carveBMP},
		{name: "imagem", ext: "gif", folder: "Fotos", scanSig: []byte("GIF8"), enabled: func(c Categories) bool { return c.Images }, detect: detectGIF, carveLen: carveGIF},
		{name: "imagem", ext: "webp", folder: "Fotos", scanSig: []byte("RIFF"), enabled: func(c Categories) bool { return c.Images }, detect: detectRIFF("WEBP"), carveLen: carveRIFF},
		{name: "documento", ext: "pdf", folder: "Documentos", scanSig: []byte("%PDF-"), enabled: func(c Categories) bool { return c.Documents }, detect: detectPrefix([]byte("%PDF-")), carveLen: carvePDF},
		{name: "office", ext: "ole", folder: "Documentos", scanSig: []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, enabled: func(c Categories) bool { return c.Documents }, detect: detectPrefix([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}), carveLen: carveOLE},
		{name: "arquivo", ext: "zip", folder: "Documentos_e_Compactados", scanSig: []byte{'P', 'K', 0x03, 0x04}, enabled: func(c Categories) bool { return c.Documents || c.Archives }, detect: detectZIP, carveLen: carveZIP},
		{name: "arquivo", ext: "7z", folder: "Documentos_e_Compactados", scanSig: []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}, enabled: func(c Categories) bool { return c.Archives }, detect: detectPrefix([]byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}), carveLen: carve7Z},
		{name: "video", ext: "avi", folder: "Videos", scanSig: []byte("RIFF"), enabled: func(c Categories) bool { return c.Videos }, detect: detectRIFF("AVI "), carveLen: carveRIFF},
		{name: "audio", ext: "wav", folder: "Audios", scanSig: []byte("RIFF"), enabled: func(c Categories) bool { return c.Audio }, detect: detectRIFF("WAVE"), carveLen: carveRIFF},
		{name: "imagem", ext: "heic", folder: "Fotos", scanSig: []byte("ftyp"), scanAdjust: -4, enabled: func(c Categories) bool { return c.Images }, detect: detectISOBMFFImage, carveLen: carveMP4},
		{name: "video", ext: "mp4", folder: "Videos", scanSig: []byte("ftyp"), scanAdjust: -4, enabled: func(c Categories) bool { return c.Videos }, detect: detectISOBMFFVideo, carveLen: carveMP4},
		{name: "banco", ext: "sqlite", folder: "Bancos_de_dados", scanSig: []byte("SQLite format 3\x00"), enabled: func(c Categories) bool { return c.Documents }, detect: detectPrefix([]byte("SQLite format 3\x00")), carveLen: carveSQLite},
		{name: "audio", ext: "mp3", folder: "Audios", scanSig: []byte("ID3"), enabled: func(c Categories) bool { return c.Audio }, detect: detectPrefix([]byte("ID3")), carveLen: carveMP3},
	}
	out := make([]fileKind, 0, len(all))
	for _, k := range all {
		if k.enabled(c) {
			out = append(out, k)
		}
	}
	return out
}

func detectPrefix(prefix []byte) func([]byte, int) bool {
	return func(data []byte, i int) bool {
		return i+len(prefix) <= len(data) && bytes.Equal(data[i:i+len(prefix)], prefix)
	}
}

func detectJPEG(data []byte, i int) bool {
	if i+4 > len(data) || data[i] != 0xFF || data[i+1] != 0xD8 || data[i+2] != 0xFF {
		return false
	}
	marker := data[i+3]
	return marker == 0xDB || marker == 0xE0 || (marker >= 0xE1 && marker <= 0xEF) || isJPEGSOF(marker)
}

func detectGIF(data []byte, i int) bool {
	return i+6 <= len(data) && (string(data[i:i+6]) == "GIF87a" || string(data[i:i+6]) == "GIF89a")
}

func detectBMP(data []byte, i int) bool {
	if i+54 > len(data) || data[i] != 'B' || data[i+1] != 'M' {
		return false
	}
	fileSize := binary.LittleEndian.Uint32(data[i+2 : i+6])
	pixelOffset := binary.LittleEndian.Uint32(data[i+10 : i+14])
	dibSize := binary.LittleEndian.Uint32(data[i+14 : i+18])
	validDIB := dibSize == 12 || dibSize == 40 || dibSize == 52 || dibSize == 56 || dibSize == 108 || dibSize == 124
	return validDIB && fileSize >= 54 && fileSize <= 512*1024*1024 && pixelOffset >= 14 && pixelOffset < fileSize
}

func detectZIP(data []byte, i int) bool {
	if i+30 > len(data) || !bytes.Equal(data[i:i+4], []byte{'P', 'K', 0x03, 0x04}) {
		return false
	}
	version := binary.LittleEndian.Uint16(data[i+4 : i+6])
	method := binary.LittleEndian.Uint16(data[i+8 : i+10])
	nameLen := binary.LittleEndian.Uint16(data[i+26 : i+28])
	extraLen := binary.LittleEndian.Uint16(data[i+28 : i+30])
	validMethod := method == 0 || method == 1 || method == 6 || method == 8 || method == 9 || method == 12 || method == 14 || method == 93 || method == 98
	return version >= 10 && version <= 100 && validMethod && nameLen > 0 && nameLen <= 4096 && int(nameLen)+int(extraLen) <= 65535
}

func detectRIFF(form string) func([]byte, int) bool {
	return func(data []byte, i int) bool {
		return i+12 <= len(data) && string(data[i:i+4]) == "RIFF" && string(data[i+8:i+12]) == form
	}
}

func detectMP4(data []byte, i int) bool {
	if i < 0 || i+12 > len(data) || string(data[i+4:i+8]) != "ftyp" {
		return false
	}
	size := binary.BigEndian.Uint32(data[i : i+4])
	return size >= 8 && size <= 1024*1024
}

func isoBMFFBrand(data []byte, i int) string {
	if !detectMP4(data, i) || i+12 > len(data) {
		return ""
	}
	return string(data[i+8 : i+12])
}

func isISOBMFFImageBrand(brand string) bool {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1", "avif", "avis":
		return true
	default:
		return false
	}
}

func detectISOBMFFImage(data []byte, i int) bool {
	return isISOBMFFImageBrand(isoBMFFBrand(data, i))
}

func detectISOBMFFVideo(data []byte, i int) bool {
	return detectMP4(data, i) && !isISOBMFFImageBrand(isoBMFFBrand(data, i))
}

func carveJPEG(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	limit := min64(diskSize, start+256*1024*1024)
	pos := start + 2
	hasSOF := false
	hasSOS := false
	markerHeader := make([]byte, 4)

	// Valida a estrutura dos segmentos antes de procurar o EOI. Isso evita
	// transformar sequências aleatórias FF D8 FF em milhares de falsos JPGs.
	for pos+4 <= limit && pos-start < 4*1024*1024 {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		if _, err := r.ReadAt(markerHeader[:2], pos); err != nil {
			return 0, "", err
		}
		if markerHeader[0] != 0xFF {
			return 0, "", errors.New("segmento JPEG inválido")
		}
		marker := markerHeader[1]
		for marker == 0xFF {
			pos++
			if _, err := r.ReadAt(markerHeader[:2], pos); err != nil {
				return 0, "", err
			}
			marker = markerHeader[1]
		}
		if marker == 0xD9 {
			if hasSOF && hasSOS {
				return pos + 2 - start, "jpg", nil
			}
			return 0, "", errors.New("JPEG terminou antes dos dados")
		}
		if marker == 0x00 || marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			pos += 2
			continue
		}
		if _, err := r.ReadAt(markerHeader, pos); err != nil {
			return 0, "", err
		}
		segmentLen := int64(binary.BigEndian.Uint16(markerHeader[2:4]))
		if segmentLen < 2 || pos+2+segmentLen > limit {
			return 0, "", errors.New("tamanho de segmento JPEG inválido")
		}
		if isJPEGSOF(marker) {
			hasSOF = true
		}
		if marker == 0xDA { // Start of Scan
			hasSOS = true
			pos += 2 + segmentLen
			break
		}
		pos += 2 + segmentLen
	}
	if !hasSOF || !hasSOS {
		return 0, "", errors.New("cabeçalho JPEG incompleto")
	}
	end, err := findMarker(r, pos, limit, []byte{0xFF, 0xD9}, cancel)
	if err != nil || end-start < 128 {
		return 0, "", err
	}
	return end - start, "jpg", nil
}

func isJPEGSOF(marker byte) bool {
	switch marker {
	case 0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
		return true
	default:
		return false
	}
}

func carvePNG(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	marker := []byte{0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82}
	end, err := findMarker(r, start+8, min64(diskSize, start+256*1024*1024), marker, cancel)
	if err != nil || end-start < 64 {
		return 0, "", err
	}
	return end - start, "png", nil
}

func carveBMP(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	header := make([]byte, 18)
	if _, err := r.ReadAt(header, start); err != nil {
		return 0, "", err
	}
	if string(header[:2]) != "BM" {
		return 0, "", errors.New("BMP inválido")
	}
	size := int64(binary.LittleEndian.Uint32(header[2:6]))
	pixelOffset := binary.LittleEndian.Uint32(header[10:14])
	if size < 54 || size > 512*1024*1024 || pixelOffset < 14 || start+size > diskSize {
		return 0, "", errors.New("tamanho BMP inválido")
	}
	return size, "bmp", nil
}

func carveGIF(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	limit := min64(diskSize, start+512*1024*1024)
	header := make([]byte, 13)
	if _, err := r.ReadAt(header, start); err != nil || (!bytes.Equal(header[:6], []byte("GIF87a")) && !bytes.Equal(header[:6], []byte("GIF89a"))) {
		return 0, "", errors.New("GIF inválido")
	}
	pos := start + 13
	if header[10]&0x80 != 0 {
		tableSize := int64(3 * (1 << ((header[10] & 0x07) + 1)))
		pos += tableSize
	}
	one := make([]byte, 1)
	descriptor := make([]byte, 9)
	for pos < limit {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		if _, err := r.ReadAt(one, pos); err != nil {
			return 0, "", err
		}
		switch one[0] {
		case 0x3B: // Trailer
			return pos + 1 - start, "gif", nil
		case 0x21: // Extension: label + sub-blocos
			pos += 2
			next, err := skipGIFSubBlocks(r, pos, limit, cancel)
			if err != nil {
				return 0, "", err
			}
			pos = next
		case 0x2C: // Image Descriptor
			if _, err := r.ReadAt(descriptor, pos+1); err != nil {
				return 0, "", err
			}
			pos += 10
			if descriptor[8]&0x80 != 0 {
				pos += int64(3 * (1 << ((descriptor[8] & 0x07) + 1)))
			}
			pos++ // LZW minimum code size
			next, err := skipGIFSubBlocks(r, pos, limit, cancel)
			if err != nil {
				return 0, "", err
			}
			pos = next
		default:
			return 0, "", errors.New("bloco GIF inválido")
		}
	}
	return 0, "", errors.New("fim GIF não encontrado")
}

func skipGIFSubBlocks(r io.ReaderAt, pos, limit int64, cancel <-chan struct{}) (int64, error) {
	one := make([]byte, 1)
	for pos < limit {
		if cancelled(cancel) {
			return 0, errCancelled
		}
		if _, err := r.ReadAt(one, pos); err != nil {
			return 0, err
		}
		size := int64(one[0])
		pos++
		if size == 0 {
			return pos, nil
		}
		if pos+size > limit {
			return 0, io.ErrUnexpectedEOF
		}
		pos += size
	}
	return 0, io.EOF
}

func carvePDF(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	end, err := findMarker(r, start+5, min64(diskSize, start+1024*1024*1024), []byte("%%EOF"), cancel)
	if err != nil || end-start < 64 {
		return 0, "", err
	}
	return end - start, "pdf", nil
}

func carveZIP(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	limit := min64(diskSize, start+4*1024*1024*1024)
	pos := start + 4
	marker := []byte{'P', 'K', 0x05, 0x06}
	for pos < limit {
		found, err := findMarkerStart(r, pos, limit, marker, cancel)
		if err != nil {
			return 0, "", err
		}
		hdr := make([]byte, 22)
		if _, err := r.ReadAt(hdr, found); err != nil {
			return 0, "", err
		}
		commentLen := int64(binary.LittleEndian.Uint16(hdr[20:22]))
		length := found - start + 22 + commentLen
		if length <= 22 || start+length > diskSize {
			pos = found + 4
			continue
		}
		cdSize := int64(binary.LittleEndian.Uint32(hdr[12:16]))
		cdOffset := int64(binary.LittleEndian.Uint32(hdr[16:20]))
		if cdOffset >= 0 && cdSize >= 0 && cdOffset+cdSize <= found-start {
			sig := make([]byte, 4)
			if _, err := r.ReadAt(sig, start+cdOffset); err == nil && string(sig) == "PK\x01\x02" {
				return length, "zip", nil
			}
		}
		pos = found + 4
	}
	return 0, "", errors.New("fim ZIP não encontrado")
}

func carve7Z(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	header := make([]byte, 32)
	if _, err := r.ReadAt(header, start); err != nil {
		return 0, "", err
	}
	magic := []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}
	if !bytes.Equal(header[:6], magic) {
		return 0, "", errors.New("7z inválido")
	}
	nextOffset := binary.LittleEndian.Uint64(header[12:20])
	nextSize := binary.LittleEndian.Uint64(header[20:28])
	if nextOffset > 64*1024*1024*1024 || nextSize > 4*1024*1024*1024 {
		return 0, "", errors.New("tamanho 7z inválido")
	}
	total := uint64(32) + nextOffset + nextSize
	if total < 32 || total > uint64(diskSize-start) || total > 64*1024*1024*1024 {
		return 0, "", errors.New("arquivo 7z fora do limite")
	}
	return int64(total), "7z", nil
}

func carveRIFF(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, start); err != nil {
		return 0, "", err
	}
	if string(hdr[:4]) != "RIFF" {
		return 0, "", errors.New("RIFF inválido")
	}
	length := int64(binary.LittleEndian.Uint32(hdr[4:8])) + 8
	if length < 44 || length > 8*1024*1024*1024 || start+length > diskSize {
		return 0, "", errors.New("tamanho RIFF inválido")
	}
	switch string(hdr[8:12]) {
	case "AVI ":
		return length, "avi", nil
	case "WAVE":
		return length, "wav", nil
	case "WEBP":
		return length, "webp", nil
	default:
		return 0, "", errors.New("tipo RIFF não suportado")
	}
}

func isoBMFFExtension(brand string) string {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
		return "heic"
	case "avif", "avis":
		return "avif"
	case "qt  ":
		return "mov"
	case "3gp4", "3gp5", "3gp6", "3gp7", "3ge6", "3gg6":
		return "3gp"
	default:
		return "mp4"
	}
}

func carveMP4(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	const maxSize = int64(16 * 1024 * 1024 * 1024)
	brandHeader := make([]byte, 12)
	if _, err := r.ReadAt(brandHeader, start); err != nil || string(brandHeader[4:8]) != "ftyp" {
		return 0, "", errors.New("arquivo ISO-BMFF inválido")
	}
	ext := isoBMFFExtension(string(brandHeader[8:12]))
	pos := start
	limit := min64(diskSize, start+maxSize)
	boxes := 0
	hasFtyp := false
	hasMedia := false
	known := map[string]bool{"ftyp": true, "free": true, "skip": true, "wide": true, "mdat": true, "moov": true, "uuid": true, "pdin": true, "moof": true, "mfra": true, "sidx": true, "styp": true}

	for pos+8 <= limit && boxes < 100000 {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		hdr := make([]byte, 16)
		if _, err := r.ReadAt(hdr[:8], pos); err != nil {
			break
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		if !known[typ] {
			break
		}
		if size == 1 {
			if _, err := r.ReadAt(hdr, pos); err != nil {
				break
			}
			size64 := binary.BigEndian.Uint64(hdr[8:16])
			if size64 > uint64(^uint64(0)>>1) {
				break
			}
			size = int64(size64)
			headerLen = 16
		} else if size == 0 {
			if hasFtyp && hasMedia {
				return limit - start, ext, nil
			}
			break
		}
		if size < headerLen || pos+size > limit {
			break
		}
		if boxes == 0 && typ != "ftyp" {
			return 0, "", errors.New("MP4 sem ftyp")
		}
		if typ == "ftyp" {
			hasFtyp = true
		}
		if typ == "mdat" || typ == "moof" {
			hasMedia = true
		}
		pos += size
		boxes++
	}
	if hasFtyp && hasMedia && pos-start >= 1024 {
		return pos - start, ext, nil
	}
	return 0, "", errors.New("estrutura MP4 incompleta")
}

func carveSQLite(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	header := make([]byte, 100)
	if _, err := r.ReadAt(header, start); err != nil {
		return 0, "", err
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return 0, "", errors.New("SQLite inválido")
	}
	pageSize := int64(binary.BigEndian.Uint16(header[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return 0, "", errors.New("página SQLite inválida")
	}
	pageCount := int64(binary.BigEndian.Uint32(header[28:32]))
	if pageCount <= 0 || pageCount > 100_000_000 {
		return 0, "", errors.New("quantidade de páginas SQLite inválida")
	}
	length := pageSize * pageCount
	if length < 512 || length > 8*1024*1024*1024 || start+length > diskSize {
		return 0, "", errors.New("tamanho SQLite fora do limite")
	}
	return length, "sqlite", nil
}

func carveMP3(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	header := make([]byte, 10)
	if _, err := r.ReadAt(header, start); err != nil || string(header[:3]) != "ID3" {
		return 0, "", errors.New("ID3 inválido")
	}
	if header[6]&0x80 != 0 || header[7]&0x80 != 0 || header[8]&0x80 != 0 || header[9]&0x80 != 0 {
		return 0, "", errors.New("tamanho ID3 inválido")
	}
	tagSize := int64(header[6])<<21 | int64(header[7])<<14 | int64(header[8])<<7 | int64(header[9])
	pos := start + 10 + tagSize
	if header[5]&0x10 != 0 {
		pos += 10
	}
	maxEnd := min64(diskSize, start+2*1024*1024*1024)
	frames := 0
	lastGood := pos
	frameHeader := make([]byte, 4)

	for pos+4 <= maxEnd {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		if _, err := r.ReadAt(frameHeader, pos); err != nil {
			break
		}
		if string(frameHeader[:3]) == "TAG" && pos+128 <= maxEnd {
			pos += 128
			lastGood = pos
			break
		}
		frameLen := mp3FrameLength(frameHeader)
		if frameLen <= 0 || pos+int64(frameLen) > maxEnd {
			break
		}
		pos += int64(frameLen)
		lastGood = pos
		frames++
	}
	if frames < 3 || lastGood-start < 1024 {
		return 0, "", errors.New("quadros MP3 insuficientes")
	}
	return lastGood - start, "mp3", nil
}

var mpeg1Layer1Bitrates = [...]int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}
var mpeg1Layer2Bitrates = [...]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0}
var mpeg1Layer3Bitrates = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
var mpeg2Layer1Bitrates = [...]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}
var mpeg2Layer23Bitrates = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
var mp3SampleRates = [3][3]int{{44100, 48000, 32000}, {22050, 24000, 16000}, {11025, 12000, 8000}}

func mp3FrameLength(h []byte) int {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0
	}
	versionID := (h[1] >> 3) & 0x03
	layerID := (h[1] >> 1) & 0x03
	bitrateIdx := (h[2] >> 4) & 0x0F
	sampleIdx := (h[2] >> 2) & 0x03
	padding := int((h[2] >> 1) & 0x01)
	if versionID == 1 || layerID == 0 || bitrateIdx == 0 || bitrateIdx == 15 || sampleIdx == 3 {
		return 0
	}

	var bitrate int
	if versionID == 3 {
		switch layerID {
		case 3:
			bitrate = mpeg1Layer1Bitrates[bitrateIdx]
		case 2:
			bitrate = mpeg1Layer2Bitrates[bitrateIdx]
		case 1:
			bitrate = mpeg1Layer3Bitrates[bitrateIdx]
		}
	} else if layerID == 3 {
		bitrate = mpeg2Layer1Bitrates[bitrateIdx]
	} else {
		bitrate = mpeg2Layer23Bitrates[bitrateIdx]
	}

	versionRow := 0
	if versionID == 2 {
		versionRow = 1
	} else if versionID == 0 {
		versionRow = 2
	}
	sampleRate := mp3SampleRates[versionRow][sampleIdx]
	if bitrate == 0 || sampleRate == 0 {
		return 0
	}
	if layerID == 3 {
		return (12*bitrate*1000/sampleRate + padding) * 4
	}
	coefficient := 144
	if layerID == 1 && versionID != 3 {
		coefficient = 72
	}
	return coefficient*bitrate*1000/sampleRate + padding
}

func carveOLE(r io.ReaderAt, start, diskSize int64, cancel <-chan struct{}) (int64, string, error) {
	header := make([]byte, 512)
	if _, err := r.ReadAt(header, start); err != nil {
		return 0, "", err
	}
	magic := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	if !bytes.Equal(header[:8], magic) {
		return 0, "", errors.New("OLE inválido")
	}
	sectorShift := binary.LittleEndian.Uint16(header[30:32])
	if sectorShift != 9 && sectorShift != 12 {
		return 0, "", errors.New("setor OLE inválido")
	}
	sectorSize := int64(1) << sectorShift
	fatCount := int(binary.LittleEndian.Uint32(header[44:48]))
	if fatCount <= 0 || fatCount > 1_000_000 {
		return 0, "", errors.New("FAT OLE inválida")
	}

	const freeSect = uint32(0xFFFFFFFF)
	const endOfChain = uint32(0xFFFFFFFE)
	difat := make([]uint32, 0, fatCount)
	for i := 0; i < 109 && len(difat) < fatCount; i++ {
		v := binary.LittleEndian.Uint32(header[76+i*4 : 80+i*4])
		if v != freeSect {
			difat = append(difat, v)
		}
	}

	nextDifat := binary.LittleEndian.Uint32(header[68:72])
	difatSectors := int(binary.LittleEndian.Uint32(header[72:76]))
	entriesPerDifat := int(sectorSize/4) - 1
	secBuf := make([]byte, sectorSize)
	for d := 0; d < difatSectors && nextDifat != endOfChain && nextDifat != freeSect && len(difat) < fatCount; d++ {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		secPos := start + 512 + int64(nextDifat)*sectorSize
		if secPos < start || secPos+sectorSize > diskSize {
			break
		}
		if _, err := r.ReadAt(secBuf, secPos); err != nil {
			break
		}
		for i := 0; i < entriesPerDifat && len(difat) < fatCount; i++ {
			v := binary.LittleEndian.Uint32(secBuf[i*4 : i*4+4])
			if v != freeSect {
				difat = append(difat, v)
			}
		}
		nextDifat = binary.LittleEndian.Uint32(secBuf[entriesPerDifat*4 : entriesPerDifat*4+4])
	}
	if len(difat) == 0 {
		return 0, "", errors.New("DIFAT OLE vazia")
	}

	entriesPerFat := int(sectorSize / 4)
	highest := int64(-1)
	for fatIndex, fatSector := range difat {
		if cancelled(cancel) {
			return 0, "", errCancelled
		}
		secPos := start + 512 + int64(fatSector)*sectorSize
		if secPos < start || secPos+sectorSize > diskSize {
			continue
		}
		if _, err := r.ReadAt(secBuf, secPos); err != nil {
			continue
		}
		base := int64(fatSector) // include the FAT sector itself
		if base > highest {
			highest = base
		}
		for i := 0; i < entriesPerFat; i++ {
			v := binary.LittleEndian.Uint32(secBuf[i*4 : i*4+4])
			sectorIndex := int64(i) + int64(entriesPerFat)*int64(fatIndex)
			if v != freeSect && sectorIndex > highest {
				highest = sectorIndex
			}
		}
	}
	if highest < 0 {
		return 0, "", errors.New("tamanho OLE não determinado")
	}
	length := int64(512) + (highest+1)*sectorSize
	if length < 512 || length > 4*1024*1024*1024 || start+length > diskSize {
		return 0, "", errors.New("tamanho OLE fora do limite")
	}
	return length, "ole", nil
}

func findMarker(r io.ReaderAt, start, limit int64, marker []byte, cancel <-chan struct{}) (int64, error) {
	pos, err := findMarkerStart(r, start, limit, marker, cancel)
	if err != nil {
		return 0, err
	}
	return pos + int64(len(marker)), nil
}

var markerBufferPool = sync.Pool{New: func() any { return make([]byte, 8*1024*1024+64) }}
var copyBufferPool = sync.Pool{New: func() any { return make([]byte, 16*1024*1024) }}

func findMarkerStart(r io.ReaderAt, start, limit int64, marker []byte, cancel <-chan struct{}) (int64, error) {
	const chunk = 8 * 1024 * 1024
	if start >= limit {
		return 0, io.EOF
	}
	overlap := len(marker) - 1
	raw := markerBufferPool.Get().([]byte)
	defer markerBufferPool.Put(raw)
	if cap(raw) < chunk+overlap {
		raw = make([]byte, chunk+overlap)
	}
	buf := raw[:chunk+overlap]
	pos := start
	carry := 0
	for pos < limit {
		if cancelled(cancel) {
			return 0, errCancelled
		}
		readLen := chunk
		if int64(readLen) > limit-pos {
			readLen = int(limit - pos)
		}
		n, err := r.ReadAt(buf[carry:carry+readLen], pos)
		total := carry + n
		if idx := bytes.Index(buf[:total], marker); idx >= 0 {
			return pos - int64(carry) + int64(idx), nil
		}
		if total < len(marker) {
			return 0, io.EOF
		}
		carry = minInt(overlap, total)
		copy(buf[:carry], buf[total-carry:total])
		pos += int64(n)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.ErrUnexpectedEOF
		}
	}
	return 0, io.EOF
}

func copyRange(r io.ReaderAt, dest string, start, length int64, cancel <-chan struct{}, progress func(int64)) (returnErr error) {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); returnErr == nil && closeErr != nil {
			returnErr = closeErr
		}
	}()

	buf := copyBufferPool.Get().([]byte)
	defer copyBufferPool.Put(buf)
	var copied int64
	for copied < length {
		if cancelled(cancel) {
			return errCancelled
		}
		want := int64(len(buf))
		if length-copied < want {
			want = length - copied
		}
		n, readErr := r.ReadAt(buf[:int(want)], start+copied)
		if n > 0 {
			written := 0
			for written < n {
				m, writeErr := out.Write(buf[written:n])
				if writeErr != nil {
					return writeErr
				}
				if m == 0 {
					return io.ErrShortWrite
				}
				written += m
			}
			copied += int64(n)
			if progress != nil {
				progress(copied)
			}
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func classifyZip(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[strings.ToLower(strings.ReplaceAll(f.Name, "\\", "/"))] = true
	}
	switch {
	case names["word/document.xml"]:
		return "docx"
	case names["xl/workbook.xml"]:
		return "xlsx"
	case names["ppt/presentation.xml"]:
		return "pptx"
	case names["androidmanifest.xml"] && names["classes.dex"]:
		return "apk"
	case names["meta-inf/manifest.mf"]:
		return "jar"
	default:
		return "zip"
	}
}

func classifyOLE(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data := make([]byte, 8*1024*1024)
	n, _ := f.Read(data)
	data = data[:n]
	containsUTF16 := func(s string) bool {
		runes := []rune(s)
		encoded := utf16.Encode(runes)
		b := make([]byte, len(encoded)*2)
		for i, v := range encoded {
			binary.LittleEndian.PutUint16(b[i*2:i*2+2], v)
		}
		return bytes.Contains(data, b)
	}
	switch {
	case containsUTF16("WordDocument"):
		return "doc"
	case containsUTF16("Workbook") || containsUTF16("Book"):
		return "xls"
	case containsUTF16("PowerPoint Document"):
		return "ppt"
	default:
		return "ole"
	}
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func cancelled(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func logf(opts Options, text string) {
	if opts.Log != nil {
		opts.Log(text)
	}
}

func humanBytes(v int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	n := float64(v)
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ErrCancelled retorna o erro sentinela usado quando o usuário cancela uma varredura.
// Ele permite que a interface diferencie cancelamento de falha real sem comparar textos.
func ErrCancelled() error { return errCancelled }
