package diagnostics_test

import (
	"io"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

// The handler scrubs while holding the mutex that serialises writes, so the
// per-operation cost of the parallel benchmark is the whole scrub rather than
// a share of it. Run both to see whether that is still the case:
//
//	go test ./internal/diagnostics -run '^$' -bench Scrub
var benchmarkSizes = []struct {
	name string
	size int
}{
	{"256B", 256},
	{"1KiB", 1 << 10},
	{"16KiB", 16 << 10},
	{"100KiB", 100 << 10},
}

func benchmarkText(size int) string {
	var text strings.Builder
	for text.Len() < size {
		text.WriteString("reconcile failed for Kustomization/flux-system/apps: " +
			"HelmRelease/media/plex not ready, retrying in 30s. ")
	}
	return text.String()[:size]
}

func BenchmarkScrubSecrets(b *testing.B) {
	for _, size := range benchmarkSizes {
		text := benchmarkText(size.size)
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(size.size))
			for b.Loop() {
				_ = diagnostics.ScrubSecrets(text)
			}
		})
	}
}

func BenchmarkScrubbingLogHandlerInParallel(b *testing.B) {
	for _, size := range benchmarkSizes {
		text := benchmarkText(size.size)
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(size.size))
			logger := diagnostics.NewLeveledLogger(io.Discard, nil, diagnostics.LevelDebug, nil)
			b.RunParallel(func(p *testing.PB) {
				for p.Next() {
					logger.Infof("%s", text)
				}
			})
		})
	}
}
