package recovery

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverFromSyntheticDisk(t *testing.T) {
	root := t.TempDir()
	diskPath := filepath.Join(root, "disk.img")
	outPath := filepath.Join(root, "out")
	if err := os.MkdirAll(outPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var disk bytes.Buffer
	disk.Write(make([]byte, 4096))

	jpeg := []byte{0xFF, 0xD8}
	jpeg = append(jpeg, 0xFF, 0xE0, 0x00, 0x10)
	jpeg = append(jpeg, bytes.Repeat([]byte{0x00}, 14)...)
	jpeg = append(jpeg, 0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01, 0x01, 0x11, 0x00)
	jpeg = append(jpeg, 0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3F, 0x00)
	jpeg = append(jpeg, bytes.Repeat([]byte{0x41}, 300)...)
	jpeg = append(jpeg, 0xFF, 0xD9)
	disk.Write(jpeg)
	disk.Write(make([]byte, 4096))

	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" + string(bytes.Repeat([]byte("A"), 100)) + "\n%%EOF")
	disk.Write(pdf)
	disk.Write(make([]byte, 4096))

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("<document>teste</document>"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	disk.Write(zipBuf.Bytes())
	disk.Write(make([]byte, 4096))

	bmp := make([]byte, 128)
	copy(bmp[:2], "BM")
	binary.LittleEndian.PutUint32(bmp[2:6], uint32(len(bmp)))
	binary.LittleEndian.PutUint32(bmp[10:14], 54)
	binary.LittleEndian.PutUint32(bmp[14:18], 40)
	disk.Write(bmp)
	disk.Write(make([]byte, 4096))

	sqlite := make([]byte, 1024)
	copy(sqlite[:16], []byte("SQLite format 3\x00"))
	binary.BigEndian.PutUint16(sqlite[16:18], 512)
	binary.BigEndian.PutUint32(sqlite[28:32], 2)
	disk.Write(sqlite)
	disk.Write(make([]byte, 4096))

	heic := make([]byte, 24+1024)
	binary.BigEndian.PutUint32(heic[0:4], 24)
	copy(heic[4:8], "ftyp")
	copy(heic[8:12], "heic")
	binary.BigEndian.PutUint32(heic[24:28], 1024)
	copy(heic[28:32], "mdat")
	disk.Write(heic)
	disk.Write(make([]byte, 4096))

	if err := os.WriteFile(diskPath, disk.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawScanning, sawExtracting bool
	result, err := Recover(Options{
		SourcePath:  diskPath,
		SourceSize:  int64(disk.Len()),
		Destination: outPath,
		Categories: Categories{
			Images:    true,
			Documents: true,
			Archives:  true,
		},
		Progress: func(status Status) {
			if status.Phase == PhaseScanning {
				sawScanning = true
			}
			if status.Phase == PhaseExtracting {
				sawExtracting = true
			}
		},
	})
	if err != nil {
		t.Fatalf("Recover returned error: %v", err)
	}
	if result.FilesFound < 6 {
		t.Fatalf("expected at least 6 recovered files, got %d", result.FilesFound)
	}
	if !sawScanning || !sawExtracting {
		t.Fatalf("expected both progress phases, scanning=%v extracting=%v", sawScanning, sawExtracting)
	}
	if _, err := os.Stat(filepath.Join(result.OutputDir, "RELATORIO_DA_RECUPERACAO.txt")); err != nil {
		t.Fatalf("expected recovery report: %v", err)
	}

	var foundDocx bool
	err = filepath.Walk(result.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && filepath.Ext(path) == ".docx" {
			foundDocx = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !foundDocx {
		t.Fatal("expected ZIP-based Office file to be classified as .docx")
	}
}

func TestSignatureAcrossScanChunkBoundary(t *testing.T) {
	root := t.TempDir()
	diskPath := filepath.Join(root, "boundary.img")
	outPath := filepath.Join(root, "out")
	if err := os.MkdirAll(outPath, 0o755); err != nil {
		t.Fatal(err)
	}

	const chunk = 8 * 1024 * 1024
	data := make([]byte, chunk+1024*1024)
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" + string(bytes.Repeat([]byte("B"), 256)) + "\n%%EOF")
	start := chunk - 3 // a assinatura %PDF- atravessa a borda dos blocos
	copy(data[start:], pdf)
	if err := os.WriteFile(diskPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Recover(Options{
		SourcePath:  diskPath,
		SourceSize:  int64(len(data)),
		SectorSize:  512,
		Destination: outPath,
		Categories:  Categories{Documents: true},
		Performance: PerformanceCompatibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesFound != 1 {
		t.Fatalf("expected one PDF across the chunk boundary, got %d", result.FilesFound)
	}
}
