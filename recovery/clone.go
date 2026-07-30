package recovery

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CloneDestination is the minimum interface required by the raw clone engine.
// *os.File satisfies it for both normal files and Windows physical disks.
type CloneDestination interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// CloneOptions configures an exact byte-for-byte clone. The destination must
// already be opened with exclusive write access by the platform-specific UI.
type CloneOptions struct {
	Source              io.ReaderAt
	Destination         CloneDestination
	SourceID            string
	DestinationID       string
	SourceIdentity      string
	DestinationIdentity string
	SourceSize          int64
	DestinationSize     int64
	SectorSize          int64
	ChunkSize           int
	DamageMode          DamageMode
	Resume              bool
	Verify              bool
	AdjustGPT           bool
	ReportDir           string
	Cancel              <-chan struct{}
	Paused              func() bool
	Progress            func(CloneStatus)
	Log                 func(string)
}

// CloneStatus is emitted during copying and verification.
type CloneStatus struct {
	Phase          string
	BytesProcessed int64
	TotalBytes     int64
	ReadErrors     int
	VerifyErrors   int
	Current        string
}

// CloneResult summarizes a disk-to-disk clone.
type CloneResult struct {
	ReportDir         string        `json:"report_dir"`
	BytesCopied       int64         `json:"bytes_copied"`
	SourceSize        int64         `json:"source_size"`
	DestinationSize   int64         `json:"destination_size"`
	ReadErrors        int           `json:"read_errors"`
	VerifyErrors      int           `json:"verify_errors"`
	BadRanges         []BadRange    `json:"bad_ranges,omitempty"`
	Duration          time.Duration `json:"duration"`
	Resumed           bool          `json:"resumed"`
	Verified          bool          `json:"verified"`
	GPTAdjusted       bool          `json:"gpt_adjusted"`
	PartitionsDropped int           `json:"partitions_dropped"`
	ManifestPath      string        `json:"manifest_path"`
	StatePath         string        `json:"state_path"`
}

type cloneState struct {
	Version           int        `json:"version"`
	SourceID          string     `json:"source_id"`
	DestinationID     string     `json:"destination_id"`
	SourceSize        int64      `json:"source_size"`
	DestinationSize   int64      `json:"destination_size"`
	SectorSize        int64      `json:"sector_size"`
	ChunkSize         int        `json:"chunk_size"`
	Offset            int64      `json:"offset"`
	Completed         bool       `json:"completed"`
	Verified          bool       `json:"verified"`
	GPTAdjusted       bool       `json:"gpt_adjusted"`
	PartitionsDropped int        `json:"partitions_dropped"`
	ReadErrors        int        `json:"read_errors"`
	VerifyErrors      int        `json:"verify_errors"`
	BadRanges         []BadRange `json:"bad_ranges,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type cloneManifestEntry struct {
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}

// CloneDisk performs a raw, exact clone from Source to Destination. Unreadable
// source ranges are filled with zero bytes and recorded in the report.
func CloneDisk(opts CloneOptions) (CloneResult, error) {
	started := time.Now()
	result := CloneResult{SourceSize: opts.SourceSize, DestinationSize: opts.DestinationSize}
	if opts.Source == nil || opts.Destination == nil {
		return result, errors.New("origem ou destino de clonagem inválido")
	}
	if opts.SourceSize <= 0 {
		return result, errors.New("tamanho da origem inválido")
	}
	if opts.DestinationSize < opts.SourceSize {
		return result, errors.New("o disco de destino é menor que o disco de origem")
	}
	if opts.SectorSize < 512 || opts.SectorSize&(opts.SectorSize-1) != 0 {
		opts.SectorSize = 512
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 16 * 1024 * 1024
	}
	opts.ChunkSize = int(alignDownLocal(int64(opts.ChunkSize), opts.SectorSize))
	if opts.ChunkSize < int(opts.SectorSize) {
		opts.ChunkSize = int(opts.SectorSize)
	}
	if strings.TrimSpace(opts.ReportDir) == "" {
		return result, errors.New("pasta de relatório não informada")
	}
	if err := os.MkdirAll(opts.ReportDir, 0o755); err != nil {
		return result, fmt.Errorf("não foi possível criar a pasta de relatório: %w", err)
	}
	result.ReportDir = opts.ReportDir
	result.StatePath = filepath.Join(opts.ReportDir, "CLONAGEM_SESSAO.json")
	result.ManifestPath = filepath.Join(opts.ReportDir, "CLONAGEM_BLOCOS.ndjson")

	sourceIdentity := strings.TrimSpace(opts.SourceIdentity)
	if sourceIdentity == "" {
		sourceIdentity = strings.TrimSpace(opts.SourceID)
	}
	destinationIdentity := strings.TrimSpace(opts.DestinationIdentity)
	if destinationIdentity == "" {
		destinationIdentity = strings.TrimSpace(opts.DestinationID)
	}
	if sourceIdentity == "" || destinationIdentity == "" {
		return result, errors.New("identidade da origem ou do destino não informada")
	}
	state := cloneState{
		Version: 2, SourceID: sourceIdentity, DestinationID: destinationIdentity,
		SourceSize: opts.SourceSize, DestinationSize: opts.DestinationSize,
		SectorSize: opts.SectorSize, ChunkSize: opts.ChunkSize,
	}
	startAt := int64(0)
	entries := []cloneManifestEntry{}
	if opts.Resume {
		if loaded, err := loadCloneState(result.StatePath); err == nil && cloneStateMatches(loaded, state) {
			state = loaded
			if state.Offset >= 0 && state.Offset <= opts.SourceSize {
				startAt = state.Offset
				result.Resumed = startAt > 0
			}
			entries, _ = loadCloneManifest(result.ManifestPath, startAt)
			if len(entries) > 0 {
				last := entries[len(entries)-1]
				manifestEnd := last.Offset + int64(last.Length)
				if manifestEnd < startAt {
					startAt = manifestEnd
					state.Offset = startAt
				}
			} else if startAt > 0 {
				// A state without a matching block manifest is not safe to resume.
				startAt = 0
				state.Offset = 0
				state.Completed = false
				state.Verified = false
				state.BadRanges = nil
				result.Resumed = false
			}
			if opts.Log != nil && startAt > 0 {
				opts.Log("Retomando clonagem em " + humanBytes(startAt) + ".")
			}
		}
	}
	if !opts.Resume || startAt == 0 {
		_ = os.Remove(result.StatePath)
		_ = os.Remove(result.ManifestPath)
		state.Offset = 0
		state.Completed = false
		state.Verified = false
		state.ReadErrors = 0
		state.VerifyErrors = 0
		state.BadRanges = nil
		entries = nil
	} else {
		if err := rewriteCloneManifest(result.ManifestPath, entries); err != nil {
			return result, err
		}
		if len(entries) > 0 {
			last := entries[len(entries)-1]
			check := make([]byte, last.Length)
			n, readErr := opts.Destination.ReadAt(check, last.Offset)
			if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(check)) || n != len(check) {
				return result, errors.New("não foi possível validar o último bloco gravado da sessão anterior")
			}
			sum := sha256.Sum256(check)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), last.SHA256) {
				return result, errors.New("o disco de destino não corresponde mais à sessão salva; desmarque a retomada para iniciar uma nova clonagem")
			}
		}
	}

	if state.Completed && state.GPTAdjusted && startAt == opts.SourceSize {
		result.BytesCopied = opts.SourceSize
		result.BadRanges = append([]BadRange(nil), state.BadRanges...)
		result.ReadErrors = state.ReadErrors
		result.VerifyErrors = state.VerifyErrors
		result.Verified = state.Verified
		result.GPTAdjusted = true
		result.Duration = time.Since(started)
		if opts.Log != nil {
			opts.Log("A sessão selecionada já foi concluída e a GPT já está ajustada.")
		}
		_ = writeCloneReports(opts, result, nil)
		return result, nil
	}

	previousBad := append([]BadRange(nil), state.BadRanges...)
	adaptive := newAdaptiveReaderAt(opts.Source, opts.SectorSize, opts.DamageMode, opts.Cancel, opts.Paused)
	manifestFile, err := os.OpenFile(result.ManifestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return result, fmt.Errorf("não foi possível abrir o manifesto da clonagem: %w", err)
	}
	manifestWriter := bufio.NewWriterSize(manifestFile, 64*1024)

	logf := func(text string) {
		if opts.Log != nil {
			opts.Log(text)
		}
	}
	logf(fmt.Sprintf("Clonagem setor a setor: %s -> %s (%s).", opts.SourceID, opts.DestinationID, humanBytes(opts.SourceSize)))
	if opts.DestinationSize > opts.SourceSize {
		logf("O destino é maior; o espaço excedente permanecerá não alocado após a cópia exata.")
	}

	offset := startAt
	buf := make([]byte, opts.ChunkSize)
	lastSync := startAt
	copyErr := error(nil)
	checkpoint := func(force bool) error {
		if !force && offset-lastSync < 1024*1024*1024 && offset < opts.SourceSize {
			return nil
		}
		if err := manifestWriter.Flush(); err != nil {
			return fmt.Errorf("falha ao salvar o manifesto: %w", err)
		}
		if err := manifestFile.Sync(); err != nil {
			return fmt.Errorf("falha ao sincronizar o manifesto: %w", err)
		}
		if err := opts.Destination.Sync(); err != nil {
			return fmt.Errorf("falha ao sincronizar os dados no destino: %w", err)
		}
		if err := saveCloneState(result.StatePath, state); err != nil {
			return fmt.Errorf("falha ao salvar a sessão de clonagem: %w", err)
		}
		lastSync = offset
		return nil
	}
	if !state.Completed || offset < opts.SourceSize {
		for offset < opts.SourceSize {
			if err := waitControl(opts.Cancel, opts.Paused); err != nil {
				copyErr = err
				break
			}
			want := int64(len(buf))
			if opts.SourceSize-offset < want {
				want = opts.SourceSize - offset
			}
			block := buf[:int(want)]
			for i := range block {
				block[i] = 0
			}
			n, readErr := adaptive.ReadAt(block, offset)
			if n < len(block) {
				// The adaptive reader normally zero-fills unreadable regions;
				// this is an additional guard for unusual short reads.
				for i := n; i < len(block); i++ {
					block[i] = 0
				}
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				if errors.Is(readErr, errCancelled) {
					copyErr = readErr
					break
				}
				logf(fmt.Sprintf("Falha de leitura próxima de %s; área problemática preenchida com zeros.", humanBytes(offset)))
			}
			if err := writeFullAt(opts.Destination, block, offset); err != nil {
				copyErr = fmt.Errorf("falha ao gravar o destino em %s: %w", humanBytes(offset), err)
				break
			}
			sum := sha256.Sum256(block)
			entry := cloneManifestEntry{Offset: offset, Length: len(block), SHA256: hex.EncodeToString(sum[:])}
			encoded, _ := json.Marshal(entry)
			if _, err := manifestWriter.Write(append(encoded, '\n')); err != nil {
				copyErr = fmt.Errorf("falha ao registrar o bloco clonado: %w", err)
				break
			}
			if err := manifestWriter.Flush(); err != nil {
				copyErr = fmt.Errorf("falha ao salvar o manifesto: %w", err)
				break
			}
			entries = append(entries, entry)
			offset += want
			bad := append(append([]BadRange(nil), previousBad...), adaptive.BadRanges()...)
			state.Offset = offset
			state.ReadErrors = len(bad)
			state.BadRanges = bad
			state.UpdatedAt = time.Now()
			if err := checkpoint(false); err != nil {
				copyErr = err
				break
			}
			if opts.Progress != nil {
				opts.Progress(CloneStatus{Phase: "cloning", BytesProcessed: offset, TotalBytes: opts.SourceSize, ReadErrors: state.ReadErrors, Current: "Copiando setores"})
			}
		}
	}
	if syncErr := checkpoint(true); syncErr != nil && copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := manifestFile.Close(); closeErr != nil && copyErr == nil {
		copyErr = fmt.Errorf("falha ao fechar o manifesto: %w", closeErr)
	}

	result.BytesCopied = offset
	result.BadRanges = append([]BadRange(nil), state.BadRanges...)
	result.ReadErrors = len(result.BadRanges)
	if copyErr != nil {
		state.Offset = offset
		state.Completed = false
		state.UpdatedAt = time.Now()
		if saveErr := saveCloneState(result.StatePath, state); saveErr != nil {
			copyErr = fmt.Errorf("%v; além disso, não foi possível salvar a retomada: %w", copyErr, saveErr)
		}
		result.Duration = time.Since(started)
		_ = writeCloneReports(opts, result, copyErr)
		return result, copyErr
	}
	if offset < opts.SourceSize {
		result.Duration = time.Since(started)
		err := errors.New("clonagem encerrada antes de copiar toda a origem")
		_ = writeCloneReports(opts, result, err)
		return result, err
	}
	if err := opts.Destination.Sync(); err != nil {
		result.Duration = time.Since(started)
		_ = writeCloneReports(opts, result, err)
		return result, err
	}
	state.Offset = opts.SourceSize
	state.Completed = true
	state.UpdatedAt = time.Now()
	if err := saveCloneState(result.StatePath, state); err != nil {
		result.Duration = time.Since(started)
		_ = writeCloneReports(opts, result, err)
		return result, fmt.Errorf("cópia concluída, mas a sessão final não pôde ser salva: %w", err)
	}

	if opts.Verify {
		logf("Iniciando verificação bloco a bloco do disco de destino.")
		entries, err = loadCloneManifest(result.ManifestPath, opts.SourceSize)
		if err != nil {
			result.Duration = time.Since(started)
			_ = writeCloneReports(opts, result, err)
			return result, err
		}
		verifyErrors := 0
		verifiedBytes := int64(0)
		verifyBuf := make([]byte, opts.ChunkSize)
		for _, entry := range entries {
			if err := waitControl(opts.Cancel, opts.Paused); err != nil {
				result.Duration = time.Since(started)
				result.VerifyErrors = verifyErrors
				_ = writeCloneReports(opts, result, err)
				return result, err
			}
			if entry.Length <= 0 || entry.Length > len(verifyBuf) {
				verifyErrors++
				continue
			}
			block := verifyBuf[:entry.Length]
			n, readErr := opts.Destination.ReadAt(block, entry.Offset)
			if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(block)) || n != len(block) {
				verifyErrors++
			} else {
				sum := sha256.Sum256(block)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), entry.SHA256) {
					verifyErrors++
				}
			}
			verifiedBytes = entry.Offset + int64(entry.Length)
			if opts.Progress != nil {
				opts.Progress(CloneStatus{Phase: "verifying", BytesProcessed: verifiedBytes, TotalBytes: opts.SourceSize, ReadErrors: result.ReadErrors, VerifyErrors: verifyErrors, Current: "Verificando destino"})
			}
		}
		result.VerifyErrors = verifyErrors
		result.Verified = verifyErrors == 0 && verifiedBytes >= opts.SourceSize
		state.VerifyErrors = verifyErrors
		state.Verified = result.Verified
		state.UpdatedAt = time.Now()
		if err := saveCloneState(result.StatePath, state); err != nil {
			result.Duration = time.Since(started)
			_ = writeCloneReports(opts, result, err)
			return result, fmt.Errorf("verificação concluída, mas a sessão não pôde ser salva: %w", err)
		}
		if !result.Verified {
			result.Duration = time.Since(started)
			err := fmt.Errorf("a verificação encontrou %d bloco(s) diferente(s) no destino", verifyErrors)
			_ = writeCloneReports(opts, result, err)
			return result, err
		}
		logf("Verificação concluída: todos os blocos gravados coincidem com o manifesto.")
	}

	if opts.AdjustGPT {
		logf("Validando e ajustando a tabela de partições ao tamanho físico do destino.")
		adjusted, dropped, adjustErr := adjustPartitionTableForDestination(opts.Destination, opts.DestinationSize, opts.SectorSize)
		if adjustErr != nil {
			result.Duration = time.Since(started)
			_ = writeCloneReports(opts, result, adjustErr)
			return result, fmt.Errorf("a cópia foi concluída, mas a tabela de partições não pôde ser adaptada ao destino: %w", adjustErr)
		}
		result.GPTAdjusted = adjusted
		result.PartitionsDropped = dropped
		state.GPTAdjusted = adjusted
		state.PartitionsDropped = dropped
		state.UpdatedAt = time.Now()
		if err := saveCloneState(result.StatePath, state); err != nil {
			result.Duration = time.Since(started)
			_ = writeCloneReports(opts, result, err)
			return result, fmt.Errorf("GPT ajustada, mas a sessão final não pôde ser salva: %w", err)
		}
		if adjusted {
			if dropped > 0 {
				logf(fmt.Sprintf("Tabela de partições adaptada ao SSD; %d partição(ões) de recuperação que não cabiam foram omitidas.", dropped))
			} else {
				logf("Tabela de partições ajustada ao tamanho físico do destino.")
			}
		}
	}

	result.Duration = time.Since(started)
	if err := writeCloneReports(opts, result, nil); err != nil {
		return result, err
	}
	return result, nil
}

func cloneStateMatches(a, b cloneState) bool {
	return a.Version == b.Version && a.SourceID == b.SourceID && a.DestinationID == b.DestinationID &&
		a.SourceSize == b.SourceSize && a.DestinationSize == b.DestinationSize &&
		a.SectorSize == b.SectorSize && a.ChunkSize == b.ChunkSize
}

func saveCloneState(path string, state cloneState) error {
	state.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadCloneState(path string) (cloneState, error) {
	var state cloneState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func loadCloneManifest(path string, maxEnd int64) ([]cloneManifestEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	entries := make([]cloneManifestEntry, 0, 1024)
	expected := int64(0)
	for scanner.Scan() {
		var entry cloneManifestEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Offset != expected || entry.Length <= 0 {
			break
		}
		digest, digestErr := hex.DecodeString(entry.SHA256)
		if digestErr != nil || len(digest) != sha256.Size {
			break
		}
		end := entry.Offset + int64(entry.Length)
		if end > maxEnd {
			break
		}
		entries = append(entries, entry)
		expected = end
	}
	return entries, scanner.Err()
}

func rewriteCloneManifest(path string, entries []cloneManifestEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		if _, err := w.Write(append(data, '\n')); err != nil {
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

func writeFullAt(w io.WriterAt, p []byte, off int64) error {
	written := 0
	for written < len(p) {
		n, err := w.WriteAt(p[written:], off+int64(written))
		if n > 0 {
			written += n
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeCloneReports(opts CloneOptions, result CloneResult, operationErr error) error {
	_ = writeBadMap(result.ReportDir, result.BadRanges)
	payload := struct {
		CloneResult
		SourceID      string `json:"source_id"`
		DestinationID string `json:"destination_id"`
		Error         string `json:"error,omitempty"`
		FinishedAt    string `json:"finished_at"`
	}{CloneResult: result, SourceID: opts.SourceID, DestinationID: opts.DestinationID, FinishedAt: time.Now().Format(time.RFC3339)}
	if operationErr != nil {
		payload.Error = operationErr.Error()
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.WriteFile(filepath.Join(result.ReportDir, "RELATORIO_CLONAGEM.json"), data, 0o644); err != nil {
		return err
	}

	status := "CONCLUÍDA"
	if operationErr != nil {
		status = "INTERROMPIDA/COM ERRO"
	} else if result.VerifyErrors > 0 {
		status = "CONCLUÍDA COM FALHA DE VERIFICAÇÃO"
	} else if result.ReadErrors > 0 {
		status = "CONCLUÍDA COM SETORES ILEGÍVEIS PREENCHIDOS COM ZERO"
	}
	txt := fmt.Sprintf("HdRecover — Relatório de clonagem\r\n\r\nStatus: %s\r\nOrigem: %s\r\nDestino: %s\r\nCopiado: %s de %s\r\nDestino total: %s\r\nFalhas de leitura: %d\r\nFalhas de verificação: %d\r\nVerificado: %t\r\nGPT ajustada ao destino: %t\r\nPartições de recuperação omitidas: %d\r\nRetomado: %t\r\nDuração: %s\r\n",
		status, opts.SourceID, opts.DestinationID, humanBytes(result.BytesCopied), humanBytes(result.SourceSize),
		humanBytes(result.DestinationSize), result.ReadErrors, result.VerifyErrors, result.Verified, result.GPTAdjusted, result.PartitionsDropped, result.Resumed, result.Duration.Round(time.Second))
	if operationErr != nil {
		txt += "Erro: " + operationErr.Error() + "\r\n"
	}
	if err := os.WriteFile(filepath.Join(result.ReportDir, "RELATORIO_CLONAGEM.txt"), []byte(txt), 0o644); err != nil {
		return err
	}

	csvFile, err := os.Create(filepath.Join(result.ReportDir, "RESUMO_CLONAGEM.csv"))
	if err == nil {
		w := csv.NewWriter(csvFile)
		_ = w.Write([]string{"origem", "destino", "bytes_copiados", "tamanho_origem", "tamanho_destino", "falhas_leitura", "falhas_verificacao", "verificado", "gpt_ajustada", "particoes_omitidas", "duracao_segundos", "status"})
		_ = w.Write([]string{opts.SourceID, opts.DestinationID, strconv.FormatInt(result.BytesCopied, 10), strconv.FormatInt(result.SourceSize, 10), strconv.FormatInt(result.DestinationSize, 10), strconv.Itoa(result.ReadErrors), strconv.Itoa(result.VerifyErrors), strconv.FormatBool(result.Verified), strconv.FormatBool(result.GPTAdjusted), strconv.Itoa(result.PartitionsDropped), strconv.FormatInt(int64(result.Duration.Seconds()), 10), status})
		w.Flush()
		_ = csvFile.Close()
	}

	html := "<!doctype html><html lang=\"pt-BR\"><meta charset=\"utf-8\"><title>Relatório de clonagem — HdRecover</title>" +
		"<style>body{font-family:Segoe UI,Arial;background:#0e1116;color:#f8fafc;max-width:1000px;margin:40px auto;padding:0 24px}section{background:#191e26;padding:24px;border-radius:14px;margin:16px 0}h1{color:#57aeff}strong{color:#ffd066}code{word-break:break-all}.ok{color:#75df9b}.bad{color:#ff8c8c}</style>" +
		"<h1>HdRecover — Relatório de clonagem</h1><section><h2>Status</h2><p><strong>" + htmlEscape(status) + "</strong></p>" +
		"<p>Origem: <code>" + htmlEscape(opts.SourceID) + "</code></p><p>Destino: <code>" + htmlEscape(opts.DestinationID) + "</code></p>" +
		"<p>Copiado: " + htmlEscape(humanBytes(result.BytesCopied)) + " de " + htmlEscape(humanBytes(result.SourceSize)) + "</p>" +
		"<p>Falhas de leitura: " + strconv.Itoa(result.ReadErrors) + "</p><p>Falhas de verificação: " + strconv.Itoa(result.VerifyErrors) + "</p>" +
		"<p>Verificação completa: " + strconv.FormatBool(result.Verified) + "</p><p>GPT ajustada ao destino: " + strconv.FormatBool(result.GPTAdjusted) + "</p><p>Partições de recuperação omitidas: " + strconv.Itoa(result.PartitionsDropped) + "</p><p>Duração: " + htmlEscape(result.Duration.Round(time.Second).String()) + "</p></section>" +
		"<section><h2>Arquivos auxiliares</h2><p>CLONAGEM_SESSAO.json permite retomar uma cópia interrompida. CLONAGEM_BLOCOS.ndjson contém os hashes usados para verificação. MAPA_DE_SETORES_RUINS.* registra regiões ilegíveis.</p></section></html>"
	return os.WriteFile(filepath.Join(result.ReportDir, "RELATORIO_CLONAGEM.html"), []byte(html), 0o644)
}

func adjustPartitionTableForDestination(d CloneDestination, destinationSize, sectorSize int64) (bool, int, error) {
	if sectorSize < 512 || sectorSize > 1024*1024 || sectorSize&(sectorSize-1) != 0 {
		return false, 0, errors.New("tamanho de setor inválido para ajuste da tabela de partições")
	}
	if destinationSize <= sectorSize*2 || destinationSize%sectorSize != 0 {
		return false, 0, errors.New("o tamanho do destino não está alinhado ao setor lógico")
	}
	mbr := make([]byte, sectorSize)
	n, err := d.ReadAt(mbr, 0)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(mbr)) || n != len(mbr) {
		return false, 0, fmt.Errorf("não foi possível ler o setor de inicialização: %w", err)
	}
	primary := make([]byte, sectorSize)
	primaryN, primaryErr := d.ReadAt(primary, sectorSize)
	hasGPTHeader := (primaryErr == nil || (errors.Is(primaryErr, io.EOF) && primaryN == len(primary))) && primaryN == len(primary) && string(primary[:8]) == "EFI PART"
	if len(mbr) < 512 || mbr[510] != 0x55 || mbr[511] != 0xAA {
		if hasGPTHeader {
			return adjustGPTForDestination(d, mbr, destinationSize, sectorSize)
		}
		return false, 0, nil
	}
	protectiveGPT := false
	for i := 0; i < 4; i++ {
		if mbr[446+i*16+4] == 0xEE {
			protectiveGPT = true
			break
		}
	}
	if protectiveGPT {
		return adjustGPTForDestination(d, mbr, destinationSize, sectorSize)
	}
	// Some damaged disks can have a valid GPT header but a missing protective
	// MBR entry. Prefer the GPT signature when present.
	if hasGPTHeader {
		return adjustGPTForDestination(d, mbr, destinationSize, sectorSize)
	}
	return adjustMBRForDestination(d, mbr, destinationSize, sectorSize)
}

func adjustMBRForDestination(d CloneDestination, mbr []byte, destinationSize, sectorSize int64) (bool, int, error) {
	totalSectors := uint64(destinationSize / sectorSize)
	changed := false
	dropped := 0
	type partitionRange struct {
		number int
		first  uint64
		end    uint64
	}
	ranges := make([]partitionRange, 0, 4)
	for i := 0; i < 4; i++ {
		off := 446 + i*16
		partType := mbr[off+4]
		start := uint64(binary.LittleEndian.Uint32(mbr[off+8 : off+12]))
		count := uint64(binary.LittleEndian.Uint32(mbr[off+12 : off+16]))
		if partType == 0 || count == 0 {
			continue
		}
		if start == 0 {
			return false, dropped, fmt.Errorf("a partição MBR %d começa no setor zero", i+1)
		}
		end := start + count
		if end <= totalSectors {
			ranges = append(ranges, partitionRange{number: i + 1, first: start, end: end})
			continue
		}
		if isDroppableMBRPartitionType(partType) {
			for j := 0; j < 16; j++ {
				mbr[off+j] = 0
			}
			changed = true
			dropped++
			continue
		}
		return false, dropped, fmt.Errorf("a partição MBR %d (tipo 0x%02X) termina além do SSD; reduza essa partição antes de migrar", i+1, partType)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].first < ranges[j].first })
	for i := 1; i < len(ranges); i++ {
		if ranges[i].first < ranges[i-1].end {
			return false, dropped, fmt.Errorf("as partições MBR %d e %d se sobrepõem", ranges[i-1].number, ranges[i].number)
		}
	}
	if !changed {
		return false, 0, nil
	}
	if err := writeFullAt(d, mbr, 0); err != nil {
		return false, dropped, err
	}
	if err := d.Sync(); err != nil {
		return false, dropped, err
	}
	return true, dropped, nil
}

func isDroppableMBRPartitionType(partType byte) bool {
	switch partType {
	case 0x12, 0x27, 0xDE:
		return true
	default:
		return false
	}
}

func adjustGPTForDestination(d CloneDestination, mbr []byte, destinationSize, sectorSize int64) (bool, int, error) {
	primarySector := make([]byte, sectorSize)
	n, err := d.ReadAt(primarySector, sectorSize)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(primarySector)) || n != len(primarySector) {
		return false, 0, fmt.Errorf("não foi possível ler o cabeçalho GPT primário: %w", err)
	}
	if string(primarySector[:8]) != "EFI PART" {
		return false, 0, nil
	}
	headerSize := int(binary.LittleEndian.Uint32(primarySector[12:16]))
	if headerSize < 92 || headerSize > len(primarySector) {
		return false, 0, errors.New("cabeçalho GPT primário possui tamanho inválido")
	}
	storedHeaderCRC := binary.LittleEndian.Uint32(primarySector[16:20])
	headerForCRC := append([]byte(nil), primarySector[:headerSize]...)
	binary.LittleEndian.PutUint32(headerForCRC[16:20], 0)
	if storedHeaderCRC == 0 || crc32.ChecksumIEEE(headerForCRC) != storedHeaderCRC {
		return false, 0, errors.New("CRC do cabeçalho GPT primário é inválido")
	}
	if binary.LittleEndian.Uint64(primarySector[24:32]) != 1 {
		return false, 0, errors.New("o cabeçalho GPT primário não está no LBA 1")
	}
	entryStartLBA := binary.LittleEndian.Uint64(primarySector[72:80])
	entryCount := binary.LittleEndian.Uint32(primarySector[80:84])
	entrySize := binary.LittleEndian.Uint32(primarySector[84:88])
	if entryCount == 0 || entrySize < 128 || entrySize > 4096 {
		return false, 0, errors.New("tabela de entradas GPT inválida")
	}
	entryBytes64 := uint64(entryCount) * uint64(entrySize)
	if entryBytes64 == 0 || entryBytes64 > 64*1024*1024 {
		return false, 0, errors.New("tabela de entradas GPT grande demais")
	}
	entryBytes := int(entryBytes64)
	entrySectors := (entryBytes64 + uint64(sectorSize) - 1) / uint64(sectorSize)
	targetLastLBA := uint64(destinationSize/sectorSize - 1)
	if targetLastLBA <= entrySectors+2 {
		return false, 0, errors.New("o destino é pequeno demais para uma tabela GPT válida")
	}
	firstUsableLBA := binary.LittleEndian.Uint64(primarySector[40:48])
	if entryStartLBA < 2 || entryStartLBA > ^uint64(0)-entrySectors ||
		entryStartLBA+entrySectors > firstUsableLBA ||
		entryStartLBA+entrySectors >= targetLastLBA {
		return false, 0, errors.New("posição da tabela GPT primária inválida")
	}
	entries := make([]byte, entryBytes)
	entryOffset := int64(entryStartLBA) * sectorSize
	n, err = d.ReadAt(entries, entryOffset)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(entries)) || n != len(entries) {
		return false, 0, fmt.Errorf("não foi possível ler as entradas GPT: %w", err)
	}
	if crc32.ChecksumIEEE(entries) != binary.LittleEndian.Uint32(primarySector[88:92]) {
		return false, 0, errors.New("CRC da tabela de entradas GPT primária é inválido")
	}
	newBackupEntriesLBA := targetLastLBA - entrySectors
	newLastUsableLBA := newBackupEntriesLBA - 1
	if newLastUsableLBA <= firstUsableLBA {
		return false, 0, errors.New("o destino não possui espaço suficiente para a GPT de backup")
	}

	dropped := 0
	type gptPartitionRange struct {
		number uint32
		first  uint64
		last   uint64
	}
	ranges := make([]gptPartitionRange, 0, entryCount)
	for i := uint32(0); i < entryCount; i++ {
		off := int(i * entrySize)
		entry := entries[off : off+int(entrySize)]
		if cloneAllZero(entry[:16]) {
			continue
		}
		first := binary.LittleEndian.Uint64(entry[32:40])
		last := binary.LittleEndian.Uint64(entry[40:48])
		if first < firstUsableLBA || last < first {
			return false, dropped, fmt.Errorf("a entrada GPT %d é inválida", i+1)
		}
		if last <= newLastUsableLBA {
			ranges = append(ranges, gptPartitionRange{number: i + 1, first: first, last: last})
			continue
		}
		if isWindowsRecoveryGPTType(entry[:16]) {
			for j := range entry {
				entry[j] = 0
			}
			dropped++
			continue
		}
		return false, dropped, fmt.Errorf("a partição GPT %d termina além do SSD e não é uma partição de recuperação descartável", i+1)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].first < ranges[j].first })
	for i := 1; i < len(ranges); i++ {
		if ranges[i].first <= ranges[i-1].last {
			return false, dropped, fmt.Errorf("as partições GPT %d e %d se sobrepõem", ranges[i-1].number, ranges[i].number)
		}
	}

	protectiveEntry := -1
	emptyEntry := -1
	for i := 0; i < 4; i++ {
		off := 446 + i*16
		if mbr[off+4] == 0xEE {
			protectiveEntry = i
			break
		}
		if emptyEntry < 0 && cloneAllZero(mbr[off:off+16]) {
			emptyEntry = i
		}
	}
	if protectiveEntry < 0 {
		protectiveEntry = emptyEntry
	}
	if protectiveEntry < 0 {
		return false, dropped, errors.New("não há entrada livre no MBR para registrar a proteção GPT")
	}

	entryCRC := crc32.ChecksumIEEE(entries)
	primary := append([]byte(nil), primarySector...)
	binary.LittleEndian.PutUint64(primary[24:32], 1)
	binary.LittleEndian.PutUint64(primary[32:40], targetLastLBA)
	binary.LittleEndian.PutUint64(primary[48:56], newLastUsableLBA)
	binary.LittleEndian.PutUint32(primary[88:92], entryCRC)
	binary.LittleEndian.PutUint32(primary[16:20], 0)
	binary.LittleEndian.PutUint32(primary[16:20], crc32.ChecksumIEEE(primary[:headerSize]))

	backup := append([]byte(nil), primary...)
	binary.LittleEndian.PutUint64(backup[24:32], targetLastLBA)
	binary.LittleEndian.PutUint64(backup[32:40], 1)
	binary.LittleEndian.PutUint64(backup[72:80], newBackupEntriesLBA)
	binary.LittleEndian.PutUint32(backup[16:20], 0)
	binary.LittleEndian.PutUint32(backup[16:20], crc32.ChecksumIEEE(backup[:headerSize]))

	// Grave primeiro a cópia secundária. Se houver interrupção, a GPT primária
	// original continua sendo a melhor chance de recuperação do layout.
	if err := writeFullAt(d, entries, int64(newBackupEntriesLBA)*sectorSize); err != nil {
		return false, dropped, fmt.Errorf("falha ao gravar entradas GPT de backup: %w", err)
	}
	if err := writeFullAt(d, backup, int64(targetLastLBA)*sectorSize); err != nil {
		return false, dropped, fmt.Errorf("falha ao gravar cabeçalho GPT de backup: %w", err)
	}
	if err := d.Sync(); err != nil {
		return false, dropped, fmt.Errorf("falha ao sincronizar a GPT de backup: %w", err)
	}
	if err := writeFullAt(d, entries, entryOffset); err != nil {
		return false, dropped, fmt.Errorf("falha ao atualizar entradas GPT primárias: %w", err)
	}
	if err := writeFullAt(d, primary, sectorSize); err != nil {
		return false, dropped, fmt.Errorf("falha ao atualizar cabeçalho GPT primário: %w", err)
	}

	// Keep the protective MBR consistent with the physical destination size.
	sectors := targetLastLBA
	if sectors > 0xFFFFFFFF {
		sectors = 0xFFFFFFFF
	}
	mbr[510] = 0x55
	mbr[511] = 0xAA
	off := 446 + protectiveEntry*16
	for i := 0; i < 16; i++ {
		mbr[off+i] = 0
	}
	mbr[off+4] = 0xEE
	binary.LittleEndian.PutUint32(mbr[off+8:off+12], 1)
	binary.LittleEndian.PutUint32(mbr[off+12:off+16], uint32(sectors))
	if err := writeFullAt(d, mbr, 0); err != nil {
		return false, dropped, fmt.Errorf("falha ao atualizar MBR protetor: %w", err)
	}
	if err := d.Sync(); err != nil {
		return false, dropped, err
	}
	if err := validateWrittenGPT(d, targetLastLBA, entryStartLBA, newBackupEntriesLBA, entryBytes, entryCRC, headerSize, sectorSize); err != nil {
		return false, dropped, fmt.Errorf("a GPT foi gravada, mas a leitura de confirmação falhou: %w", err)
	}
	return true, dropped, nil
}

func validateWrittenGPT(d CloneDestination, lastLBA, primaryEntriesLBA, backupEntriesLBA uint64, entryBytes int, entryCRC uint32, headerSize int, sectorSize int64) error {
	checkHeader := func(lba, currentLBA, alternateLBA, entriesLBA uint64) error {
		header := make([]byte, sectorSize)
		n, err := d.ReadAt(header, int64(lba)*sectorSize)
		if err != nil && !(errors.Is(err, io.EOF) && n == len(header)) || n != len(header) {
			return fmt.Errorf("não foi possível reler o cabeçalho no LBA %d: %w", lba, err)
		}
		if string(header[:8]) != "EFI PART" ||
			binary.LittleEndian.Uint64(header[24:32]) != currentLBA ||
			binary.LittleEndian.Uint64(header[32:40]) != alternateLBA ||
			binary.LittleEndian.Uint64(header[72:80]) != entriesLBA ||
			binary.LittleEndian.Uint32(header[88:92]) != entryCRC {
			return fmt.Errorf("campos inconsistentes no cabeçalho GPT do LBA %d", lba)
		}
		storedCRC := binary.LittleEndian.Uint32(header[16:20])
		headerCopy := append([]byte(nil), header[:headerSize]...)
		binary.LittleEndian.PutUint32(headerCopy[16:20], 0)
		if storedCRC == 0 || crc32.ChecksumIEEE(headerCopy) != storedCRC {
			return fmt.Errorf("CRC inválido no cabeçalho GPT do LBA %d", lba)
		}
		table := make([]byte, entryBytes)
		n, err = d.ReadAt(table, int64(entriesLBA)*sectorSize)
		if err != nil && !(errors.Is(err, io.EOF) && n == len(table)) || n != len(table) {
			return fmt.Errorf("não foi possível reler a tabela GPT do LBA %d: %w", entriesLBA, err)
		}
		if crc32.ChecksumIEEE(table) != entryCRC {
			return fmt.Errorf("CRC inválido na tabela GPT do LBA %d", entriesLBA)
		}
		return nil
	}
	if err := checkHeader(1, 1, lastLBA, primaryEntriesLBA); err != nil {
		return err
	}
	return checkHeader(lastLBA, lastLBA, 1, backupEntriesLBA)
}

func cloneAllZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func isWindowsRecoveryGPTType(raw []byte) bool {
	if len(raw) < 16 {
		return false
	}
	// de94bba4-06d1-4d40-a16a-bfd50179d6ac, encoded in GPT byte order.
	recovery := [16]byte{0xA4, 0xBB, 0x94, 0xDE, 0xD1, 0x06, 0x40, 0x4D, 0xA1, 0x6A, 0xBF, 0xD5, 0x01, 0x79, 0xD6, 0xAC}
	for i := range recovery {
		if raw[i] != recovery[i] {
			return false
		}
	}
	return true
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(s)
}
