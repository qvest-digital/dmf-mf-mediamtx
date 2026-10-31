package mxl

import (
	"os"
	"sync"
	"time"

	mxlmonitoring "github.com/marquies/libmxl-monitoring/go"
	"github.com/marquies/libmxl-monitoring/go/otlp"
	"github.com/qvest-digital/go-mxl/mxl"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// pollInterval keeps events within a quarter second of what caused them; the
// library needs a poll more often than once a second.
const pollInterval = 250 * time.Millisecond

var (
	monitoringOnce sync.Once
	monitoring     *mxlmonitoring.Monitoring
)

// processMonitoring is the one libmxl-monitoring context of the process,
// shared by every MXL path, polled for its events, and exported over OTLP
// when the environment names a collector. It is never closed: paths come and
// go, the process's flows outlive any one of them. nil when it could not be
// created, which leaves the paths playing unmonitored.
func processMonitoring(l logger.Writer) *mxlmonitoring.Monitoring {
	monitoringOnce.Do(func() {
		var err error
		if monitoring, err = mxlmonitoring.New(); err != nil {
			l.Log(logger.Error, "MXL paths are not monitored: %v", err)
			monitoring = nil
			return
		}
		// Without an endpoint the exporter would push to localhost:4318 and
		// fail at every interval on a pod that runs no collector.
		if otlpConfigured() {
			if _, err := otlp.Start(monitoring, "mediamtx"); err != nil {
				l.Log(logger.Warn, "MXL monitoring is not exported over OTLP: %v", err)
			}
		}
		go func() {
			for range time.Tick(pollInterval) {
				_ = monitoring.Poll()
			}
		}()
	})
	return monitoring
}

func otlpConfigured() bool {
	for _, v := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

// flowHandles keeps one monitoring handle per flow a Source reads, across the
// retries of Run, so a flow that cannot be opened or keeps failing is counted
// and notified as one ongoing condition rather than started afresh by every
// retry. A flow is reported under its id, so two paths reading one flow
// cannot both have a handle: the second reads it unmonitored.
type flowHandles struct {
	m     *mxlmonitoring.Monitoring
	flows map[string]*mxlmonitoring.Flow
}

// get returns the handle of flowID, adding it on first use, or nil when the
// flow cannot be monitored. Called only from the goroutine running Run,
// before any track starts.
func (h *flowHandles) get(l logger.Writer, flowID string) *mxlmonitoring.Flow {
	if h.m == nil {
		return nil
	}
	if f, ok := h.flows[flowID]; ok {
		return f
	}
	f, err := h.m.AddFlow(flowID, mxlmonitoring.RoleReader)
	if err != nil {
		l.Log(logger.Warn, "flow %s is read unmonitored: %v", flowID, err)
		return nil
	}
	if h.flows == nil {
		h.flows = map[string]*mxlmonitoring.Flow{}
	}
	h.flows[flowID] = f
	return f
}

// release removes every handle: what was failing ends without a recovery
// event, and the flows stop being reported.
func (h *flowHandles) release() {
	for id, f := range h.flows {
		_ = h.m.RemoveFlow(f)
		delete(h.flows, id)
	}
}

// recordOpen records the flow's format and timing once a reader is open,
// timed as the library's own reader wrappers time it from span, the media
// one read covers: a read is too early once it is span plus 50 ms past its
// timestamp, and four spans without data, at least 100 ms, are a stall. The
// stall has to come before the source's own watchdog, which ends the reader
// before a later read could count it.
func recordOpen(f *mxlmonitoring.Flow, info mxl.FlowInfo, mediaType string, span time.Duration) {
	common := info.Config.Common
	unit := mxlmonitoring.UnitGrain
	if common.Format == mxl.FormatAudio {
		unit = mxlmonitoring.UnitSample
	}
	_ = f.SetFormat(common.Format, unit, common.GrainRate, mediaType)
	_ = f.SetTiming(span+50*time.Millisecond, max(4*span, 100*time.Millisecond))
}

// commits records when each head was committed, for the commit latency of
// the flow. The runtime is read far more often than the head moves, and
// every head is one commit.
type commits struct {
	f    *mxlmonitoring.Flow
	head uint64
}

func (c *commits) observe(rt mxl.FlowRuntime) {
	if rt.LastWriteTime != 0 && rt.HeadIndex != c.head {
		c.head = rt.HeadIndex
		c.f.RecordCommit(rt.HeadIndex, rt.LastWriteTime)
	}
}

// period is the span of n grains or samples at rate.
func period(rate mxl.Rational, n uint64) time.Duration {
	if rate.Num <= 0 || rate.Den <= 0 {
		return 0
	}
	return time.Duration(n * uint64(rate.Den) * uint64(time.Second) / uint64(rate.Num))
}
