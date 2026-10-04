package main

import (
	"time"
)

const (
	// maxDriftRate caps how much the cadence may speed up or slow down to
	// hold latency at the target delay. 5% is not visible in video.
	maxDriftRate = 0.05
	// driftGain converts latency error (seconds) into a cadence adjustment.
	driftGain = 0.5
	// slackAlpha is the EMA weight given to each new frame's slack.
	slackAlpha = 0.05
	// maxBacklog is how far behind the target delay playback may fall (e.g.
	// after a network stall delivers a burst) before skipping ahead to live.
	maxBacklog = time.Second
	// rateWindow is how many recent arrivals the frame rate is measured
	// over, and rateMinSpan how long they must span before being trusted.
	rateWindow  = 100
	rateMinSpan = time.Second
	// rateWarmup ignores arrivals right after connecting, when ffmpeg may
	// deliver a burst of frames that would skew the measured rate.
	rateWarmup = time.Second
)

// frame is one decoded NV12 picture plus its timing.
type frame struct {
	buf     []byte
	arrival time.Time
	due     time.Time
}

// scheduler is a jitter buffer. Frames are shown in arrival order at a
// steady cadence: the camera's frame rate as measured from arrivals. Stream
// timestamps are not used because cameras get them wrong; one stamped two
// frames per 100ms tick and jumped back ~300ms every second. Each frame is
// due one interval after the previous one, with the interval nudged by up
// to maxDriftRate so frames keep arriving about delay before they are due.
type scheduler struct {
	delay      time.Duration
	maxPending int
	release    func([]byte)

	pending  []*frame
	started  bool
	firstArr time.Time
	lastDue  time.Time
	slack    float64 // EMA of due-minus-arrival, in seconds
	interval time.Duration
	arrivals []time.Time // recent arrivals, for measuring the frame rate

	shown, late, dropped, reanchors uint64
}

func newScheduler(delay time.Duration, fps float64, maxPending int, release func([]byte)) *scheduler {
	return &scheduler{
		delay:      delay,
		maxPending: max(maxPending, 1),
		release:    release,
		interval:   time.Duration(float64(time.Second) / fps),
	}
}

// noteArrival updates the measured frame interval.
func (s *scheduler) noteArrival(t time.Time) {
	if t.Sub(s.firstArr) < rateWarmup {
		return
	}
	s.arrivals = append(s.arrivals, t)
	if len(s.arrivals) > rateWindow {
		s.arrivals = s.arrivals[len(s.arrivals)-rateWindow:]
	}
	if span := t.Sub(s.arrivals[0]); span >= rateMinSpan {
		s.interval = span / time.Duration(len(s.arrivals)-1)
	}
}

// restart schedules f delay after its arrival and continues from there.
func (s *scheduler) restart(f *frame) {
	f.due = f.arrival.Add(s.delay)
	s.lastDue = f.due
	s.slack = s.delay.Seconds()
}

// add queues a newly received frame.
func (s *scheduler) add(f *frame) {
	if !s.started {
		s.started = true
		s.firstArr = f.arrival
		s.restart(f)
		s.pending = append(s.pending, f)
		return
	}
	s.noteArrival(f.arrival)

	adj := min(maxDriftRate, max(-maxDriftRate, (s.delay.Seconds()-s.slack)*driftGain))
	due := s.lastDue.Add(time.Duration(float64(s.interval) * (1 + adj)))
	if due.Before(f.arrival) {
		due = f.arrival // arrived late: show it as soon as possible
	}
	if due.Sub(f.arrival) > s.delay+maxBacklog {
		// Far behind live, typically after a stall: skip ahead.
		s.flush()
		s.restart(f)
		s.reanchors++
	} else {
		f.due = due
		s.lastDue = due
		s.slack += (due.Sub(f.arrival).Seconds() - s.slack) * slackAlpha
	}

	if len(s.pending) >= s.maxPending {
		s.release(s.pending[0].buf)
		s.pending[0] = nil
		s.pending = s.pending[1:]
		s.dropped++
	}
	s.pending = append(s.pending, f)
}

// newest returns the most recently received frame that is still queued, or
// nil. It stays queued, and its buffer still belongs to the scheduler.
func (s *scheduler) newest() *frame {
	if len(s.pending) == 0 {
		return nil
	}
	return s.pending[len(s.pending)-1]
}

func (s *scheduler) flush() {
	for i, f := range s.pending {
		s.release(f.buf)
		s.pending[i] = nil
	}
	s.pending = s.pending[:0]
}

// next returns the newest frame that is due at now, releasing any older due
// frames it skips, or nil when nothing new is due. The caller owns the
// returned frame's buffer and must release it.
func (s *scheduler) next(now time.Time) *frame {
	var show *frame
	for len(s.pending) > 0 && !s.pending[0].due.After(now) {
		if show != nil {
			s.release(show.buf)
			s.late++
		}
		show = s.pending[0]
		s.pending[0] = nil
		s.pending = s.pending[1:]
	}
	if show != nil {
		s.shown++
	}
	return show
}
