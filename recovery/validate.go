package recovery

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func validateRecoveredFile(path, ext string, expectedSize int64, duplicate bool) (Integrity, string) {
	if duplicate {
		return IntegrityGood, "Duplicado exato; não foi gravada outra cópia."
	}
	st, err := os.Stat(path)
	if err != nil {
		return IntegrityDamaged, "Arquivo de saída não pôde ser aberto."
	}
	if expectedSize > 0 && st.Size() != expectedSize {
		return IntegrityPartial, "Tamanho gravado difere do tamanho previsto."
	}
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	switch ext {
	case "jpg", "jpeg", "png", "gif":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		cfg, format, err := image.DecodeConfig(f)
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			return IntegrityDamaged, "A imagem não passou na decodificação estrutural."
		}
		return IntegrityGood, "Imagem validada: " + format + "."
	case "zip", "docx", "xlsx", "pptx", "apk", "jar":
		zr, err := zip.OpenReader(path)
		if err != nil {
			return IntegrityDamaged, "Diretório ZIP inválido."
		}
		defer zr.Close()
		if len(zr.File) == 0 {
			return IntegrityPartial, "Contêiner ZIP sem entradas."
		}
		for _, zf := range zr.File {
			r, err := zf.Open()
			if err != nil {
				return IntegrityDamaged, "Entrada ZIP não pôde ser aberta."
			}
			_, copyErr := io.Copy(io.Discard, r)
			closeErr := r.Close()
			if copyErr != nil || closeErr != nil {
				return IntegrityDamaged, "CRC ou conteúdo ZIP inválido."
			}
		}
		return IntegrityGood, "Estrutura ZIP e CRC das entradas validados."
	case "pdf":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		head := make([]byte, 8)
		_, _ = io.ReadFull(f, head)
		if !bytes.HasPrefix(head, []byte("%PDF-")) {
			return IntegrityDamaged, "Cabeçalho PDF ausente."
		}
		tailSize := int64(4096)
		if st.Size() < tailSize {
			tailSize = st.Size()
		}
		tail := make([]byte, tailSize)
		_, _ = f.ReadAt(tail, st.Size()-tailSize)
		if !bytes.Contains(tail, []byte("%%EOF")) {
			return IntegrityPartial, "Marcador final do PDF não foi localizado."
		}
		return IntegrityLikely, "Cabeçalho e final PDF localizados."
	case "sqlite", "db", "sqlite3":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		h := make([]byte, 100)
		if _, err := io.ReadFull(f, h); err != nil || !bytes.Equal(h[:16], []byte("SQLite format 3\x00")) {
			return IntegrityDamaged, "Cabeçalho SQLite inválido."
		}
		page := int(binary.BigEndian.Uint16(h[16:18]))
		if page == 1 {
			page = 65536
		}
		if page < 512 || page > 65536 || page&(page-1) != 0 {
			return IntegrityDamaged, "Tamanho de página SQLite inválido."
		}
		if st.Size()%int64(page) != 0 {
			return IntegrityPartial, "O tamanho não fecha em páginas SQLite completas."
		}
		return IntegrityLikely, "Cabeçalho e páginas SQLite coerentes."
	case "mp4", "mov", "3gp", "heic", "heif", "avif", "m4a":
		return validateISOBMFF(path, st.Size())
	case "avi", "wav", "webp":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		h := make([]byte, 12)
		if _, err := io.ReadFull(f, h); err != nil || string(h[:4]) != "RIFF" {
			return IntegrityDamaged, "Cabeçalho RIFF inválido."
		}
		reported := int64(binary.LittleEndian.Uint32(h[4:8])) + 8
		if reported > st.Size() {
			return IntegrityPartial, "Contêiner RIFF parece truncado."
		}
		return IntegrityLikely, "Cabeçalho e tamanho RIFF coerentes."
	case "bmp":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		h := make([]byte, 18)
		if _, err := io.ReadFull(f, h); err != nil || string(h[:2]) != "BM" {
			return IntegrityDamaged, "Cabeçalho BMP inválido."
		}
		reported := int64(binary.LittleEndian.Uint32(h[2:6]))
		if reported != st.Size() {
			return IntegrityPartial, "Tamanho BMP não confere."
		}
		return IntegrityGood, "Cabeçalho e tamanho BMP validados."
	case "doc", "xls", "ppt", "ole":
		f, err := os.Open(path)
		if err != nil {
			return IntegrityDamaged, err.Error()
		}
		defer f.Close()
		h := make([]byte, 8)
		_, _ = io.ReadFull(f, h)
		if !bytes.Equal(h, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
			return IntegrityDamaged, "Cabeçalho OLE inválido."
		}
		return IntegrityLikely, "Cabeçalho OLE válido."
	default:
		if st.Size() == 0 {
			return IntegrityDamaged, "Arquivo vazio."
		}
		return IntegrityUnverified, "Formato sem validador específico nesta versão."
	}
}

func validateISOBMFF(path string, size int64) (Integrity, string) {
	f, err := os.Open(path)
	if err != nil {
		return IntegrityDamaged, err.Error()
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1024*1024)
	var offset int64
	seenFtyp, seenData, seenMeta := false, false, false
	for offset+8 <= size && offset < 64*1024*1024 {
		h := make([]byte, 8)
		if _, err := io.ReadFull(r, h); err != nil {
			break
		}
		boxSize := int64(binary.BigEndian.Uint32(h[:4]))
		boxType := string(h[4:8])
		headerSize := int64(8)
		if boxSize == 1 {
			ext := make([]byte, 8)
			if _, err := io.ReadFull(r, ext); err != nil {
				return IntegrityPartial, "Caixa ISO BMFF estendida truncada."
			}
			boxSize = int64(binary.BigEndian.Uint64(ext))
			headerSize = 16
		} else if boxSize == 0 {
			boxSize = size - offset
		}
		if boxSize < headerSize || offset+boxSize > size {
			return IntegrityPartial, "Caixa ISO BMFF excede o arquivo."
		}
		switch boxType {
		case "ftyp":
			seenFtyp = true
		case "mdat":
			seenData = true
		case "moov", "meta":
			seenMeta = true
		}
		toSkip := boxSize - headerSize
		if toSkip > 0 {
			if _, err := io.CopyN(io.Discard, r, toSkip); err != nil {
				return IntegrityPartial, "Caixa ISO BMFF truncada."
			}
		}
		offset += boxSize
		if seenFtyp && seenData && seenMeta {
			return IntegrityLikely, "Contêiner ISO BMFF contém ftyp, dados e metadados."
		}
	}
	if !seenFtyp {
		return IntegrityDamaged, "Caixa ftyp ausente."
	}
	if seenData || seenMeta {
		return IntegrityLikely, "Estrutura ISO BMFF parcialmente validada."
	}
	return IntegrityPartial, "Somente o cabeçalho ISO BMFF foi confirmado."
}

var _ = filepath.Separator
var _ = errors.Is
