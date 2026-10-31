package mxl

import (
	"context"
	"testing"
	"time"

	mxlmonitoring "github.com/marquies/libmxl-monitoring/go"
	"github.com/qvest-digital/go-mxl/mxl"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
)

const absentFlow = "5fbec3b1-1b0f-417d-9059-8b94a47197ed"

func snapshotOf(t *testing.T, flowID string) (mxlmonitoring.Snapshot, bool) {
	t.Helper()
	m := processMonitoring(&countingParent{})
	require.NotNil(t, m)
	all, err := m.Snapshot()
	require.NoError(t, err)
	for _, s := range all {
		if s.FlowID == flowID {
			return s, true
		}
	}
	return mxlmonitoring.Snapshot{}, false
}

func runSource(t *testing.T, src *Source, domain string) error {
	t.Helper()
	return src.Run(defs.StaticSourceRunParams{
		Context:        context.Background(),
		ResolvedSource: "mxl://" + domain + "/" + absentFlow,
		Conf:           &conf.Path{},
	})
}

// The handler re-runs a failing source every few seconds. A flow that cannot
// be opened is one ongoing condition across those retries, so its handle and
// counts have to outlive a single Run; and once the handler closes the
// source, the flow stops being reported, so a path built again for it can
// monitor it afresh.
func TestAnAbsentFlowIsCountedAcrossRetriesUntilClosed(t *testing.T) {
	domain := t.TempDir()
	src := &Source{Parent: &countingParent{}}

	require.ErrorContains(t, runSource(t, src, domain), "open MXL reader")
	require.ErrorContains(t, runSource(t, src, domain), "open MXL reader")

	s, ok := snapshotOf(t, absentFlow)
	require.True(t, ok, "absent flow is not reported")
	require.Equal(t, mxlmonitoring.AccessFlowNotFound, s.Access)
	require.Equal(t, uint64(2), s.AccessFailures[mxlmonitoring.AccessFlowNotFound])

	src.Close()
	_, ok = snapshotOf(t, absentFlow)
	require.False(t, ok, "flow of a closed source is still reported")

	again := &Source{Parent: &countingParent{}}
	require.ErrorContains(t, runSource(t, again, domain), "open MXL reader")
	s, ok = snapshotOf(t, absentFlow)
	require.True(t, ok, "flow of a path built again is not reported")
	require.Equal(t, uint64(1), s.AccessFailures[mxlmonitoring.AccessFlowNotFound])
	again.Close()
}

// A flow is reported under its id, so a second path on it cannot have a
// handle of its own. That has to leave the path reading, not failing.
func TestASecondPathOnOneFlowStillRuns(t *testing.T) {
	domain := t.TempDir()
	first := &Source{Parent: &countingParent{}}
	second := &Source{Parent: &countingParent{}}
	defer first.Close()
	defer second.Close()

	require.ErrorContains(t, runSource(t, first, domain), "open MXL reader")
	require.ErrorContains(t, runSource(t, second, domain), "open MXL reader")
}

// The runtime is polled far more often than the head moves; each head is one
// commit, not one per poll.
func TestCommitsAreObservedOncePerHead(t *testing.T) {
	const id = "1d4c3a0e-7d55-4a6e-9e43-1f2d6c7b9a01"
	m := processMonitoring(&countingParent{})
	f, err := m.AddFlow(id, mxlmonitoring.RoleReader)
	require.NoError(t, err)
	defer func() { _ = m.RemoveFlow(f) }()
	rate := mxl.Rational{Num: 50, Den: 1}
	require.NoError(t, f.SetFormat(mxl.FormatVideo, mxlmonitoring.UnitGrain, rate, ""))

	head := mxl.TimestampToIndex(rate, mxl.Now()) - 5
	c := commits{f: f}
	for range 3 {
		c.observe(mxl.FlowRuntime{HeadIndex: head, LastWriteTime: mxl.IndexToTimestamp(rate, head)})
	}
	c.observe(mxl.FlowRuntime{HeadIndex: head + 1, LastWriteTime: mxl.IndexToTimestamp(rate, head+1)})

	s, ok := snapshotOf(t, id)
	require.True(t, ok)
	require.Equal(t, uint64(2), s.CommitLatency.Count)
}

func TestPeriod(t *testing.T) {
	require.Equal(t, 20*time.Millisecond, period(mxl.Rational{Num: 50, Den: 1}, 1))
	require.Equal(t, 20*time.Millisecond, period(mxl.Rational{Num: 48000, Den: 1}, audioReadSamples))
	require.Equal(t, time.Duration(0), period(mxl.Rational{Num: 0, Den: 1}, 1))
}
