package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectMBRNTFSPartition(t *testing.T) {
	const sector = 512
	disk := make([]byte, 16*1024*1024)
	disk[510], disk[511] = 0x55, 0xAA
	entry := disk[446:462]
	entry[4] = 0x07
	binary.LittleEndian.PutUint32(entry[8:12], 2048)
	binary.LittleEndian.PutUint32(entry[12:16], 12000)
	copy(disk[2048*sector+3:2048*sector+11], []byte("NTFS    "))
	parts, err := DetectPartitions(bytes.NewReader(disk), int64(len(disk)), sector)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].FileSystem != "NTFS" || parts[0].Start != 2048*sector {
		t.Fatalf("unexpected partitions: %+v", parts)
	}
}

func TestDecodeRunListFragmentedAndSparse(t *testing.T) {
	// 3 clusters at LCN 10, 2 clusters at LCN 15 (+5), then 1 sparse cluster.
	data := []byte{0x11, 0x03, 0x0A, 0x11, 0x02, 0x05, 0x01, 0x01, 0x00}
	runs, err := decodeRunList(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].LCN != 10 || runs[1].LCN != 15 || !runs[2].Sparse {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestCandidateExternalSortAndDedupe(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.bin")
	store, err := openCandidateStore(in, false)
	if err != nil {
		t.Fatal(err)
	}
	items := []scanCandidate{{offset: 30, kind: 2}, {offset: 10, kind: 1}, {offset: 10, kind: 1}, {offset: 10, kind: 0}, {offset: 20, kind: 1}}
	if err := store.Append(items); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.bin")
	count, err := sortCandidateFile(in, out, filepath.Join(dir, "tmp"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("count=%d", count)
	}
	it, err := openCandidateIterator(out)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var got []scanCandidate
	for {
		c, err := it.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	want := []scanCandidate{{offset: 10, kind: 0}, {offset: 10, kind: 1}, {offset: 20, kind: 1}, {offset: 30, kind: 2}}
	if len(got) != len(want) {
		t.Fatalf("got=%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%+v want=%+v", got, want)
		}
	}
}

type failReader struct{ data []byte }

func (r failReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= 4096 && off < 8192 {
		return 0, errors.New("bad sector")
	}
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	if off < 4096 && off+int64(len(p)) > 4096 {
		n := copy(p, r.data[off:4096])
		return n, errors.New("bad sector")
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestAdaptiveReaderRecordsBadRange(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 16384)
	r := newAdaptiveReaderAt(failReader{data: data}, 512, DamageCareful, nil, nil)
	buf := make([]byte, 8192)
	n, _ := r.ReadAt(buf, 0)
	if n != len(buf) {
		t.Fatalf("read=%d", n)
	}
	bad := r.BadRanges()
	if len(bad) == 0 {
		t.Fatal("expected bad range")
	}
	if _, err := os.Stat(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestParseDeletedMFTRecordWithResidentData(t *testing.T) {
	rec := make([]byte, 1024)
	copy(rec[:4], []byte("FILE"))
	binary.LittleEndian.PutUint16(rec[4:6], 48) // USA offset
	binary.LittleEndian.PutUint16(rec[6:8], 3)  // two sectors + sequence
	binary.LittleEndian.PutUint16(rec[20:22], 56)
	binary.LittleEndian.PutUint16(rec[22:24], 0) // deleted, regular file
	binary.LittleEndian.PutUint32(rec[44:48], 42)
	binary.LittleEndian.PutUint16(rec[48:50], 0xA55A)
	binary.LittleEndian.PutUint16(rec[50:52], 0x1111)
	binary.LittleEndian.PutUint16(rec[52:54], 0x2222)
	binary.LittleEndian.PutUint16(rec[510:512], 0xA55A)
	binary.LittleEndian.PutUint16(rec[1022:1024], 0xA55A)

	name := "foto.jpg"
	nameUTF := make([]byte, len([]rune(name))*2)
	for i, r := range []rune(name) {
		binary.LittleEndian.PutUint16(nameUTF[i*2:i*2+2], uint16(r))
	}
	nameValueLen := 66 + len(nameUTF)
	nameAttrLen := (24 + nameValueLen + 7) &^ 7
	pos := 56
	binary.LittleEndian.PutUint32(rec[pos:pos+4], ntfsAttrFileName)
	binary.LittleEndian.PutUint32(rec[pos+4:pos+8], uint32(nameAttrLen))
	binary.LittleEndian.PutUint32(rec[pos+16:pos+20], uint32(nameValueLen))
	binary.LittleEndian.PutUint16(rec[pos+20:pos+22], 24)
	v := rec[pos+24 : pos+24+nameValueLen]
	binary.LittleEndian.PutUint64(v[:8], 5)
	v[64] = byte(len([]rune(name)))
	v[65] = 1
	copy(v[66:], nameUTF)

	pos += nameAttrLen
	data := []byte("hello deleted world")
	dataAttrLen := (24 + len(data) + 7) &^ 7
	binary.LittleEndian.PutUint32(rec[pos:pos+4], ntfsAttrData)
	binary.LittleEndian.PutUint32(rec[pos+4:pos+8], uint32(dataAttrLen))
	binary.LittleEndian.PutUint32(rec[pos+16:pos+20], uint32(len(data)))
	binary.LittleEndian.PutUint16(rec[pos+20:pos+22], 24)
	copy(rec[pos+24:], data)
	pos += dataAttrLen
	binary.LittleEndian.PutUint32(rec[pos:pos+4], ntfsAttrEnd)

	if err := applyNTFSFixup(rec, 512); err != nil {
		t.Fatal(err)
	}
	entry, err := parseMFTRecord(rec, 42, ntfsVolume{clusterSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if entry.inUse || entry.directory || entry.name != name || string(entry.data.resident) != string(data) || entry.parent != 5 {
		t.Fatalf("unexpected entry: %+v data=%q", entry, entry.data.resident)
	}
}

func TestReadNTFSFragmentedStream(t *testing.T) {
	const cluster = int64(16)
	disk := make([]byte, 128)
	copy(disk[2*cluster:3*cluster], []byte("AAAAAAAAAAAAAAAA"))
	copy(disk[5*cluster:6*cluster], []byte("BBBBBBBBBBBBBBBB"))
	vol := ntfsVolume{start: 0, size: int64(len(disk)), clusterSize: cluster}
	runs := []ntfsRun{{VCN: 0, LCN: 2, Clusters: 1}, {VCN: 1, LCN: 5, Clusters: 1}}
	out := make([]byte, 32)
	if err := readNTFSStreamAt(bytes.NewReader(disk), vol, runs, 0, out); err != nil {
		t.Fatal(err)
	}
	if string(out) != "AAAAAAAAAAAAAAAABBBBBBBBBBBBBBBB" {
		t.Fatalf("unexpected data: %q", out)
	}
}

func TestWriteHTMLReportWithFiltersAndBadMap(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "foto.jpg")
	if err := os.WriteFile(imagePath, []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o644); err != nil {
		t.Fatal(err)
	}
	result := Result{
		OutputDir:    dir,
		FilesFound:   1,
		Candidates:   2,
		BytesScanned: 1024 * 1024,
		ReadErrors:   1,
		BadRanges:    []BadRange{{Offset: 4096, Length: 512, Error: "teste"}},
		Duration:     time.Second,
		RecoveredFiles: []RecoveredFile{{
			Path: imagePath, OriginalName: "foto.jpg", Extension: "jpg", Category: "Fotos",
			Size: 4, Integrity: IntegrityLikely, Method: "Teste",
		}},
	}
	if err := writeHTMLReport(dir, result, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "RESULTADOS.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{"Pesquisar nome", "data-category", "Mapa aproximado", "foto.jpg"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in report", expected)
		}
	}
}
