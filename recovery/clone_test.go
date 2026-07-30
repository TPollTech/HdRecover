package recovery

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneDiskAndVerify(t *testing.T) {
	dir := t.TempDir()
	sourceData := bytes.Repeat([]byte("HdRecover-clone-test-"), 128*1024)
	sourcePath := filepath.Join(dir, "source.bin")
	targetPath := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(sourcePath, sourceData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, make([]byte, len(sourceData)+4096), 0o644); err != nil {
		t.Fatal(err)
	}
	source, _ := os.Open(sourcePath)
	defer source.Close()
	target, _ := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	defer target.Close()
	result, err := CloneDisk(CloneOptions{
		Source: source, Destination: target, SourceID: "source", DestinationID: "target",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData) + 4096), SectorSize: 512,
		ChunkSize: 64 * 1024, DamageMode: DamageBalanced, Verify: true, ReportDir: filepath.Join(dir, "report"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified {
		t.Fatalf("expected verified result: %+v", result)
	}
	got := make([]byte, len(sourceData))
	if _, err := target.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sourceData) {
		t.Fatal("clone differs from source")
	}
}

func TestCloneDiskRejectsSmallerDestination(t *testing.T) {
	source := bytes.NewReader(make([]byte, 4096))
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.bin")
	_ = os.WriteFile(targetPath, make([]byte, 1024), 0o644)
	target, _ := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	defer target.Close()
	_, err := CloneDisk(CloneOptions{Source: source, Destination: target, SourceSize: 4096, DestinationSize: 1024, SectorSize: 512, ReportDir: dir})
	if err == nil {
		t.Fatal("expected error")
	}
}

type failingReaderAt struct {
	data  []byte
	start int64
	end   int64
}

func (r failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < r.end && off+int64(len(p)) > r.start {
		return 0, os.ErrInvalid
	}
	if off >= int64(len(r.data)) {
		return 0, os.ErrClosed
	}
	n := copy(p, r.data[off:])
	if n != len(p) {
		return n, os.ErrInvalid
	}
	return n, nil
}

func TestCloneDiskResume(t *testing.T) {
	dir := t.TempDir()
	sourceData := bytes.Repeat([]byte("resume-test-0123456789"), 64*1024)
	source := bytes.NewReader(sourceData)
	targetPath := filepath.Join(dir, "target-resume.bin")
	if err := os.WriteFile(targetPath, make([]byte, len(sourceData)), 0o644); err != nil {
		t.Fatal(err)
	}
	target, _ := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	cancel := make(chan struct{})
	cancelled := false
	result1, err := CloneDisk(CloneOptions{
		Source: source, Destination: target, SourceID: "src", DestinationID: "dst",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData)), SectorSize: 512,
		ChunkSize: 64 * 1024, DamageMode: DamageBalanced, Resume: true, ReportDir: filepath.Join(dir, "report"), Cancel: cancel,
		Progress: func(s CloneStatus) {
			if !cancelled && s.BytesProcessed >= 128*1024 {
				cancelled = true
				close(cancel)
			}
		},
	})
	if err == nil || result1.BytesCopied == 0 || result1.BytesCopied >= int64(len(sourceData)) {
		t.Fatalf("expected interrupted clone, result=%+v err=%v", result1, err)
	}
	_ = target.Close()

	target, _ = os.OpenFile(targetPath, os.O_RDWR, 0o644)
	defer target.Close()
	result2, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target, SourceID: "src", DestinationID: "dst",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData)), SectorSize: 512,
		ChunkSize: 64 * 1024, DamageMode: DamageBalanced, Resume: true, Verify: true, ReportDir: filepath.Join(dir, "report"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result2.Resumed || !result2.Verified {
		t.Fatalf("expected resumed verified clone: %+v", result2)
	}
}

func TestCloneDiskDoesNotResumeWithDifferentHardwareIdentity(t *testing.T) {
	dir := t.TempDir()
	sourceData := bytes.Repeat([]byte("identity-check-"), 32*1024)
	targetPath := filepath.Join(dir, "target-identity.bin")
	if err := os.WriteFile(targetPath, make([]byte, len(sourceData)), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cancel := make(chan struct{})
	cancelled := false
	_, firstErr := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "src", DestinationID: "dst",
		SourceIdentity: "serial=SOURCE-A", DestinationIdentity: "serial=TARGET-A",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData)), SectorSize: 512,
		ChunkSize: 64 * 1024, DamageMode: DamageBalanced, Resume: true,
		ReportDir: filepath.Join(dir, "report-identity"), Cancel: cancel,
		Progress: func(s CloneStatus) {
			if !cancelled && s.BytesProcessed >= 64*1024 {
				cancelled = true
				close(cancel)
			}
		},
	})
	if firstErr == nil {
		t.Fatal("expected the first clone to be interrupted")
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	target, err = os.OpenFile(targetPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	result, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "src", DestinationID: "dst",
		SourceIdentity: "serial=SOURCE-A", DestinationIdentity: "serial=TARGET-B",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData)), SectorSize: 512,
		ChunkSize: 64 * 1024, DamageMode: DamageBalanced, Resume: true, Verify: true,
		ReportDir: filepath.Join(dir, "report-identity"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resumed {
		t.Fatal("a session from another physical destination must not be resumed")
	}
}

func TestCloneDiskZeroFillsUnreadableRange(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x5A}, 256*1024)
	targetPath := filepath.Join(dir, "target-bad.bin")
	_ = os.WriteFile(targetPath, make([]byte, len(data)), 0o644)
	target, _ := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	defer target.Close()
	result, err := CloneDisk(CloneOptions{
		Source: failingReaderAt{data: data, start: 64 * 1024, end: 128 * 1024}, Destination: target,
		SourceID: "src-bad", DestinationID: "dst-bad", SourceSize: int64(len(data)), DestinationSize: int64(len(data)),
		SectorSize: 512, ChunkSize: 64 * 1024, DamageMode: DamageCareful, ReportDir: filepath.Join(dir, "report-bad"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadErrors == 0 {
		t.Fatal("expected bad ranges")
	}
	got := make([]byte, 64*1024)
	if _, err := target.ReadAt(got, 64*1024); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatal("unreadable range was not zero-filled")
	}
}

func TestCloneDiskAdjustsGPTForLargerDestination(t *testing.T) {
	const sector = 512
	const sourceSectors = 8192
	const targetSectors = 16384
	sourceData := make([]byte, sourceSectors*sector)
	entries := make([]byte, 128*128)
	// One synthetic partition entry, enough to make the table non-empty.
	entries[0] = 0xA2
	entries[16] = 0x11
	binary.LittleEndian.PutUint64(entries[32:40], 2048)
	binary.LittleEndian.PutUint64(entries[40:48], 4095)
	entryCRC := crc32.ChecksumIEEE(entries)
	copy(sourceData[2*sector:], entries)

	header := sourceData[sector : 2*sector]
	copy(header[:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(header[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(header[12:16], 92)
	binary.LittleEndian.PutUint64(header[24:32], 1)
	binary.LittleEndian.PutUint64(header[32:40], sourceSectors-1)
	binary.LittleEndian.PutUint64(header[40:48], 34)
	binary.LittleEndian.PutUint64(header[48:56], sourceSectors-34)
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 128)
	binary.LittleEndian.PutUint32(header[84:88], 128)
	binary.LittleEndian.PutUint32(header[88:92], entryCRC)
	binary.LittleEndian.PutUint32(header[16:20], 0)
	binary.LittleEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(header[:92]))

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target-gpt.bin")
	if err := os.WriteFile(targetPath, make([]byte, targetSectors*sector), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	result, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "gpt-source", DestinationID: "gpt-target",
		SourceSize: int64(len(sourceData)), DestinationSize: targetSectors * sector,
		SectorSize: sector, ChunkSize: 64 * 1024, DamageMode: DamageBalanced,
		Verify: true, AdjustGPT: true, ReportDir: filepath.Join(dir, "report-gpt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GPTAdjusted || !result.Verified {
		t.Fatalf("expected verified GPT adjustment: %+v", result)
	}
	primary := make([]byte, sector)
	if _, err := target.ReadAt(primary, sector); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(primary[32:40]); got != targetSectors-1 {
		t.Fatalf("primary alternate LBA = %d", got)
	}
	backup := make([]byte, sector)
	if _, err := target.ReadAt(backup, int64(targetSectors-1)*sector); err != nil {
		t.Fatal(err)
	}
	if string(backup[:8]) != "EFI PART" {
		t.Fatal("backup GPT header not moved")
	}
	storedCRC := binary.LittleEndian.Uint32(backup[16:20])
	binary.LittleEndian.PutUint32(backup[16:20], 0)
	if crc32.ChecksumIEEE(backup[:92]) != storedCRC {
		t.Fatal("backup GPT header CRC invalid")
	}
	result2, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "gpt-source", DestinationID: "gpt-target",
		SourceSize: int64(len(sourceData)), DestinationSize: targetSectors * sector,
		SectorSize: sector, ChunkSize: 64 * 1024, DamageMode: DamageBalanced,
		Resume: true, Verify: true, AdjustGPT: true, ReportDir: filepath.Join(dir, "report-gpt"),
	})
	if err != nil || !result2.GPTAdjusted {
		t.Fatalf("completed GPT session should be safely reusable: result=%+v err=%v", result2, err)
	}
}

func TestCloneDiskAdaptsGPTToSmallerSSDAndDropsRecovery(t *testing.T) {
	const sector = 512
	const originalPhysicalSectors = 20000
	const targetSectors = 16000
	sourceData := make([]byte, targetSectors*sector)

	// Protective MBR.
	sourceData[510] = 0x55
	sourceData[511] = 0xAA
	sourceData[446+4] = 0xEE
	binary.LittleEndian.PutUint32(sourceData[446+8:446+12], 1)
	binary.LittleEndian.PutUint32(sourceData[446+12:446+16], originalPhysicalSectors-1)

	entries := make([]byte, 128*128)
	// Basic data partition that already fits on the smaller SSD.
	entries[0] = 0xA2
	entries[16] = 0x11
	binary.LittleEndian.PutUint64(entries[32:40], 2048)
	binary.LittleEndian.PutUint64(entries[40:48], 14900)
	// Microsoft Recovery partition that extends beyond the destination.
	recoveryType := []byte{0xA4, 0xBB, 0x94, 0xDE, 0xD1, 0x06, 0x40, 0x4D, 0xA1, 0x6A, 0xBF, 0xD5, 0x01, 0x79, 0xD6, 0xAC}
	copy(entries[128:144], recoveryType)
	entries[144] = 0x22
	binary.LittleEndian.PutUint64(entries[128+32:128+40], 14901)
	binary.LittleEndian.PutUint64(entries[128+40:128+48], 19000)
	entryCRC := crc32.ChecksumIEEE(entries)
	copy(sourceData[2*sector:], entries)

	header := sourceData[sector : 2*sector]
	copy(header[:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(header[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(header[12:16], 92)
	binary.LittleEndian.PutUint64(header[24:32], 1)
	binary.LittleEndian.PutUint64(header[32:40], originalPhysicalSectors-1)
	binary.LittleEndian.PutUint64(header[40:48], 34)
	binary.LittleEndian.PutUint64(header[48:56], originalPhysicalSectors-34)
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 128)
	binary.LittleEndian.PutUint32(header[84:88], 128)
	binary.LittleEndian.PutUint32(header[88:92], entryCRC)
	binary.LittleEndian.PutUint32(header[16:20], 0)
	binary.LittleEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(header[:92]))

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target-smaller-gpt.bin")
	if err := os.WriteFile(targetPath, make([]byte, targetSectors*sector), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	result, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "larger-layout", DestinationID: "smaller-ssd",
		SourceSize: int64(len(sourceData)), DestinationSize: targetSectors * sector,
		SectorSize: sector, ChunkSize: 64 * 1024, DamageMode: DamageBalanced,
		Verify: true, AdjustGPT: true, ReportDir: filepath.Join(dir, "report-smaller-gpt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GPTAdjusted || result.PartitionsDropped != 1 || !result.Verified {
		t.Fatalf("expected adjusted GPT with one dropped recovery partition: %+v", result)
	}
	updatedEntries := make([]byte, len(entries))
	if _, err := target.ReadAt(updatedEntries, 2*sector); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(updatedEntries[:128], make([]byte, 128)) {
		t.Fatal("essential data partition was removed")
	}
	if !bytes.Equal(updatedEntries[128:256], make([]byte, 128)) {
		t.Fatal("recovery partition that did not fit was not removed")
	}
	primary := make([]byte, sector)
	if _, err := target.ReadAt(primary, sector); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(primary[32:40]); got != targetSectors-1 {
		t.Fatalf("primary alternate LBA = %d", got)
	}
}

func TestCloneDiskDropsTrailingMBRRecoveryOnSmallerSSD(t *testing.T) {
	const sector = 512
	const targetSectors = 8192
	sourceData := make([]byte, targetSectors*sector)
	sourceData[510] = 0x55
	sourceData[511] = 0xAA
	// Main NTFS partition fits.
	sourceData[446+4] = 0x07
	binary.LittleEndian.PutUint32(sourceData[446+8:446+12], 2048)
	binary.LittleEndian.PutUint32(sourceData[446+12:446+16], 5000)
	// Recovery partition points beyond the physical destination.
	off := 446 + 3*16
	sourceData[off+4] = 0x27
	binary.LittleEndian.PutUint32(sourceData[off+8:off+12], 7500)
	binary.LittleEndian.PutUint32(sourceData[off+12:off+16], 2000)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target-smaller-mbr.bin")
	if err := os.WriteFile(targetPath, make([]byte, len(sourceData)), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(targetPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	result, err := CloneDisk(CloneOptions{
		Source: bytes.NewReader(sourceData), Destination: target,
		SourceID: "mbr-source", DestinationID: "mbr-target",
		SourceSize: int64(len(sourceData)), DestinationSize: int64(len(sourceData)),
		SectorSize: sector, ChunkSize: 64 * 1024, DamageMode: DamageBalanced,
		Verify: true, AdjustGPT: true, ReportDir: filepath.Join(dir, "report-smaller-mbr"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PartitionsDropped != 1 {
		t.Fatalf("expected one dropped MBR recovery partition: %+v", result)
	}
	mbr := make([]byte, sector)
	if _, err := target.ReadAt(mbr, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mbr[off:off+16], make([]byte, 16)) {
		t.Fatal("trailing MBR recovery partition was not cleared")
	}
	if mbr[446+4] != 0x07 {
		t.Fatal("main MBR partition was altered")
	}
}

func TestAdjustGPTRejectsInvalidHeaderCRC(t *testing.T) {
	const sector = 512
	const sectors = 8192
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid-gpt.bin")
	data := make([]byte, sectors*sector)
	data[510] = 0x55
	data[511] = 0xAA
	data[446+4] = 0xEE
	header := data[sector : 2*sector]
	copy(header[:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(header[12:16], 92)
	binary.LittleEndian.PutUint32(header[16:20], 0xDEADBEEF)
	binary.LittleEndian.PutUint64(header[24:32], 1)
	binary.LittleEndian.PutUint64(header[40:48], 34)
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 128)
	binary.LittleEndian.PutUint32(header[84:88], 128)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	disk, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	_, _, err = adjustPartitionTableForDestination(disk, int64(len(data)), sector)
	if err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("expected invalid GPT CRC to be rejected, got %v", err)
	}
}

func TestAdjustMBRRejectsOverlappingPartitions(t *testing.T) {
	const sector = 512
	const sectors = 8192
	dir := t.TempDir()
	path := filepath.Join(dir, "overlapping-mbr.bin")
	data := make([]byte, sectors*sector)
	data[510] = 0x55
	data[511] = 0xAA
	data[446+4] = 0x07
	binary.LittleEndian.PutUint32(data[446+8:446+12], 2048)
	binary.LittleEndian.PutUint32(data[446+12:446+16], 3000)
	second := 446 + 16
	data[second+4] = 0x07
	binary.LittleEndian.PutUint32(data[second+8:second+12], 4096)
	binary.LittleEndian.PutUint32(data[second+12:second+16], 1000)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	disk, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	_, _, err = adjustPartitionTableForDestination(disk, int64(len(data)), sector)
	if err == nil || !strings.Contains(err.Error(), "sobrepõem") {
		t.Fatalf("expected overlapping MBR partitions to be rejected, got %v", err)
	}
}
