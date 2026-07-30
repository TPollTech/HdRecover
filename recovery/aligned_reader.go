package recovery

import (
	"errors"
	"io"
)

// alignedReaderAt adapta leituras arbitrárias para dispositivos físicos do
// Windows, que podem exigir deslocamento e tamanho múltiplos do setor lógico.
// O arquivo de origem continua sendo acessado somente para leitura.
type alignedReaderAt struct {
	source io.ReaderAt
	size   int64
	sector int64
}

func newAlignedReaderAt(source io.ReaderAt, size, sector int64) *alignedReaderAt {
	if sector <= 0 {
		sector = 512
	}
	// Valores absurdos ou não-potência-de-dois normalmente indicam metadado
	// inválido. 512 funciona como fallback para a maioria dos dispositivos.
	if sector < 512 || sector > 1024*1024 || sector&(sector-1) != 0 {
		sector = 512
	}
	return &alignedReaderAt{source: source, size: size, sector: sector}
}

func (r *alignedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("deslocamento de leitura negativo")
	}
	if off >= r.size {
		return 0, io.EOF
	}

	wanted := int64(len(p))
	if remaining := r.size - off; remaining < wanted {
		wanted = remaining
	}

	// Caminho rápido: a solicitação já respeita o alinhamento do dispositivo.
	if off%r.sector == 0 && wanted%r.sector == 0 {
		n, err := r.source.ReadAt(p[:wanted], off)
		if int64(n) == wanted {
			return n, nil
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}

	alignedStart := alignDown(off, r.sector)
	alignedEnd := alignUp(off+wanted, r.sector)

	// Discos físicos normalmente têm tamanho múltiplo do setor. Este limite
	// evita solicitar bytes além do fim quando uma imagem de teste não tem.
	maxAlignedEnd := alignDown(r.size, r.sector)
	if maxAlignedEnd == 0 {
		maxAlignedEnd = r.size
	}
	if alignedEnd > maxAlignedEnd {
		alignedEnd = maxAlignedEnd
	}
	if alignedEnd <= alignedStart {
		return 0, io.EOF
	}

	raw := make([]byte, int(alignedEnd-alignedStart))
	n, err := r.source.ReadAt(raw, alignedStart)
	innerStart := off - alignedStart
	if int64(n) <= innerStart {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}

	available := int64(n) - innerStart
	if available > wanted {
		available = wanted
	}
	copy(p[:available], raw[innerStart:innerStart+available])

	if available == wanted {
		return int(available), nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return int(available), err
}

func alignDown(value, alignment int64) int64 {
	return value - value%alignment
}

func alignUp(value, alignment int64) int64 {
	if value%alignment == 0 {
		return value
	}
	return value + alignment - value%alignment
}
