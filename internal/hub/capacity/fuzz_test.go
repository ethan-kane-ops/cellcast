package capacity

import (
	"math"
	"testing"
	"time"
)

// A capacity report is arithmetic on numbers a compromised agent chooses
// (docs/threat-model.md T-07), and the result steers every placement in the
// fleet. Not on ENG-184's list of targets, added because it is the same class
// of input as the ones that are: bytes from something the hub authenticates but
// does not trust.
//
// The property is narrow and total: whatever Validate accepts, Utilisation
// returns a real number in [0,1]. A NaN would make every comparison in the
// scorer false, which hands the placement to whichever cell happened to be
// first rather than to the least loaded one, and it would do so silently.

func FuzzReportUtilisation(f *testing.F) {
	f.Add(3, int64(4000), int64(1000), int64(8<<30), int64(2<<30), 40, 110)
	f.Add(1, int64(1), int64(1), int64(1), int64(1), 0, 0)
	// Committed far beyond allocatable, which is what an oversubscribed or a
	// lying agent reports.
	f.Add(1, int64(1), int64(math.MaxInt64), int64(1), int64(math.MaxInt64), 0, 0)
	// One dimension missing entirely, which is the reporting-only-memory case
	// the divisor guard exists for.
	f.Add(1, int64(0), int64(0), int64(1<<30), int64(1<<20), 0, 0)
	f.Add(1, int64(1<<30), int64(1<<20), int64(0), int64(0), 0, 0)
	f.Add(0, int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64), 0, 0)

	f.Fuzz(func(t *testing.T,
		nodes int, cpuAlloc, cpuCommitted, memAlloc, memCommitted int64, pods, podCapacity int,
	) {
		report := Report{
			Cell:                   "prod-euw1",
			Nodes:                  nodes,
			CPUMilliAllocatable:    cpuAlloc,
			CPUMilliCommitted:      cpuCommitted,
			MemoryBytesAllocatable: memAlloc,
			MemoryBytesCommitted:   memCommitted,
			Pods:                   pods,
			PodCapacity:            podCapacity,
		}

		if err := report.Validate(); err != nil {
			// Refusing is always allowed. What is not allowed is accepting
			// something that then scores as a number the engine cannot compare.
			return
		}

		got := report.Utilisation()
		if math.IsNaN(got) {
			t.Fatalf("Validate accepted %+v and it scores NaN", report)
		}
		if math.IsInf(got, 0) {
			t.Fatalf("Validate accepted %+v and it scores %v", report, got)
		}
		if got < 0 {
			t.Fatalf("Validate accepted %+v and it scores %v, below zero", report, got)
		}
	})
}

// FuzzRegistryReport covers the ingest path around that arithmetic: a report
// that is accepted has to come back out of the index as something the scorer
// can rank.
func FuzzRegistryReport(f *testing.F) {
	f.Add("prod-euw1", int64(4000), int64(1000), int64(8<<30), int64(2<<30))
	f.Add("", int64(1), int64(1), int64(1), int64(1))
	f.Add("../../etc/passwd", int64(1), int64(1), int64(1), int64(1))
	f.Add("prod-euw1", int64(-1), int64(0), int64(1), int64(0))
	f.Add("prod-euw1", int64(0), int64(0), int64(0), int64(0))

	f.Fuzz(func(t *testing.T, cell string, cpuAlloc, cpuCommitted, memAlloc, memCommitted int64) {
		index := New(Options{Staleness: time.Hour, MaxCells: 8})

		report := Report{
			Cell:                   cell,
			Nodes:                  1,
			CPUMilliAllocatable:    cpuAlloc,
			CPUMilliCommitted:      cpuCommitted,
			MemoryBytesAllocatable: memAlloc,
			MemoryBytesCommitted:   memCommitted,
		}

		if err := index.Report(report); err != nil {
			// A refused report must leave nothing behind. An index entry for a
			// cell whose report was rejected would be scored on whatever the
			// partial write left there.
			if snapshot := index.Snapshot(); len(snapshot) != 0 {
				t.Fatalf("a refused report left %d entries in the index", len(snapshot))
			}
			return
		}

		snapshot := index.Snapshot()
		entry, ok := snapshot[cell]
		if !ok {
			t.Fatalf("Report accepted cell %q and Snapshot does not hold it", cell)
		}
		if math.IsNaN(entry.Utilisation) || math.IsInf(entry.Utilisation, 0) {
			t.Fatalf("cell %q is in the index scoring %v", cell, entry.Utilisation)
		}
		if entry.ObservedAt.IsZero() {
			t.Fatalf("cell %q was stored with no observation time, so it can never go stale", cell)
		}
	})
}
