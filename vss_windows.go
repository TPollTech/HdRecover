//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const fsctlAllowExtendedDASDIO = 0x00090083

type volumeExtentInfo struct {
	Letter     string
	DiskNumber int
	Offset     int64
	Length     int64
}

type vssSnapshotSet struct {
	ranges  []mappedReadRange
	letters []string
	files   []*os.File
	workDir string
	log     func(string)
	closed  bool
}

func (s *vssSnapshotSet) Reader(base io.ReaderAt) io.ReaderAt {
	return newMappedRangeReader(base, s.ranges)
}

func (s *vssSnapshotSet) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	for _, f := range s.files {
		if f != nil {
			_ = f.Close()
		}
	}
	s.files = nil

	if len(s.letters) > 0 {
		var script strings.Builder
		script.WriteString("set context persistent\r\nset verbose on\r\n")
		for _, letter := range s.letters {
			script.WriteString("delete shadows exposed ")
			script.WriteString(letter)
			script.WriteString("\r\n")
		}
		script.WriteString("reset\r\n")
		cleanupPath := filepath.Join(s.workDir, "cleanup.dsh")
		_ = os.WriteFile(cleanupPath, []byte(script.String()), 0o600)
		if out, err := runHiddenCommand(diskshadowExecutable(), "-s", cleanupPath); err != nil {
			if s.log != nil {
				s.log("Aviso: não foi possível remover automaticamente a cópia de sombra VSS: " + compactCommandOutput(out, err))
			}
		} else if s.log != nil {
			s.log("Cópias de sombra VSS removidas com segurança.")
		}
	}
	_ = os.RemoveAll(s.workDir)
}

func createVSSSnapshotSet(source diskInfo, reportDir string, log func(string)) (*vssSnapshotSet, error) {
	if !diskContainsProtectedSystemRole(source) {
		return nil, errors.New("o modo VSS exige que a origem contenha o Windows atualmente em uso")
	}
	if len(source.DriveLetters) == 0 {
		return nil, errors.New("nenhum volume montado da origem foi encontrado para criar a cópia de sombra")
	}
	if err := ensureNoBitLockerVolumes(source); err != nil {
		return nil, err
	}

	extents := make([]volumeExtentInfo, 0, len(source.DriveLetters))
	for _, letter := range source.DriveLetters {
		extent, err := querySingleVolumeExtent(letter)
		if err != nil {
			return nil, fmt.Errorf("o volume %s não pôde ser incluído no conjunto VSS: %w", letter, err)
		}
		if extent.DiskNumber != source.Index {
			return nil, fmt.Errorf("o volume %s mudou de disco físico durante a preparação do VSS", letter)
		}
		extents = append(extents, extent)
	}
	if len(extents) != len(source.DriveLetters) {
		return nil, errors.New("nem todos os volumes montados da origem puderam ser protegidos por VSS")
	}

	exposeLetters, err := chooseFreeDriveLetters(len(extents))
	if err != nil {
		return nil, err
	}

	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	workDir := filepath.Join(base, "HdRecover", fmt.Sprintf("VSS-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("não foi possível criar a pasta temporária do VSS: %w", err)
	}

	metadataPath := filepath.Join(workDir, "metadata.cab")
	scriptPath := filepath.Join(workDir, "create.dsh")
	var script strings.Builder
	script.WriteString("set context persistent\r\n")
	script.WriteString("set metadata ")
	script.WriteString(metadataPath)
	script.WriteString("\r\nset verbose on\r\nbegin backup\r\n")
	for i, extent := range extents {
		script.WriteString(fmt.Sprintf("add volume %s alias hdrecovervss%d\r\n", normalizeDriveLetter(extent.Letter), i))
	}
	script.WriteString("create\r\n")
	for i, expose := range exposeLetters {
		script.WriteString(fmt.Sprintf("expose %%hdrecovervss%d%% %s\r\n", i, expose))
	}
	script.WriteString("end backup\r\n")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o600); err != nil {
		_ = os.RemoveAll(workDir)
		return nil, err
	}

	if log != nil {
		log("Criando cópia de sombra VSS consistente do Windows em uso...")
	}
	output, cmdErr := runHiddenCommand(diskshadowExecutable(), "-s", scriptPath)
	_ = os.MkdirAll(reportDir, 0o755)
	_ = os.WriteFile(filepath.Join(reportDir, "VSS_DISKSHADOW.log"), []byte(output), 0o644)
	if cmdErr != nil {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("o Windows não conseguiu criar a cópia de sombra VSS: %s", compactCommandOutput(output, cmdErr))
	}

	set := &vssSnapshotSet{workDir: workDir, log: log, letters: append([]string(nil), exposeLetters...)}
	cleanupOnError := func(err error) (*vssSnapshotSet, error) {
		set.Close()
		return nil, err
	}

	for i, extent := range extents {
		expose := exposeLetters[i]
		path := `\\.\` + strings.TrimSuffix(expose, `\`)
		handle, openErr := openDeviceAdvanced(path, genericRead, fileShareRead|fileShareWrite, fileAttributeNormal|fileFlagSequentialScan)
		if openErr != nil {
			return cleanupOnError(fmt.Errorf("a cópia de sombra do volume %s foi criada, mas não pôde ser aberta: %w", extent.Letter, openErr))
		}
		var returned uint32
		procDiskDeviceIoControl.Call(handle, fsctlAllowExtendedDASDIO, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
		file := os.NewFile(handle, path)
		if file == nil {
			closeDevice(handle)
			return cleanupOnError(fmt.Errorf("não foi possível criar o leitor da cópia de sombra %s", expose))
		}
		set.files = append(set.files, file)
		set.ranges = append(set.ranges, mappedReadRange{
			Start: extent.Offset, End: extent.Offset + extent.Length,
			Label: extent.Letter, Reader: file,
		})
		if log != nil {
			log(fmt.Sprintf("VSS pronto: %s (%s) será lido de forma consistente.", normalizeDriveLetter(extent.Letter), humanBytes(extent.Length)))
		}
	}
	if len(set.ranges) == 0 {
		return cleanupOnError(errors.New("nenhuma cópia de sombra pôde ser aberta"))
	}
	return set, nil
}

type bitLockerVolumeStatus struct {
	MountPoint           string `json:"MountPoint"`
	VolumeStatus         string `json:"VolumeStatus"`
	ProtectionStatus     string `json:"ProtectionStatus"`
	EncryptionPercentage int    `json:"EncryptionPercentage"`
}

func ensureNoBitLockerVolumes(source diskInfo) error {
	letters := make([]string, 0, len(source.DriveLetters))
	for _, letter := range source.DriveLetters {
		if normalized := normalizeDriveLetter(letter); normalized != "" {
			letters = append(letters, normalized)
		}
	}
	if len(letters) == 0 {
		return errors.New("não foi possível identificar os volumes da origem para verificar o BitLocker")
	}
	var script strings.Builder
	script.WriteString(`$ErrorActionPreference='Stop'; $items=@(); `)
	for _, letter := range letters {
		script.WriteString(`$v=Get-BitLockerVolume -MountPoint '`)
		script.WriteString(letter)
		script.WriteString(`' -ErrorAction Stop; if($null -eq $v){throw 'BitLocker sem resposta'}; $items += [pscustomobject]@{MountPoint=[string]$v.MountPoint;VolumeStatus=[string]$v.VolumeStatus;ProtectionStatus=[string]$v.ProtectionStatus;EncryptionPercentage=[int]$v.EncryptionPercentage}; `)
	}
	script.WriteString(`ConvertTo-Json -InputObject $items -Depth 4 -Compress`)
	out, err := runPowerShell(script.String())
	if err != nil {
		return fmt.Errorf("não foi possível confirmar o estado do BitLocker; a migração VSS foi bloqueada: %s", compactCommandOutput(out, err))
	}
	text := strings.TrimSpace(strings.TrimPrefix(out, "\ufeff"))
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end < start {
		return errors.New("o Windows retornou um estado de BitLocker inválido; a migração VSS foi bloqueada")
	}
	var statuses []bitLockerVolumeStatus
	if err := json.Unmarshal([]byte(text[start:end+1]), &statuses); err != nil || len(statuses) != len(letters) {
		return errors.New("não foi possível validar todos os volumes no BitLocker; a migração VSS foi bloqueada")
	}
	for _, status := range statuses {
		if !strings.EqualFold(strings.TrimSpace(status.VolumeStatus), "FullyDecrypted") ||
			!strings.EqualFold(strings.TrimSpace(status.ProtectionStatus), "Off") ||
			status.EncryptionPercentage != 0 {
			return fmt.Errorf("o volume %s usa ou está alterando BitLocker; descriptografe-o completamente antes da migração VSS", status.MountPoint)
		}
	}
	return nil
}

func querySingleVolumeExtent(letter string) (volumeExtentInfo, error) {
	normalized := normalizeDriveLetter(letter)
	if normalized == "" {
		return volumeExtentInfo{}, errors.New("letra de unidade inválida")
	}
	h, err := openDevice(`\\.\`+normalized, 0)
	if err != nil {
		return volumeExtentInfo{}, err
	}
	defer closeDevice(h)

	out := make([]byte, 4096)
	var returned uint32
	ok, _, callErr := procDiskDeviceIoControl.Call(
		h, ioctlVolumeExtents, 0, 0,
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)),
		uintptr(unsafe.Pointer(&returned)), 0,
	)
	if ok == 0 || returned < 32 {
		return volumeExtentInfo{}, fmt.Errorf("DeviceIoControl falhou: %v", callErr)
	}
	count := uint32At(out, 0)
	if count != 1 {
		return volumeExtentInfo{}, fmt.Errorf("volume dinâmico ou distribuído com %d extensões não é suportado no modo VSS", count)
	}
	return volumeExtentInfo{
		Letter:     normalized,
		DiskNumber: int(uint32At(out, 8)),
		Offset:     int64(uint64At(out, 16)),
		Length:     int64(uint64At(out, 24)),
	}, nil
}

func uint64At(b []byte, offset int) uint64 {
	if offset < 0 || offset+8 > len(b) {
		return 0
	}
	return uint64(b[offset]) |
		uint64(b[offset+1])<<8 |
		uint64(b[offset+2])<<16 |
		uint64(b[offset+3])<<24 |
		uint64(b[offset+4])<<32 |
		uint64(b[offset+5])<<40 |
		uint64(b[offset+6])<<48 |
		uint64(b[offset+7])<<56
}

func normalizeDriveLetter(letter string) string {
	s := strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(letter, `\`)))
	if len(s) >= 2 && s[1] == ':' && s[0] >= 'A' && s[0] <= 'Z' {
		return s[:2]
	}
	return ""
}

func chooseFreeDriveLetters(count int) ([]string, error) {
	mask, _, _ := procGetLogicalDrives.Call()
	result := make([]string, 0, count)
	for c := byte('Z'); c >= 'D' && len(result) < count; c-- {
		bit := uintptr(1) << uintptr(c-'A')
		if mask&bit == 0 {
			result = append(result, fmt.Sprintf("%c:", c))
		}
		if c == 'D' {
			break
		}
	}
	if len(result) != count {
		return nil, errors.New("não há letras de unidade livres suficientes para expor as cópias de sombra VSS")
	}
	return result, nil
}

func diskshadowExecutable() string {
	root := strings.TrimSpace(os.Getenv("SystemRoot"))
	if root == "" {
		return "diskshadow.exe"
	}
	return filepath.Join(root, "System32", "diskshadow.exe")
}

func runHiddenCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func compactCommandOutput(output string, err error) string {
	text := strings.TrimSpace(strings.ReplaceAll(output, "\x00", ""))
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' })
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	compact := strings.Join(lines, " | ")
	if compact == "" && err != nil {
		compact = err.Error()
	} else if err != nil {
		compact += " | " + err.Error()
	}
	return compact
}

func setDiskOfflineBestEffort(index int, log func(string)) {
	command := fmt.Sprintf("Set-Disk -Number %d -IsOffline $true -ErrorAction Stop", index)
	out, err := runHiddenCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	if err != nil {
		if log != nil {
			log("Aviso: a clonagem terminou, mas o Windows não colocou o destino offline automaticamente: " + compactCommandOutput(out, err))
		}
		return
	}
	if log != nil {
		log("Disco de destino colocado offline para evitar conflito de assinatura com a origem.")
	}
}
