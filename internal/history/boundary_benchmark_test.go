package history

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func BenchmarkBoundaryMatch(b *testing.B) {
	for _, width := range []int{62, 150} {
		b.Run(fmt.Sprintf("columns%d", width), func(b *testing.B) {
			var archive []string
			for i := range 60 {
				archive = append(archive, fmt.Sprintf("row%d lanterns guide quiet ships beside distant rocky shores every winter", i))
			}
			text := strings.Join(archive, " ")
			var incoming []string
			line := ""
			for _, word := range strings.Fields(text) {
				if len(line)+len(word)+1 > width {
					incoming = append(incoming, line)
					line = ""
				}
				if line != "" {
					line += " "
				}
				line += word
			}
			incoming = append(incoming, line)
			b.ReportAllocs()
			b.ResetTimer()
			var result boundaryResult
			for range b.N {
				result = matchBoundarySuffix(archive, 0, incoming)
			}
			b.ReportMetric(float64(result.work.rawBytes), "raw_B/op")
			b.ReportMetric(float64(result.work.normalizedRows), "normalized_rows/op")
		})
	}
	for _, kind := range []string{"earlier-context", "discovery-raw-over"} {
		b.Run(kind, func(b *testing.B) {
			archive := slices.Repeat([]string{"unrelated substantive context stays before this suffix", "---", ""}, 200)
			if kind == "discovery-raw-over" {
				archive = []string{strings.Repeat(" ", MaxBoundaryRawBytes+1), "---", ""}
			}
			archive = append(archive, boundaryProse...)
			b.ReportAllocs()
			b.ResetTimer()
			var result boundaryResult
			for range b.N {
				result = matchBoundarySuffix(archive, 0, boundaryProse)
			}
			if result.refused != (kind == "discovery-raw-over") {
				b.Fatalf("unexpected discovery result: %+v", result)
			}
			b.ReportMetric(float64(result.work.rawBytes), "raw_B/op")
			b.ReportMetric(float64(result.work.normalizedRows), "normalized_rows/op")
		})
	}
	for _, kind := range []string{"raw-edge", "raw-over", "normalized-edge"} {
		b.Run(kind, func(b *testing.B) {
			row := strings.Repeat(" ", MaxBoundaryRawBytes)
			if kind == "raw-over" {
				row += " "
			}
			if kind == "normalized-edge" {
				row = strings.Repeat("x", MaxBoundaryNormalizedBytes)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				work := boundaryWork{}
				_, _ = work.row(row)
			}
		})
	}
}

func BenchmarkBoundaryAdmissionLock(b *testing.B) {
	m := &Manager{now: func() time.Time { return time.Unix(10000, 0) }}
	ctx := physicalContext(1, 150)
	body := append([]string{"prior"}, boundaryProse...)
	raw := snapshotPage(boundaryProse...)
	b.ReportAllocs()
	for range b.N {
		state := &PaneState{History: slices.Clone(body), ReadSource: SourceRecent, Columns: 150, SnapshotVersion: 1, TerminalID: ctx.TerminalID}
		m.states = map[string]*PaneState{"p": state}
		m.lastSave = make(map[string]time.Time)
		next := ctx
		next.Columns = 62
		m.AdmitSnapshot("p", raw, next, 0)
	}
}
