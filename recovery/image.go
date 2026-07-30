package recovery

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func createDiskImage(opts Options, source io.ReaderAt, adaptive *adaptiveReaderAt, outDir string, session *SessionState, sessionPath string) (Result, error) {
	started := time.Now()
	rangeStart, rangeSize := normalizeRange(opts)
	name := strings.TrimSpace(opts.ImageName)
	if name == "" {
		name = "HdRecover_imagem_" + time.Now().Format("20060102_150405") + ".img"
	}
	if filepath.Ext(name) == "" {
		name += ".img"
	}
	imagePath := filepath.Join(opts.Destination, name)
	if session != nil && session.ImagePath != "" {
		imagePath = session.ImagePath
	}

	flags := os.O_CREATE | os.O_WRONLY
	startAt := int64(0)
	if opts.Resume {
		flags |= os.O_APPEND
		if st, err := os.Stat(imagePath); err == nil {
			startAt = st.Size()
			if startAt > rangeSize {
				return Result{OutputDir: outDir, ImagePath: imagePath}, fmt.Errorf("a imagem existente é maior que a origem selecionada")
			}
		}
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(imagePath, flags, 0o644)
	if err != nil {
		return Result{OutputDir: outDir}, err
	}
	defer f.Close()

	const chunkSize = 16 * 1024 * 1024
	buf := make([]byte, chunkSize)
	offset := startAt
	lastCheckpoint := time.Now()
	logf(opts, fmt.Sprintf("Criando imagem somente leitura de %s em %s.", humanBytes(rangeSize), imagePath))
	if startAt > 0 {
		logf(opts, "Retomando imagem em "+humanBytes(startAt)+".")
	}

	for offset < rangeSize {
		if err := waitControl(opts.Cancel, opts.Paused); err != nil {
			result := Result{OutputDir: outDir, ImagePath: imagePath, BytesScanned: offset, BadRanges: adaptive.BadRanges(), Duration: time.Since(started), Resumed: startAt > 0}
			_ = writeBadMap(outDir, result.BadRanges)
			return result, err
		}
		want := int64(len(buf))
		if rangeSize-offset < want {
			want = rangeSize - offset
		}
		n, readErr := adaptive.ReadAt(buf[:want], rangeStart+offset)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return Result{OutputDir: outDir, ImagePath: imagePath, BytesScanned: offset, BadRanges: adaptive.BadRanges(), Duration: time.Since(started)}, err
			}
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			logf(opts, fmt.Sprintf("Trecho problemático próximo de %s: %v", humanBytes(rangeStart+offset), readErr))
		}
		if opts.Progress != nil {
			opts.Progress(Status{Phase: "imaging", BytesScanned: offset, TotalBytes: rangeSize, ReadErrors: len(adaptive.BadRanges()), Current: "Criando imagem de segurança"})
		}
		if session != nil && (time.Since(lastCheckpoint) >= 2*time.Second || offset >= rangeSize) {
			session.Phase = "imaging"
			session.ScanOffset = offset
			session.ImagePath = imagePath
			session.ReadErrors = len(adaptive.BadRanges())
			_ = saveSession(sessionPath, *session)
			lastCheckpoint = time.Now()
		}
	}
	if err := f.Sync(); err != nil {
		return Result{OutputDir: outDir, ImagePath: imagePath, BytesScanned: offset, BadRanges: adaptive.BadRanges(), Duration: time.Since(started)}, err
	}
	bad := adaptive.BadRanges()
	_ = writeBadMap(outDir, bad)
	result := Result{OutputDir: outDir, ImagePath: imagePath, BytesScanned: rangeSize, ReadErrors: len(bad), BadRanges: bad, Duration: time.Since(started), Resumed: startAt > 0}
	if session != nil {
		session.Completed = true
		session.Phase = "completed"
		session.ScanOffset = rangeSize
		session.ImagePath = imagePath
		session.ReadErrors = len(bad)
		_ = saveSession(sessionPath, *session)
	}
	return result, nil
}

func writeBadMap(outDir string, ranges []BadRange) error {
	jsonPath := filepath.Join(outDir, "MAPA_DE_SETORES_RUINS.json")
	data, _ := json.MarshalIndent(ranges, "", "  ")
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return err
	}
	csvPath := filepath.Join(outDir, "MAPA_DE_SETORES_RUINS.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"offset", "tamanho", "erro"})
	for _, r := range ranges {
		_ = w.Write([]string{strconv.FormatInt(r.Offset, 10), strconv.FormatInt(r.Length, 10), r.Error})
	}
	w.Flush()
	closeErr := f.Close()
	if err := w.Error(); err != nil {
		return err
	}
	return closeErr
}
