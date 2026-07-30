//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	genericRead               = 0x80000000
	genericWrite              = 0x40000000
	fileShareRead             = 0x00000001
	fileShareWrite            = 0x00000002
	openExisting              = 3
	fileAttributeNormal       = 0x00000080
	fileFlagWriteThrough      = 0x80000000
	fileFlagSequentialScan    = 0x08000000
	fsctlLockVolume           = 0x00090018
	fsctlUnlockVolume         = 0x0009001C
	fsctlDismountVolume       = 0x00090020
	ioctlDiskUpdateProperties = 0x00070140
	ioctlDiskGeometryEx       = 0x000700A0
	ioctlStorageQuery         = 0x002D1400
	ioctlVolumeExtents        = 0x00560000
	ioctlPredictFailure       = 0x002D1100
	maxPhysicalDrives         = 32
	diskProbeTimeout          = 2500 * time.Millisecond
)

var (
	diskKernel32            = syscall.NewLazyDLL("kernel32.dll")
	procDiskCreateFileW     = diskKernel32.NewProc("CreateFileW")
	procDiskCloseHandle     = diskKernel32.NewProc("CloseHandle")
	procDiskDeviceIoControl = diskKernel32.NewProc("DeviceIoControl")
	procGetLogicalDrives    = diskKernel32.NewProc("GetLogicalDrives")
	procGetDiskFreeSpaceExW = diskKernel32.NewProc("GetDiskFreeSpaceExW")
)

type diskInfo struct {
	Index            int
	Model            string
	Size             int64
	InterfaceType    string
	BytesPerSector   int64
	DriveLetters     []string
	Removable        bool
	TrimEnabled      bool
	SeekPenalty      bool
	Health           string
	Serial           string
	UniqueID         string
	PartitionStyle   string
	IsBoot           bool
	IsSystem         bool
	IsReadOnly       bool
	IsOffline        bool
	MetadataVerified bool
	CustomPath       string
}

func (d diskInfo) Path() string {
	if d.CustomPath != "" {
		return d.CustomPath
	}
	return fmt.Sprintf(`\\.\PhysicalDrive%d`, d.Index)
}

func (d diskInfo) StableIdentity() string {
	parts := []string{
		fmt.Sprintf("size=%d", d.Size),
		fmt.Sprintf("sector=%d", d.BytesPerSector),
	}
	if value := strings.TrimSpace(d.UniqueID); value != "" {
		parts = append(parts, "uid="+value)
	}
	if value := strings.TrimSpace(d.Serial); value != "" {
		parts = append(parts, "serial="+value)
	}
	if value := strings.TrimSpace(d.Model); value != "" {
		parts = append(parts, "model="+value)
	}
	return strings.Join(parts, "|")
}

func (d diskInfo) HasHardwareIdentity() bool {
	return strings.TrimSpace(d.UniqueID) != "" || strings.TrimSpace(d.Serial) != ""
}

func (d diskInfo) Label() string {
	model := strings.TrimSpace(d.Model)
	if model == "" {
		model = "Disco sem identificação"
	}
	extra := ""
	if len(d.DriveLetters) > 0 {
		extra = " — " + strings.Join(d.DriveLetters, ", ")
	}
	if d.Removable {
		extra += " — Removível"
	}
	media := "HDD"
	if d.TrimEnabled || !d.SeekPenalty || d.InterfaceType == "NVMe" {
		media = "SSD"
	}
	if d.CustomPath != "" {
		return fmt.Sprintf("Imagem — %s — %s", model, humanBytes(d.Size))
	}
	return fmt.Sprintf("Disco %d — %s — %s — %s/%s%s", d.Index, model, humanBytes(d.Size), d.InterfaceType, media, extra)
}

type diskGeometry struct {
	Cylinders         int64
	MediaType         uint32
	TracksPerCylinder uint32
	SectorsPerTrack   uint32
	BytesPerSector    uint32
}

type diskGeometryEx struct {
	Geometry diskGeometry
	DiskSize int64
}

func listDisksNative() ([]diskInfo, error) {
	type indexedResult struct {
		index int
		disk  diskInfo
		err   error
	}

	// As consultas são independentes. Executá-las em paralelo evita que um
	// leitor USB com problema faça a inicialização esperar vários timeouts em série.
	results := make(chan indexedResult, maxPhysicalDrives)
	for index := 0; index < maxPhysicalDrives; index++ {
		go func(i int) {
			d, err := probePhysicalDriveWithTimeout(i, diskProbeTimeout)
			results <- indexedResult{index: i, disk: d, err: err}
		}(index)
	}

	disks := make([]diskInfo, 0, 8)
	for i := 0; i < maxPhysicalDrives; i++ {
		result := <-results
		if result.err == nil {
			disks = append(disks, result.disk)
		}
	}
	if len(disks) == 0 {
		return nil, errors.New("nenhum disco físico pôde ser consultado")
	}

	lettersByDisk := driveLettersByDisk()
	for i := range disks {
		disks[i].DriveLetters = lettersByDisk[disks[i].Index]
	}
	enrichDisksWithPowerShell(disks)
	sort.Slice(disks, func(i, j int) bool { return disks[i].Index < disks[j].Index })
	return disks, nil
}

type diskPowerShellInfo struct {
	Number            int    `json:"Number"`
	FriendlyName      string `json:"FriendlyName"`
	SerialNumber      string `json:"SerialNumber"`
	UniqueID          string `json:"UniqueId"`
	PartitionStyle    string `json:"PartitionStyle"`
	IsBoot            bool   `json:"IsBoot"`
	IsSystem          bool   `json:"IsSystem"`
	IsReadOnly        bool   `json:"IsReadOnly"`
	IsOffline         bool   `json:"IsOffline"`
	Size              int64  `json:"Size"`
	LogicalSectorSize int64  `json:"LogicalSectorSize"`
}

func enrichDisksWithPowerShell(disks []diskInfo) {
	script := `$ErrorActionPreference='Stop'; $items=@(Get-Disk -ErrorAction Stop | ForEach-Object { [pscustomobject]@{Number=[int]$_.Number;FriendlyName=[string]$_.FriendlyName;SerialNumber=[string]$_.SerialNumber;UniqueId=[string]$_.UniqueId;PartitionStyle=[string]$_.PartitionStyle;IsBoot=[bool]$_.IsBoot;IsSystem=[bool]$_.IsSystem;IsReadOnly=[bool]$_.IsReadOnly;IsOffline=[bool]$_.IsOffline;Size=[Int64]$_.Size;LogicalSectorSize=[Int64]$_.LogicalSectorSize} }); ConvertTo-Json -InputObject $items -Depth 4 -Compress`
	out, err := runPowerShell(script)
	if err != nil {
		return
	}
	text := strings.TrimSpace(strings.TrimPrefix(out, "\ufeff"))
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end < start {
		return
	}
	var metadata []diskPowerShellInfo
	if json.Unmarshal([]byte(text[start:end+1]), &metadata) != nil {
		return
	}
	byNumber := make(map[int]diskPowerShellInfo, len(metadata))
	for _, item := range metadata {
		byNumber[item.Number] = item
	}
	for i := range disks {
		item, ok := byNumber[disks[i].Index]
		if !ok || item.Size <= 0 || item.Size != disks[i].Size {
			continue
		}
		if item.LogicalSectorSize > 0 && item.LogicalSectorSize != disks[i].BytesPerSector {
			continue
		}
		if strings.TrimSpace(disks[i].Model) == "" || disks[i].Model == "Disco físico" {
			disks[i].Model = strings.TrimSpace(item.FriendlyName)
		}
		if serial := strings.TrimSpace(item.SerialNumber); serial != "" {
			disks[i].Serial = serial
		}
		disks[i].UniqueID = strings.TrimSpace(item.UniqueID)
		disks[i].PartitionStyle = strings.ToUpper(strings.TrimSpace(item.PartitionStyle))
		disks[i].IsBoot = item.IsBoot
		disks[i].IsSystem = item.IsSystem
		disks[i].IsReadOnly = item.IsReadOnly
		disks[i].IsOffline = item.IsOffline
		disks[i].MetadataVerified = true
	}
}

func probePhysicalDriveWithTimeout(index int, timeout time.Duration) (diskInfo, error) {
	type result struct {
		disk diskInfo
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		d, err := probePhysicalDrive(index)
		ch <- result{disk: d, err: err}
	}()

	select {
	case r := <-ch:
		return r.disk, r.err
	case <-time.After(timeout):
		return diskInfo{}, fmt.Errorf("tempo limite ao consultar PhysicalDrive%d", index)
	}
}

func probePhysicalDrive(index int) (diskInfo, error) {
	path := fmt.Sprintf(`\\.\PhysicalDrive%d`, index)
	h, err := openDevice(path, genericRead)
	if err != nil {
		// Alguns controladores permitem consultar metadados apenas com acesso zero.
		h, err = openDevice(path, 0)
		if err != nil {
			return diskInfo{}, err
		}
	}
	defer closeDevice(h)

	var geometry diskGeometryEx
	var returned uint32
	ok, _, callErr := procDiskDeviceIoControl.Call(
		h,
		ioctlDiskGeometryEx,
		0,
		0,
		uintptr(unsafe.Pointer(&geometry)),
		unsafe.Sizeof(geometry),
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	if ok == 0 || geometry.DiskSize <= 0 {
		return diskInfo{}, fmt.Errorf("não foi possível consultar geometria: %v", callErr)
	}

	model, serial, bus, removable := queryStorageIdentity(h)
	trim := queryStorageBoolProperty(h, 8, 8)
	seekPenalty := queryStorageBoolProperty(h, 7, 8)
	health := queryPredictHealth(h)
	sector := int64(geometry.Geometry.BytesPerSector)
	if sector < 512 || sector > 1024*1024 || sector&(sector-1) != 0 {
		sector = 512
	}

	return diskInfo{
		Index:          index,
		Model:          model,
		Serial:         serial,
		Size:           geometry.DiskSize,
		InterfaceType:  bus,
		BytesPerSector: sector,
		Removable:      removable,
		TrimEnabled:    trim,
		SeekPenalty:    seekPenalty,
		Health:         health,
	}, nil
}

func queryStorageIdentity(h uintptr) (model, serial, bus string, removable bool) {
	// STORAGE_PROPERTY_QUERY: PropertyId=0 (StorageDeviceProperty), QueryType=0.
	query := [12]byte{}
	out := make([]byte, 4096)
	var returned uint32
	ok, _, _ := procDiskDeviceIoControl.Call(
		h,
		ioctlStorageQuery,
		uintptr(unsafe.Pointer(&query[0])),
		uintptr(len(query)),
		uintptr(unsafe.Pointer(&out[0])),
		uintptr(len(out)),
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	if ok == 0 || returned < 36 {
		return "Disco físico", "", "Desconhecido", false
	}

	vendor := descriptorString(out[:returned], uint32At(out, 12))
	product := descriptorString(out[:returned], uint32At(out, 16))
	revision := descriptorString(out[:returned], uint32At(out, 20))
	model = strings.TrimSpace(strings.Join(nonEmpty(vendor, product, revision), " "))
	if model == "" {
		model = "Disco físico"
	}
	serial = descriptorString(out[:returned], uint32At(out, 24))
	removable = out[10] != 0
	bus = storageBusName(uint32At(out, 28))
	return model, serial, bus, removable
}

func queryStorageBoolProperty(h uintptr, propertyID uint32, boolOffset int) bool {
	query := [12]byte{}
	query[0] = byte(propertyID)
	out := make([]byte, 64)
	var returned uint32
	ok, _, _ := procDiskDeviceIoControl.Call(
		h, ioctlStorageQuery,
		uintptr(unsafe.Pointer(&query[0])), uintptr(len(query)),
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)),
		uintptr(unsafe.Pointer(&returned)), 0,
	)
	return ok != 0 && int(returned) > boolOffset && out[boolOffset] != 0
}

func queryPredictHealth(h uintptr) string {
	out := make([]byte, 516)
	var returned uint32
	ok, _, _ := procDiskDeviceIoControl.Call(
		h, ioctlPredictFailure, 0, 0,
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)),
		uintptr(unsafe.Pointer(&returned)), 0,
	)
	if ok == 0 || returned < 4 {
		return "SMART indisponível"
	}
	if uint32At(out, 0) != 0 {
		return "Falha prevista"
	}
	return "Sem falha prevista"
}

func storageBusName(v uint32) string {
	switch v {
	case 1:
		return "SCSI"
	case 2:
		return "ATAPI"
	case 3:
		return "ATA"
	case 7:
		return "USB"
	case 8:
		return "RAID"
	case 9:
		return "iSCSI"
	case 10:
		return "SAS"
	case 11:
		return "SATA"
	case 12:
		return "SD"
	case 13:
		return "MMC"
	case 15:
		return "Virtual"
	case 16:
		return "Arquivo"
	case 17:
		return "NVMe"
	default:
		return "Desconhecido"
	}
}

func uint32At(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

func descriptorString(b []byte, offset uint32) string {
	if offset == 0 || int(offset) >= len(b) {
		return ""
	}
	end := int(offset)
	for end < len(b) && b[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(b[int(offset):end]))
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func openDevice(path string, access uintptr) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, _, callErr := procDiskCreateFileW.Call(
		uintptr(unsafe.Pointer(p)),
		access,
		fileShareRead|fileShareWrite,
		0,
		openExisting,
		fileAttributeNormal,
		0,
	)
	if h == ^uintptr(0) {
		return 0, callErr
	}
	return h, nil
}

func closeDevice(h uintptr) {
	if h != 0 && h != ^uintptr(0) {
		procDiskCloseHandle.Call(h)
	}
}

func physicalDiskForPathNative(path string) (int, error) {
	volume := filepath.VolumeName(path)
	if len(volume) < 2 || volume[1] != ':' {
		return -1, errors.New("destino sem letra de unidade local")
	}
	device := `\\.\` + strings.ToUpper(volume[:2])
	h, err := openDevice(device, 0)
	if err != nil {
		return -1, err
	}
	defer closeDevice(h)

	// VOLUME_DISK_EXTENTS tem alinhamento de 8 bytes em processos x64.
	out := make([]byte, 1024)
	var returned uint32
	ok, _, callErr := procDiskDeviceIoControl.Call(
		h,
		ioctlVolumeExtents,
		0,
		0,
		uintptr(unsafe.Pointer(&out[0])),
		uintptr(len(out)),
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	if ok == 0 || returned < 12 {
		return -1, fmt.Errorf("não foi possível mapear volume para disco físico: %v", callErr)
	}
	count := uint32At(out, 0)
	if count == 0 {
		return -1, errors.New("volume sem extensão física")
	}
	// O primeiro DISK_EXTENT começa no offset 8; DiskNumber é seu primeiro campo.
	return int(uint32At(out, 8)), nil
}

func driveLettersByDisk() map[int][]string {
	mask, _, _ := procGetLogicalDrives.Call()
	out := make(map[int][]string)
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := fmt.Sprintf("%c:", 'A'+i)
		idx, err := physicalDiskForPathNative(letter + `\`)
		if err == nil {
			out[idx] = append(out[idx], letter)
		}
	}
	return out
}

func freeSpaceForPath(path string) (uint64, error) {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return 0, errors.New("caminho sem volume")
	}
	root := volume + `\`
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var freeAvailable, total, freeTotal uint64
	ok, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&freeTotal)),
	)
	if ok == 0 {
		return 0, callErr
	}
	return freeAvailable, nil
}

type cloneDiskAccess struct {
	Source        *os.File
	Destination   *os.File
	targetHandle  uintptr
	lockedVolumes []uintptr
}

func (a *cloneDiskAccess) Close() {
	if a == nil {
		return
	}
	if a.Destination != nil {
		_ = a.Destination.Sync()
		if a.targetHandle != 0 {
			var returned uint32
			procDiskDeviceIoControl.Call(a.targetHandle, ioctlDiskUpdateProperties, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
		}
		_ = a.Destination.Close()
		a.Destination = nil
		a.targetHandle = 0
	}
	for i := len(a.lockedVolumes) - 1; i >= 0; i-- {
		h := a.lockedVolumes[i]
		var returned uint32
		procDiskDeviceIoControl.Call(h, fsctlUnlockVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
		closeDevice(h)
	}
	a.lockedVolumes = nil
	if a.Source != nil {
		_ = a.Source.Close()
		a.Source = nil
	}
}

func diskContainsSystemVolume(d diskInfo) bool {
	system := strings.TrimSpace(os.Getenv("SystemDrive"))
	if system == "" {
		system = "C:"
	}
	system = strings.ToUpper(strings.TrimSuffix(system, `\`))
	for _, letter := range d.DriveLetters {
		if strings.ToUpper(strings.TrimSuffix(letter, `\`)) == system {
			return true
		}
	}
	return false
}

func diskContainsProtectedSystemRole(d diskInfo) bool {
	return d.IsBoot || d.IsSystem || diskContainsSystemVolume(d)
}

func revalidateCloneDisks(source, destination diskInfo) (diskInfo, diskInfo, error) {
	disks, err := listDisksNative()
	if err != nil {
		return diskInfo{}, diskInfo{}, fmt.Errorf("não foi possível revalidar os discos físicos: %w", err)
	}
	var currentSource, currentDestination diskInfo
	sourceFound := false
	destinationFound := false
	for _, disk := range disks {
		if disk.Index == source.Index {
			currentSource = disk
			sourceFound = true
		}
		if disk.Index == destination.Index {
			currentDestination = disk
			destinationFound = true
		}
	}
	if !sourceFound || !destinationFound {
		return diskInfo{}, diskInfo{}, errors.New("a origem ou o destino foi desconectado; atualize a lista e selecione novamente")
	}
	if !currentSource.MetadataVerified || !currentDestination.MetadataVerified {
		return diskInfo{}, diskInfo{}, errors.New("o Windows não confirmou os metadados de segurança dos discos; a clonagem foi bloqueada")
	}
	if source.StableIdentity() != currentSource.StableIdentity() {
		return diskInfo{}, diskInfo{}, errors.New("o disco de origem mudou desde a confirmação; atualize a lista e selecione novamente")
	}
	if destination.StableIdentity() != currentDestination.StableIdentity() {
		return diskInfo{}, diskInfo{}, errors.New("o disco de destino mudou desde a confirmação; nenhuma gravação foi iniciada")
	}
	if currentSource.Index == currentDestination.Index {
		return diskInfo{}, diskInfo{}, errors.New("origem e destino apontam para o mesmo disco físico")
	}
	if diskContainsProtectedSystemRole(currentDestination) {
		return diskInfo{}, diskInfo{}, errors.New("o destino possui função de boot/sistema do Windows em uso e nunca pode ser sobrescrito")
	}
	if currentDestination.IsReadOnly {
		return diskInfo{}, diskInfo{}, errors.New("o disco de destino está marcado como somente leitura")
	}
	return currentSource, currentDestination, nil
}

func openCloneDiskAccess(source, destination diskInfo, lockSource, allowSmaller bool, log func(string)) (*cloneDiskAccess, error) {
	if source.CustomPath != "" || destination.CustomPath != "" {
		return nil, errors.New("a clonagem direta aceita somente discos físicos")
	}
	if source.Index == destination.Index {
		return nil, errors.New("origem e destino apontam para o mesmo disco físico")
	}
	currentSource, currentDestination, err := revalidateCloneDisks(source, destination)
	if err != nil {
		return nil, err
	}
	source = currentSource
	destination = currentDestination
	if destination.Size < source.Size && !allowSmaller {
		return nil, errors.New("o disco de destino é menor que o disco de origem")
	}
	if diskContainsProtectedSystemRole(destination) {
		return nil, errors.New("o disco de destino contém uma função protegida do Windows em uso e não pode ser apagado")
	}

	access := &cloneDiskAccess{}
	cleanup := func(err error) (*cloneDiskAccess, error) {
		access.Close()
		return nil, err
	}

	// No modo offline, tentamos congelar os volumes montados da origem. No modo
	// VSS, o Windows continua em uso e a consistência vem das cópias de sombra.
	if lockSource {
		for _, letter := range source.DriveLetters {
			trimmed := strings.TrimSuffix(strings.TrimSpace(letter), `\`)
			if len(trimmed) < 2 || trimmed[1] != ':' {
				continue
			}
			volumePath := `\\.\` + trimmed[:2]
			h, err := openDeviceAdvanced(volumePath, genericRead|genericWrite, fileShareRead|fileShareWrite, fileAttributeNormal)
			if err != nil {
				return cleanup(fmt.Errorf("não foi possível abrir o volume %s da origem para bloqueio: %w", trimmed, err))
			}
			var returned uint32
			ok, _, _ := procDiskDeviceIoControl.Call(h, fsctlLockVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
			if ok == 0 {
				closeDevice(h)
				return cleanup(fmt.Errorf("não foi possível bloquear o volume %s da origem; feche programas que estejam usando o disco", trimmed))
			}
			ok, _, _ = procDiskDeviceIoControl.Call(h, fsctlDismountVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
			if ok == 0 {
				procDiskDeviceIoControl.Call(h, fsctlUnlockVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
				closeDevice(h)
				return cleanup(fmt.Errorf("não foi possível desmontar o volume %s da origem; a clonagem offline foi bloqueada", trimmed))
			}
			access.lockedVolumes = append(access.lockedVolumes, h)
			if log != nil {
				log(fmt.Sprintf("Volume %s da origem bloqueado e desmontado para uma cópia consistente.", trimmed))
			}
		}
	} else if log != nil {
		log("A origem permanecerá online; os volumes montados serão lidos por snapshots VSS.")
	}

	// Keep every mounted destination volume locked for the whole operation.
	for _, letter := range destination.DriveLetters {
		trimmed := strings.TrimSuffix(strings.TrimSpace(letter), `\`)
		if len(trimmed) < 2 || trimmed[1] != ':' {
			continue
		}
		volumePath := `\\.\` + trimmed[:2]
		h, err := openDeviceAdvanced(volumePath, genericRead|genericWrite, fileShareRead|fileShareWrite, fileAttributeNormal)
		if err != nil {
			return cleanup(fmt.Errorf("não foi possível abrir o volume %s do destino: %w", trimmed, err))
		}
		var returned uint32
		ok, _, callErr := procDiskDeviceIoControl.Call(h, fsctlLockVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
		if ok == 0 {
			closeDevice(h)
			return cleanup(fmt.Errorf("não foi possível bloquear o volume %s; feche programas e janelas que estejam usando o disco: %v", trimmed, callErr))
		}
		ok, _, callErr = procDiskDeviceIoControl.Call(h, fsctlDismountVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
		if ok == 0 {
			procDiskDeviceIoControl.Call(h, fsctlUnlockVolume, 0, 0, 0, 0, uintptr(unsafe.Pointer(&returned)), 0)
			closeDevice(h)
			return cleanup(fmt.Errorf("não foi possível desmontar o volume %s do destino: %v", trimmed, callErr))
		}
		access.lockedVolumes = append(access.lockedVolumes, h)
	}

	sourceHandle, err := openDeviceAdvanced(source.Path(), genericRead, fileShareRead|fileShareWrite, fileAttributeNormal|fileFlagSequentialScan)
	if err != nil {
		return cleanup(fmt.Errorf("não foi possível abrir a origem para leitura: %w", err))
	}
	access.Source = os.NewFile(sourceHandle, source.Path())
	if access.Source == nil {
		closeDevice(sourceHandle)
		return cleanup(errors.New("não foi possível criar o leitor do disco de origem"))
	}

	targetHandle, err := openDeviceAdvanced(destination.Path(), genericRead|genericWrite, fileShareRead|fileShareWrite, fileAttributeNormal|fileFlagWriteThrough|fileFlagSequentialScan)
	if err != nil {
		return cleanup(fmt.Errorf("não foi possível abrir o destino para gravação: %w", err))
	}
	access.targetHandle = targetHandle
	access.Destination = os.NewFile(targetHandle, destination.Path())
	if access.Destination == nil {
		closeDevice(targetHandle)
		access.targetHandle = 0
		return cleanup(errors.New("não foi possível criar o gravador do disco de destino"))
	}
	return access, nil
}

func openDeviceAdvanced(path string, access, share, flags uintptr) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, _, callErr := procDiskCreateFileW.Call(
		uintptr(unsafe.Pointer(p)), access, share, 0, openExisting, flags, 0,
	)
	if h == ^uintptr(0) {
		return 0, callErr
	}
	return h, nil
}
