package recovery

import (
	"bytes"
	"math/rand"
	"testing"
)

func BenchmarkScanChunk32MB(b *testing.B) {
	data := make([]byte, 32*1024*1024)
	r := rand.New(rand.NewSource(42))
	_, _ = r.Read(data)
	kinds := enabledKinds(Categories{Images: true, Documents: true, Videos: true, Audio: true, Archives: true})
	groups := buildScanGroups(kinds)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scanChunk(data, 0, kinds, groups)
	}
}

func BenchmarkScanZeroChunk32MB(b *testing.B) {
	data := make([]byte, 32*1024*1024)
	kinds := enabledKinds(Categories{Images: true, Documents: true, Videos: true, Audio: true, Archives: true})
	groups := buildScanGroups(kinds)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scanChunk(data, 0, kinds, groups)
	}
}

func benchmarkLegacyScan(data []byte, kinds []fileKind, groups []scanGroup) int {
	count := 0
	for _, group := range groups {
		searchFrom := 0
		for searchFrom < len(data) {
			relative := bytes.Index(data[searchFrom:], group.signature)
			if relative < 0 {
				break
			}
			match := searchFrom + relative
			candidateIndex := match + group.adjust
			if candidateIndex >= 0 && candidateIndex < len(data) {
				for _, kindIndex := range group.kinds {
					if kinds[kindIndex].detect(data, candidateIndex) {
						count++
					}
				}
			}
			searchFrom = match + 1
		}
	}
	return count
}

func BenchmarkLegacySignaturePasses32MB(b *testing.B) {
	data := make([]byte, 32*1024*1024)
	r := rand.New(rand.NewSource(42))
	_, _ = r.Read(data)
	kinds := enabledKinds(Categories{Images: true, Documents: true, Videos: true, Audio: true, Archives: true})
	groups := buildScanGroups(kinds)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = benchmarkLegacyScan(data, kinds, groups)
	}
}
