//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSmartMigrationPlanDoesNotDropGenericHiddenPartition(t *testing.T) {
	source := diskInfo{Index: 0, Size: 500 * 1024 * 1024, BytesPerSector: 512, UniqueID: "source"}
	target := diskInfo{Index: 1, Size: 400 * 1024 * 1024, BytesPerSector: 512, UniqueID: "target"}
	layout := smartStorageLayout{
		PartitionStyle: "GPT",
		IsBoot:         true,
		Partitions: []smartPartition{
			{
				PartitionNumber: 1,
				Offset:          1024 * 1024,
				Size:            450 * 1024 * 1024,
				Type:            "Basic",
				IsHidden:        true,
			},
		},
	}
	_, err := buildSmartMigrationPlan(source, target, layout)
	if err == nil {
		t.Fatal("a generic hidden partition outside the target must block the plan")
	}
}

func TestBuildSmartMigrationPlanDropsExplicitRecoveryPartition(t *testing.T) {
	source := diskInfo{Index: 0, Size: 500 * 1024 * 1024, BytesPerSector: 512, UniqueID: "source"}
	target := diskInfo{Index: 1, Size: 400 * 1024 * 1024, BytesPerSector: 512, UniqueID: "target"}
	layout := smartStorageLayout{
		PartitionStyle: "GPT",
		IsSystem:       true,
		Partitions: []smartPartition{
			{
				PartitionNumber: 1,
				Offset:          1024 * 1024,
				Size:            300 * 1024 * 1024,
				Type:            "Basic",
				GptType:         "{EBD0A0A2-B9E5-4433-87C0-68B6B72699C7}",
			},
			{
				PartitionNumber: 2,
				Offset:          440 * 1024 * 1024,
				Size:            40 * 1024 * 1024,
				Type:            "Recovery",
				GptType:         "{DE94BBA4-06D1-4D40-A16A-BFD50179D6AC}",
			},
		},
	}
	plan, err := buildSmartMigrationPlan(source, target, layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DroppedRecoveryPartitions) != 1 || plan.DroppedRecoveryPartitions[0].PartitionNumber != 2 {
		t.Fatalf("expected one explicit recovery partition to be dropped: %+v", plan)
	}
}

func TestRestoreScriptTreatsDiskIdentityAsData(t *testing.T) {
	plan := smartMigrationPlan{
		Version:               2,
		SourceDisk:            3,
		SourceUniqueID:        `uid'; Write-Host INJECTED; '`,
		SourceSerial:          `serial&INJECTED`,
		SourcePhysicalSize:    500 * 1024 * 1024,
		RequiresShrink:        true,
		ShrinkPartitionNumber: 4,
		ShrinkOriginalSize:    400 * 1024 * 1024,
		ShrinkNewSize:         300 * 1024 * 1024,
	}
	dir := t.TempDir()
	if err := writeSmartPlanFiles(plan, dir); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(dir, "RESTAURAR-PARTICAO-ORIGEM.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if strings.Contains(text, plan.SourceUniqueID) || strings.Contains(text, plan.SourceSerial) {
		t.Fatal("disk identity was interpolated into executable PowerShell")
	}
	if !strings.Contains(text, "ConvertFrom-Json") {
		t.Fatal("restore script must load identity from the plan as data")
	}
}
