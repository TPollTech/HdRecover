package recovery

import "time"

// OperationMode selects the recovery engine.
type OperationMode int

const (
	ModeDeepCarving OperationMode = iota
	ModeQuickNTFS
	ModeCreateImage
)

func (m OperationMode) String() string {
	switch m {
	case ModeQuickNTFS:
		return "Verificação rápida NTFS"
	case ModeCreateImage:
		return "Criar imagem/clone"
	default:
		return "Verificação profunda"
	}
}

// DamageMode controls how aggressively bad areas are retried and split.
type DamageMode int

const (
	DamageFast DamageMode = iota
	DamageBalanced
	DamageCareful
)

func (m DamageMode) String() string {
	switch m {
	case DamageCareful:
		return "Disco danificado"
	case DamageBalanced:
		return "Equilibrado"
	default:
		return "Rápido"
	}
}

// RecoveryProfile groups safe defaults for common jobs.
type RecoveryProfile int

const (
	ProfileComplete RecoveryProfile = iota
	ProfilePhotos
	ProfileDocuments
	ProfileVideos
	ProfileFormattedDrive
	ProfileDamagedDrive
)

func (p RecoveryProfile) String() string {
	switch p {
	case ProfilePhotos:
		return "Fotos de câmera"
	case ProfileDocuments:
		return "Documentos de trabalho"
	case ProfileVideos:
		return "Vídeos"
	case ProfileFormattedDrive:
		return "Unidade formatada"
	case ProfileDamagedDrive:
		return "Disco com falhas"
	default:
		return "Recuperação completa"
	}
}

// Integrity is a best-effort structural assessment, not a guarantee.
type Integrity string

const (
	IntegrityGood       Integrity = "Íntegro"
	IntegrityLikely     Integrity = "Possivelmente íntegro"
	IntegrityPartial    Integrity = "Parcial"
	IntegrityDamaged    Integrity = "Danificado"
	IntegrityUnverified Integrity = "Não validado"
)

// RecoveredFile is persisted in JSON/CSV reports and the HTML results page.
type RecoveredFile struct {
	Path         string    `json:"path"`
	OriginalName string    `json:"original_name,omitempty"`
	OriginalPath string    `json:"original_path,omitempty"`
	Extension    string    `json:"extension"`
	Category     string    `json:"category"`
	Size         int64     `json:"size"`
	SourceOffset int64     `json:"source_offset"`
	Integrity    Integrity `json:"integrity"`
	HashSHA256   string    `json:"sha256,omitempty"`
	DuplicateOf  string    `json:"duplicate_of,omitempty"`
	Method       string    `json:"method"`
	Deleted      bool      `json:"deleted"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	ModifiedAt   time.Time `json:"modified_at,omitempty"`
	Notes        string    `json:"notes,omitempty"`
}

// SessionState is saved while long operations are running.
type SessionState struct {
	Version         int           `json:"version"`
	SessionID       string        `json:"session_id"`
	Mode            OperationMode `json:"mode"`
	SourcePath      string        `json:"source_path"`
	SourceSize      int64         `json:"source_size"`
	SectorSize      int64         `json:"sector_size"`
	RangeStart      int64         `json:"range_start"`
	RangeSize       int64         `json:"range_size"`
	Destination     string        `json:"destination"`
	OutputDir       string        `json:"output_dir"`
	Phase           string        `json:"phase"`
	ScanOffset      int64         `json:"scan_offset"`
	CandidateCount  int64         `json:"candidate_count"`
	Processed       int64         `json:"processed"`
	FilesFound      int           `json:"files_found"`
	ReadErrors      int           `json:"read_errors"`
	Completed       bool          `json:"completed"`
	LastUpdate      time.Time     `json:"last_update"`
	CandidatesPath  string        `json:"candidates_path"`
	SortedPath      string        `json:"sorted_path"`
	ImagePath       string        `json:"image_path,omitempty"`
	BadMapPath      string        `json:"bad_map_path,omitempty"`
	ConfigSignature string        `json:"config_signature"`
}
