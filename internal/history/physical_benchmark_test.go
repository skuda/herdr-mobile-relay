package history

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

type physicalBenchCase struct {
	name      string
	old, next []string
}

func physicalBenchCases() []physicalBenchCase {
	unique := func(n int) []string {
		rows := make([]string, n)
		for i := range rows {
			rows[i] = fmt.Sprintf("unique-%04d", i)
		}
		return rows
	}
	cases := []physicalBenchCase{}
	for _, n := range []int{76, 350, 1000} {
		old := unique(n)
		next := slices.Clone(old)
		next[n-1] = "timer changed"
		cases = append(cases, physicalBenchCase{fmt.Sprintf("timer-%d", n), old, next})
	}
	old := unique(1000)
	cases = append(cases, physicalBenchCase{"identical-1000", old, slices.Clone(old)})
	blank := make([]string, 1000)
	blank[500] = "A"
	blank[501] = "B"
	next := slices.Clone(blank)
	next[999] = "timer"
	cases = append(cases, physicalBenchCase{"blank-heavy", blank, next})
	for _, n := range []int{255, 256, 257} {
		a := slices.Repeat([]string{"repeat"}, n)
		b := slices.Repeat([]string{"repeat"}, n+1)
		cases = append(cases, physicalBenchCase{fmt.Sprintf("ambiguous-%d", n), a, b})
	}
	scroll := unique(1000)
	fresh := make([]string, 100)
	for i := range fresh {
		fresh[i] = fmt.Sprintf("fresh-%04d", i)
	}
	cases = append(cases, physicalBenchCase{"scroll-1000", scroll, append(slices.Clone(scroll[100:]), fresh...)})
	nearOld := slices.Repeat([]string{"repeat"}, 255)
	nearOld = append(nearOld, "extra")
	nearNext := slices.Repeat([]string{"repeat"}, 257)
	nearNext = append(nearNext, "extra", "extra")
	cases = append(cases, physicalBenchCase{"pairs-65537", nearOld, nearNext})
	atOld := slices.Repeat([]string{"repeat"}, 255)
	atOld = append(atOld, "A")
	atNext := append([]string{"A"}, slices.Repeat([]string{"repeat"}, 257)...)
	cases = append(cases, physicalBenchCase{"pairs-65536", atOld, atNext})
	return cases
}

func BenchmarkPhysicalMatch(b *testing.B) {
	for _, fixture := range physicalBenchCases() {
		b.Run(fixture.name, func(b *testing.B) {
			old, next := comparisonBody(fixture.old), comparisonBody(fixture.next)
			sample := planPhysicalTransition(old, next, true)
			b.ReportMetric(float64(len(old)), "old-rows")
			b.ReportMetric(float64(len(next)), "new-rows")
			b.ReportMetric(float64(sample.consensus.pairs), "pairs")
			b.Logf("old=%d new=%d pairs=%d path=%s class=%s", len(old), len(next), sample.consensus.pairs, sample.diagnostic.MatchPath, sample.diagnostic.Class)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = planPhysicalTransition(old, next, true)
			}
		})
	}
}

func BenchmarkPhysicalMatchManager(b *testing.B) {
	for _, fixture := range physicalBenchCases() {
		b.Run(fixture.name, func(b *testing.B) {
			m := NewManager(b.TempDir())
			ctx := physicalContext(1, 54)
			first, second := snapshotPage(fixture.old...), snapshotPage(fixture.next...)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				m.AdmitSnapshot("p", first, ctx, 0)
				m.AdmitSnapshot("p", second, ctx, 0)
			}
		})
	}
}

func TestPhysicalPairBudgetBoundAndTallTimers(t *testing.T) {
	for _, fixture := range physicalBenchCases() {
		t.Run(fixture.name, func(t *testing.T) {
			result := planPhysicalTransition(comparisonBody(fixture.old), comparisonBody(fixture.next), true)
			if fixture.name == "ambiguous-257" || fixture.name == "ambiguous-256" || fixture.name == "pairs-65537" {
				if result.diagnostic.Reason != "pair_budget" || len(result.consensus.feasible) != 0 {
					t.Fatal("over-bound transition allocated pairs instead of freezing")
				}
			} else if fixture.name == "pairs-65536" {
				if result.consensus.budget || result.consensus.pairs != 65536 {
					t.Fatal("at-bound transition did not run bounded sparse consensus")
				}
			} else if fixture.name == "timer-350" || fixture.name == "timer-1000" {
				if result.diagnostic.Class == "freeze" || result.diagnostic.ReplacedRows != 1 {
					t.Fatal("tall timer did not use supported local replacement")
				}
			}
		})
	}
}

func TestPhysicalManagerFourReadsPerSecondBudget(t *testing.T) {
	for _, fixture := range physicalBenchCases() {
		t.Run(fixture.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			first, second := snapshotPage(fixture.old...), snapshotPage(fixture.next...)
			start := time.Now()
			for i := range 40 {
				raw := first
				if i%2 != 0 {
					raw = second
				}
				m.AdmitSnapshot("p", raw, ctx, 0)
			}
			elapsed := time.Since(start)
			t.Logf("40 admissions=%s mean=%s", elapsed, elapsed/40)
			if elapsed > 10*time.Second {
				t.Fatal("manager could not service four reads per second")
			}
		})
	}
}

func TestPhysicalFastAndSparseOutcomeEquivalence(t *testing.T) {
	alphabet := []string{"A", "B", "", "{", "}"}
	for seed := range 300 {
		old, next := make([]string, 7), make([]string, 7)
		value := uint64(seed + 1)
		for i := range old {
			value = value*6364136223846793005 + 1
			old[i] = alphabet[value%uint64(len(alphabet))]
			value = value*6364136223846793005 + 1
			next[i] = alphabet[value%uint64(len(alphabet))]
		}
		a, b := planPhysicalTransition(old, next, true), planPhysicalTransition(old, next, false)
		if !slices.Equal(a.outcome, b.outcome) || a.diagnostic.HeldNonblank != b.diagnostic.HeldNonblank || a.diagnostic.Reason != b.diagnostic.Reason {
			t.Fatalf("linear outcome differs old=%q next=%q", old, next)
		}
	}
}
