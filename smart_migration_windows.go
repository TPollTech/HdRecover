//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	smartAlignment    int64 = 1024 * 1024
	smartTailReserve  int64 = 8 * 1024 * 1024
	smartMinFree      int64 = 2 * 1024 * 1024 * 1024
	smartResizeBuffer int64 = 256 * 1024 * 1024
)

type smartStorageLayout struct {
	PartitionStyle string           `json:"PartitionStyle"`
	IsBoot         bool             `json:"IsBoot"`
	IsSystem       bool             `json:"IsSystem"`
	Partitions     []smartPartition `json:"Partitions"`
}

type smartPartition struct {
	PartitionNumber      int    `json:"PartitionNumber"`
	DriveLetter          string `json:"DriveLetter"`
	Offset               int64  `json:"Offset"`
	Size                 int64  `json:"Size"`
	Type                 string `json:"Type"`
	GptType              string `json:"GptType"`
	MbrTypeCode          int    `json:"MbrTypeCode"`
	IsHidden             bool   `json:"IsHidden"`
	NoDefaultDriveLetter bool   `json:"NoDefaultDriveLetter"`
	FileSystem           string `json:"FileSystem"`
	FileSystemLabel      string `json:"FileSystemLabel"`
	VolumeSize           int64  `json:"VolumeSize"`
	SizeRemaining        int64  `json:"SizeRemaining"`
	SizeMin              int64  `json:"SizeMin"`
	SizeMax              int64  `json:"SizeMax"`
}

func (p smartPartition) End() int64 { return p.Offset + p.Size }

func (p smartPartition) IsRecovery() bool {
	gpt := strings.ToLower(strings.TrimSpace(strings.Trim(p.GptType, "{}")))
	if gpt == "de94bba4-06d1-4d40-a16a-bfd50179d6ac" {
		return true
	}
	if p.MbrTypeCode == 0x12 || p.MbrTypeCode == 0x27 || p.MbrTypeCode == 0xDE {
		return true
	}
	t := strings.ToLower(p.Type + " " + p.FileSystemLabel)
	if strings.Contains(t, "recovery") || strings.Contains(t, "recupera") || strings.Contains(t, "oem") {
		return true
	}
	return false
}

func (p smartPartition) UsedBytes() int64 {
	if p.VolumeSize > 0 && p.SizeRemaining >= 0 && p.SizeRemaining <= p.VolumeSize {
		return p.VolumeSize - p.SizeRemaining
	}
	return 0
}

type smartMigrationPlan struct {
	Version                   int                `json:"version"`
	CreatedAt                 time.Time          `json:"created_at"`
	SourceDisk                int                `json:"source_disk"`
	TargetDisk                int                `json:"target_disk"`
	SourceUniqueID            string             `json:"source_unique_id,omitempty"`
	SourceSerial              string             `json:"source_serial,omitempty"`
	TargetUniqueID            string             `json:"target_unique_id,omitempty"`
	TargetSerial              string             `json:"target_serial,omitempty"`
	SourcePhysicalSize        int64              `json:"source_physical_size"`
	TargetPhysicalSize        int64              `json:"target_physical_size"`
	CopySize                  int64              `json:"copy_size"`
	PartitionStyle            string             `json:"partition_style"`
	TargetPartitionLimit      int64              `json:"target_partition_limit"`
	RequiresShrink            bool               `json:"requires_shrink"`
	ShrinkPartitionNumber     int                `json:"shrink_partition_number,omitempty"`
	ShrinkDriveLetter         string             `json:"shrink_drive_letter,omitempty"`
	ShrinkOriginalSize        int64              `json:"shrink_original_size,omitempty"`
	ShrinkNewSize             int64              `json:"shrink_new_size,omitempty"`
	ShrinkReduction           int64              `json:"shrink_reduction,omitempty"`
	ShrinkMinimumSafe         int64              `json:"shrink_minimum_safe,omitempty"`
	DroppedRecoveryPartitions []smartPartition   `json:"dropped_recovery_partitions,omitempty"`
	Layout                    smartStorageLayout `json:"layout"`
}

func (p smartMigrationPlan) Summary() string {
	if p.TargetPhysicalSize >= p.SourcePhysicalSize {
		return "O destino comporta a cópia física completa; nenhuma redução temporária será necessária."
	}
	parts := []string{fmt.Sprintf("Serão copiados %s para um SSD de %s.", humanBytes(p.CopySize), humanBytes(p.TargetPhysicalSize))}
	if p.RequiresShrink {
		parts = append(parts, fmt.Sprintf("A partição %s (nº %d) será reduzida temporariamente em %s e restaurada ao tamanho original ao final.", normalizeDriveLetter(p.ShrinkDriveLetter), p.ShrinkPartitionNumber, humanBytes(p.ShrinkReduction)))
	}
	if len(p.DroppedRecoveryPartitions) > 0 {
		parts = append(parts, fmt.Sprintf("%d partição(ões) de recuperação localizada(s) além do limite do SSD serão omitidas no destino; isso não impede o boot do Windows, mas o WinRE pode precisar ser reativado depois.", len(p.DroppedRecoveryPartitions)))
	}
	return strings.Join(parts, "\n")
}

func analyzeSmartMigration(source, target diskInfo) (smartMigrationPlan, error) {
	if err := ensureNoBitLockerVolumes(source); err != nil {
		return smartMigrationPlan{}, err
	}
	layout, err := querySmartStorageLayout(source.Index)
	if err != nil {
		return smartMigrationPlan{}, err
	}
	return buildSmartMigrationPlan(source, target, layout)
}

func buildSmartMigrationPlan(source, target diskInfo, layout smartStorageLayout) (smartMigrationPlan, error) {
	plan := smartMigrationPlan{
		Version: 2, CreatedAt: time.Now(), SourceDisk: source.Index, TargetDisk: target.Index,
		SourceUniqueID: source.UniqueID, SourceSerial: source.Serial,
		TargetUniqueID: target.UniqueID, TargetSerial: target.Serial,
		SourcePhysicalSize: source.Size, TargetPhysicalSize: target.Size, CopySize: alignDownSmart(target.Size, source.BytesPerSector),
	}
	if source.Index == target.Index {
		return plan, errors.New("origem e destino apontam para o mesmo disco")
	}
	if !source.HasHardwareIdentity() || !target.HasHardwareIdentity() {
		return plan, errors.New("a migração exige serial ou identificador físico confirmado na origem e no destino")
	}
	if source.BytesPerSector != target.BytesPerSector {
		return plan, fmt.Errorf("os discos usam setores lógicos diferentes (%d e %d bytes)", source.BytesPerSector, target.BytesPerSector)
	}
	if target.Size <= 0 || plan.CopySize <= 0 {
		return plan, errors.New("capacidade do SSD de destino inválida")
	}
	plan.Layout = layout
	if !layout.IsBoot && !layout.IsSystem {
		return plan, errors.New("a origem não foi confirmada como disco de boot/sistema pelo Windows")
	}
	plan.PartitionStyle = strings.ToUpper(strings.TrimSpace(layout.PartitionStyle))
	if plan.PartitionStyle != "GPT" && plan.PartitionStyle != "MBR" {
		return plan, fmt.Errorf("o disco de origem usa estilo de partição %q; a migração inteligente aceita somente discos básicos GPT ou MBR", layout.PartitionStyle)
	}
	if len(layout.Partitions) == 0 {
		return plan, errors.New("nenhuma partição foi encontrada no disco de origem")
	}
	if plan.PartitionStyle == "MBR" {
		for _, part := range layout.Partitions {
			if part.PartitionNumber > 4 {
				return plan, errors.New("o disco MBR usa partições lógicas/estendidas; a migração inteligente desta versão não altera esse tipo de layout com segurança")
			}
		}
	}
	if target.Size >= source.Size {
		plan.CopySize = source.Size
		plan.TargetPartitionLimit = source.Size
		return plan, nil
	}

	reserve := smartTailReserve
	if source.BytesPerSector*64 > reserve {
		reserve = source.BytesPerSector * 64
	}
	plan.TargetPartitionLimit = alignDownSmart(target.Size-reserve, smartAlignment)
	if plan.TargetPartitionLimit <= smartAlignment {
		return plan, errors.New("o SSD de destino é pequeno demais")
	}

	parts := append([]smartPartition(nil), layout.Partitions...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].Offset < parts[j].Offset })
	essential := make([]smartPartition, 0, len(parts))
	for _, part := range parts {
		if part.Size <= 0 || part.Offset < 0 {
			continue
		}
		if part.End() > plan.TargetPartitionLimit && part.IsRecovery() {
			plan.DroppedRecoveryPartitions = append(plan.DroppedRecoveryPartitions, part)
			continue
		}
		essential = append(essential, part)
	}
	if len(essential) == 0 {
		return plan, errors.New("não foi possível identificar as partições essenciais do Windows")
	}
	last := essential[0]
	for _, part := range essential[1:] {
		if part.End() > last.End() {
			last = part
		}
	}
	if last.End() <= plan.TargetPartitionLimit {
		return plan, nil
	}
	if !strings.EqualFold(strings.TrimSpace(last.FileSystem), "NTFS") || normalizeDriveLetter(last.DriveLetter) == "" {
		return plan, fmt.Errorf("a última partição essencial que ultrapassa o SSD é a partição %d (%s), e ela não é um volume NTFS redimensionável com letra de unidade", last.PartitionNumber, strings.TrimSpace(last.Type))
	}

	newSize := alignDownSmart(plan.TargetPartitionLimit-last.Offset, smartAlignment)
	if newSize <= 0 || newSize >= last.Size {
		return plan, errors.New("não foi possível calcular uma redução válida para a última partição NTFS")
	}
	used := last.UsedBytes()
	freeReserve := smartMinFree
	if used/20 > freeReserve {
		freeReserve = used / 20
	}
	minimumSafe := last.SizeMin + smartResizeBuffer
	if used > 0 && used+freeReserve > minimumSafe {
		minimumSafe = used + freeReserve
	}
	minimumSafe = alignUpSmart(minimumSafe, smartAlignment)
	if newSize < minimumSafe {
		return plan, fmt.Errorf("os arquivos cabem em teoria, mas o Windows só permite reduzir %s com segurança. Seriam necessários %s; libere ou mova pelo menos %s na partição %s", humanBytes(last.Size-last.SizeMin), humanBytes(last.Size-newSize), humanBytes(minimumSafe-newSize), normalizeDriveLetter(last.DriveLetter))
	}
	plan.RequiresShrink = true
	plan.ShrinkPartitionNumber = last.PartitionNumber
	plan.ShrinkDriveLetter = last.DriveLetter
	plan.ShrinkOriginalSize = last.Size
	plan.ShrinkNewSize = newSize
	plan.ShrinkReduction = last.Size - newSize
	plan.ShrinkMinimumSafe = minimumSafe
	return plan, nil
}

func smartPlansEquivalent(a, b smartMigrationPlan) bool {
	if !(a.SourceDisk == b.SourceDisk &&
		a.TargetDisk == b.TargetDisk &&
		a.SourceUniqueID == b.SourceUniqueID &&
		a.SourceSerial == b.SourceSerial &&
		a.TargetUniqueID == b.TargetUniqueID &&
		a.TargetSerial == b.TargetSerial &&
		a.SourcePhysicalSize == b.SourcePhysicalSize &&
		a.TargetPhysicalSize == b.TargetPhysicalSize &&
		a.CopySize == b.CopySize &&
		a.PartitionStyle == b.PartitionStyle &&
		a.TargetPartitionLimit == b.TargetPartitionLimit &&
		a.RequiresShrink == b.RequiresShrink &&
		a.ShrinkPartitionNumber == b.ShrinkPartitionNumber &&
		a.ShrinkDriveLetter == b.ShrinkDriveLetter &&
		a.ShrinkOriginalSize == b.ShrinkOriginalSize &&
		a.ShrinkNewSize == b.ShrinkNewSize &&
		a.ShrinkMinimumSafe == b.ShrinkMinimumSafe &&
		len(a.DroppedRecoveryPartitions) == len(b.DroppedRecoveryPartitions)) {
		return false
	}
	for i := range a.DroppedRecoveryPartitions {
		left := a.DroppedRecoveryPartitions[i]
		right := b.DroppedRecoveryPartitions[i]
		if left.PartitionNumber != right.PartitionNumber ||
			left.Offset != right.Offset ||
			left.Size != right.Size ||
			!strings.EqualFold(strings.TrimSpace(left.GptType), strings.TrimSpace(right.GptType)) ||
			left.MbrTypeCode != right.MbrTypeCode {
			return false
		}
	}
	return true
}

func querySmartStorageLayout(diskNumber int) (smartStorageLayout, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; $d=Get-Disk -Number %d -ErrorAction Stop; $items=@(Get-Partition -DiskNumber %d -ErrorAction Stop | Sort-Object Offset | ForEach-Object { $p=$_; $v=$null; try {$v=$p | Get-Volume -ErrorAction Stop} catch {}; $mn=[UInt64]$p.Size; $mx=[UInt64]$p.Size; $mbrCode=0; try {$mbrCode=[int]$p.MbrType} catch {}; if($p.DriveLetter -and $v -and ([string]$v.FileSystem -eq 'NTFS')) { try {$ss=Get-PartitionSupportedSize -DiskNumber %d -PartitionNumber $p.PartitionNumber -ErrorAction Stop; $mn=[UInt64]$ss.SizeMin; $mx=[UInt64]$ss.SizeMax} catch {} }; [pscustomobject]@{PartitionNumber=[int]$p.PartitionNumber;DriveLetter=$(if($p.DriveLetter){[string]$p.DriveLetter}else{''});Offset=[Int64]$p.Offset;Size=[Int64]$p.Size;Type=[string]$p.Type;GptType=[string]$p.GptType;MbrTypeCode=$mbrCode;IsHidden=[bool]$p.IsHidden;NoDefaultDriveLetter=[bool]$p.NoDefaultDriveLetter;FileSystem=$(if($v){[string]$v.FileSystem}else{''});FileSystemLabel=$(if($v){[string]$v.FileSystemLabel}else{''});VolumeSize=$(if($v){[Int64]$v.Size}else{0});SizeRemaining=$(if($v){[Int64]$v.SizeRemaining}else{0});SizeMin=[Int64]$mn;SizeMax=[Int64]$mx} }); [pscustomobject]@{PartitionStyle=[string]$d.PartitionStyle;IsBoot=[bool]$d.IsBoot;IsSystem=[bool]$d.IsSystem;Partitions=$items} | ConvertTo-Json -Depth 6 -Compress`, diskNumber, diskNumber, diskNumber)
	out, err := runPowerShell(script)
	if err != nil {
		return smartStorageLayout{}, fmt.Errorf("não foi possível analisar as partições da origem: %s", compactCommandOutput(out, err))
	}
	jsonText := extractJSONObject(out)
	if jsonText == "" {
		return smartStorageLayout{}, errors.New("o Windows não retornou o mapa de partições esperado")
	}
	var layout smartStorageLayout
	if err := json.Unmarshal([]byte(jsonText), &layout); err != nil {
		return layout, fmt.Errorf("resposta inválida do serviço de armazenamento do Windows: %w", err)
	}
	return layout, nil
}

func runPowerShell(script string) (string, error) {
	return runHiddenCommand("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
}

func extractJSONObject(text string) string {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return ""
	}
	return text[start : end+1]
}

func applySmartShrink(plan smartMigrationPlan, reportDir string, log func(string)) error {
	if !plan.RequiresShrink {
		return nil
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return err
	}
	if err := writeSmartPlanFiles(plan, reportDir); err != nil {
		return err
	}
	if err := validateSmartPlanSource(plan); err != nil {
		return err
	}
	if log != nil {
		log(fmt.Sprintf("Reduzindo temporariamente a partição %s de %s para %s. Os arquivos não serão apagados.", normalizeDriveLetter(plan.ShrinkDriveLetter), humanBytes(plan.ShrinkOriginalSize), humanBytes(plan.ShrinkNewSize)))
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; Resize-Partition -DiskNumber %d -PartitionNumber %d -Size ([UInt64]%d) -ErrorAction Stop; Update-HostStorageCache`, plan.SourceDisk, plan.ShrinkPartitionNumber, plan.ShrinkNewSize)
	out, err := runPowerShell(script)
	_ = os.WriteFile(filepath.Join(reportDir, "REDUCAO_ORIGEM.log"), []byte(out), 0o644)
	if err != nil {
		return fmt.Errorf("o Windows não conseguiu reduzir a partição %s: %s", normalizeDriveLetter(plan.ShrinkDriveLetter), compactCommandOutput(out, err))
	}
	actual, err := queryPartitionSize(plan.SourceDisk, plan.ShrinkPartitionNumber)
	if err != nil {
		return err
	}
	if actual > plan.ShrinkNewSize+smartAlignment || actual < plan.ShrinkNewSize-smartAlignment {
		return fmt.Errorf("a partição foi redimensionada para %s, diferente do tamanho planejado %s", humanBytes(actual), humanBytes(plan.ShrinkNewSize))
	}
	if log != nil {
		log("Redução temporária concluída. A origem será restaurada automaticamente depois da clonagem.")
	}
	return nil
}

func restoreSmartShrink(plan smartMigrationPlan, reportDir string, log func(string)) error {
	if !plan.RequiresShrink {
		return nil
	}
	if err := validateSmartPlanSource(plan); err != nil {
		return fmt.Errorf("a identidade do disco de origem não confere; restauração automática bloqueada: %w", err)
	}
	if log != nil {
		log(fmt.Sprintf("Restaurando a partição %s ao tamanho original de %s...", normalizeDriveLetter(plan.ShrinkDriveLetter), humanBytes(plan.ShrinkOriginalSize)))
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; Resize-Partition -DiskNumber %d -PartitionNumber %d -Size ([UInt64]%d) -ErrorAction Stop; Update-HostStorageCache`, plan.SourceDisk, plan.ShrinkPartitionNumber, plan.ShrinkOriginalSize)
	out, err := runPowerShell(script)
	_ = os.WriteFile(filepath.Join(reportDir, "RESTAURACAO_ORIGEM.log"), []byte(out), 0o644)
	if err != nil {
		return fmt.Errorf("a clonagem terminou, mas a partição original não pôde ser expandida automaticamente: %s. Execute RESTAURAR-PARTICAO-ORIGEM.cmd como administrador", compactCommandOutput(out, err))
	}
	if log != nil {
		log("Partição da origem restaurada ao tamanho anterior.")
	}
	return nil
}

func queryPartitionSize(diskNumber, partitionNumber int) (int64, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; [string](Get-Partition -DiskNumber %d -PartitionNumber %d -ErrorAction Stop).Size`, diskNumber, partitionNumber)
	out, err := runPowerShell(script)
	if err != nil {
		return 0, fmt.Errorf("não foi possível confirmar o novo tamanho da partição: %s", compactCommandOutput(out, err))
	}
	value := strings.TrimSpace(strings.TrimPrefix(out, "\ufeff"))
	fields := strings.Fields(value)
	for i := len(fields) - 1; i >= 0; i-- {
		if size, parseErr := strconv.ParseInt(strings.TrimSpace(fields[i]), 10, 64); parseErr == nil {
			return size, nil
		}
	}
	return 0, fmt.Errorf("o Windows retornou um tamanho de partição inválido: %q", value)
}

func writeSmartPlanFiles(plan smartMigrationPlan, reportDir string) error {
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	planPath := filepath.Join(reportDir, "PLANO_MIGRACAO_INTELIGENTE.json")
	if err := writeAndSyncFile(planPath, data, 0o600); err != nil {
		return err
	}
	ps1 := `param(
    [Parameter(Mandatory = $true)]
    [string]$PlanPath
)

$ErrorActionPreference = 'Stop'
$plan = Get-Content -LiteralPath $PlanPath -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
if ([int]$plan.version -ne 2) { throw 'Versao de plano nao suportada.' }
$disk = Get-Disk -Number ([int]$plan.source_disk) -ErrorAction Stop
if ([Int64]$disk.Size -ne [Int64]$plan.source_physical_size) { throw 'Tamanho do disco divergente.' }
$expectedUid = ([string]$plan.source_unique_id).Trim()
$expectedSerial = ([string]$plan.source_serial).Trim()
if ($expectedUid -eq '' -and $expectedSerial -eq '') { throw 'Plano sem identidade fisica da origem.' }
if ($expectedUid -ne '' -and ([string]$disk.UniqueId).Trim() -ne $expectedUid) { throw 'UniqueId divergente.' }
if ($expectedSerial -ne '' -and ([string]$disk.SerialNumber).Trim() -ne $expectedSerial) { throw 'Serial divergente.' }
$partition = Get-Partition -DiskNumber ([int]$plan.source_disk) -PartitionNumber ([int]$plan.shrink_partition_number) -ErrorAction Stop
$difference = [Math]::Abs([Int64]$partition.Size - [Int64]$plan.shrink_new_size)
if ($difference -gt 1048576) { throw 'A particao nao esta no tamanho temporario previsto.' }
Resize-Partition -DiskNumber ([int]$plan.source_disk) -PartitionNumber ([int]$plan.shrink_partition_number) -Size ([UInt64]$plan.shrink_original_size) -ErrorAction Stop
Update-HostStorageCache
Write-Host 'Particao restaurada com sucesso.'
`
	if err := writeAndSyncFile(filepath.Join(reportDir, "RESTAURAR-PARTICAO-ORIGEM.ps1"), []byte(ps1), 0o600); err != nil {
		return err
	}
	cmd := "@echo off\r\nchcp 65001 >nul\r\nnet session >nul 2>&1 || (echo Execute este arquivo como administrador. & pause & exit /b 1)\r\npowershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File \"%~dp0RESTAURAR-PARTICAO-ORIGEM.ps1\" -PlanPath \"%~dp0PLANO_MIGRACAO_INTELIGENTE.json\"\r\nif errorlevel 1 (echo Falha ao restaurar: a identidade do disco, o estado da particao ou o redimensionamento nao conferiu. & pause & exit /b 1)\r\npause\r\n"
	return writeAndSyncFile(filepath.Join(reportDir, "RESTAURAR-PARTICAO-ORIGEM.cmd"), []byte(cmd), 0o600)
}

func writeAndSyncFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validateSmartPlanSource(plan smartMigrationPlan) error {
	out, err := runPowerShell(`$ErrorActionPreference='Stop'; ` + smartPlanSourceCheckScript(plan) + `'OK'`)
	if err != nil || !strings.Contains(out, "OK") {
		return fmt.Errorf("o disco físico %d não corresponde mais ao plano salvo: %s", plan.SourceDisk, compactCommandOutput(out, err))
	}
	return nil
}

func smartPlanSourceCheckScript(plan smartMigrationPlan) string {
	uid := strings.ReplaceAll(strings.TrimSpace(plan.SourceUniqueID), "'", "''")
	serial := strings.ReplaceAll(strings.TrimSpace(plan.SourceSerial), "'", "''")
	return fmt.Sprintf(`$d=Get-Disk -Number %d -ErrorAction Stop; if([Int64]$d.Size -ne [Int64]%d){throw 'Tamanho do disco divergente'}; if('%s' -ne '' -and ([string]$d.UniqueId).Trim() -ne '%s'){throw 'UniqueId divergente'}; if('%s' -ne '' -and ([string]$d.SerialNumber).Trim() -ne '%s'){throw 'Serial divergente'}; `, plan.SourceDisk, plan.SourcePhysicalSize, uid, uid, serial, serial)
}

func alignDownSmart(value, alignment int64) int64 {
	if alignment <= 0 {
		return value
	}
	return value - value%alignment
}

func alignUpSmart(value, alignment int64) int64 {
	if alignment <= 0 || value%alignment == 0 {
		return value
	}
	return value + alignment - value%alignment
}
