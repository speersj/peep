package main

import (
	"math/rand"
	"slices"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const vsync = time.Second / 60

type schedHarness struct {
	t        *testing.T
	s        *scheduler
	now      time.Time
	seq      int
	shown    []int // sequence numbers of shown frames, in order
	shownAt  []time.Time
	released int
}

func newHarness(t *testing.T, delay time.Duration, fps float64, maxPending int) *schedHarness {
	h := &schedHarness{t: t, now: epoch}
	h.s = newScheduler(delay, fps, maxPending, func([]byte) { h.released++ })
	return h
}

// runUntil ticks once per vsync up to t, recording shown frames.
func (h *schedHarness) runUntil(t time.Time) {
	for !h.now.After(t) {
		if f := h.s.next(h.now); f != nil {
			h.shown = append(h.shown, int(f.buf[0])<<8|int(f.buf[1]))
			h.shownAt = append(h.shownAt, h.now)
			h.released++
		}
		h.now = h.now.Add(vsync)
	}
}

// arrive delivers the next frame at t, running the display loop until then.
func (h *schedHarness) arrive(t time.Time) {
	h.runUntil(t)
	h.s.add(&frame{buf: []byte{byte(h.seq >> 8), byte(h.seq)}, arrival: t})
	h.seq++
}

// checkSmooth verifies every frame from index from onwards was shown once,
// in order, with gaps that are as even as the vsync grid allows.
func (h *schedHarness) checkSmooth(from int, interval time.Duration) {
	h.t.Helper()
	start := -1
	for i, seq := range h.shown {
		if seq == from {
			start = i
			break
		}
	}
	if start < 0 {
		h.t.Fatalf("frame %d never shown", from)
	}
	for i := start; i < len(h.shown); i++ {
		if want := from + i - start; h.shown[i] != want {
			h.t.Fatalf("shown[%d] = frame %d; want %d (frames skipped or reordered)", i, h.shown[i], want)
		}
		if i > start {
			gap := h.shownAt[i].Sub(h.shownAt[i-1])
			if gap < interval-vsync-time.Millisecond || gap > interval+vsync+time.Millisecond {
				h.t.Fatalf("frame %d shown %v after the previous one; want about %v", h.shown[i], gap, interval)
			}
		}
	}
}

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// 20fps arriving with up to ±80ms of network jitter must come out evenly
// spaced, every frame once, when the buffer exceeds the jitter.
func TestSchedulerSmoothsJitter(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 20, 64)
	rng := rand.New(rand.NewSource(1))
	last := epoch
	for i := range 400 {
		arr := epoch.Add(ms(float64(i)*50 + rng.Float64()*160 - 80))
		if arr.Before(last) {
			arr = last // arrivals are ordered
		}
		last = arr
		h.arrive(arr)
	}
	h.runUntil(h.now.Add(time.Second))
	if len(h.shown) != 400 || h.s.late != 0 || h.s.dropped != 0 {
		t.Fatalf("shown %d of 400, late %d, dropped %d", len(h.shown), h.s.late, h.s.dropped)
	}
	h.checkSmooth(0, 50*time.Millisecond)
}

// ffprobe's frame rate can be wrong (the camera reported r_frame_rate=40 for
// a 20fps stream). The measured rate must take over and playback settle.
func TestSchedulerMeasuresFrameRate(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 40, 64)
	for i := range 400 {
		h.arrive(epoch.Add(ms(float64(i) * 50)))
	}
	h.runUntil(h.now.Add(time.Second))
	if got := h.s.interval; got < ms(49) || got > ms(51) {
		t.Errorf("measured interval %v; want 50ms", got)
	}
	h.checkSmooth(200, 50*time.Millisecond)
}

// Frames arriving in pairs every 100ms (the pattern the camera's timestamps
// showed) must still be shown one every 50ms.
func TestSchedulerSteadyUnderBurstyArrival(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 20, 64)
	// Pairs of frames together, every 100ms: average 20fps.
	for i := range 400 {
		h.arrive(epoch.Add(ms(float64(i/2) * 100)))
	}
	h.runUntil(h.now.Add(time.Second))
	if h.s.late != 0 {
		t.Errorf("%d frames late", h.s.late)
	}
	h.checkSmooth(100, 50*time.Millisecond)
}

// A camera whose clock runs 0.5% slow must not make latency creep up or
// frames go late: the cadence follows the measured rate.
func TestSchedulerTracksClockDrift(t *testing.T) {
	delay := 200 * time.Millisecond
	h := newHarness(t, delay, 25, 64)
	const n = 25 * 120 // two minutes
	for i := range n {
		h.arrive(epoch.Add(ms(float64(i) * 40 * 1.005)))
	}
	if h.s.late > 0 {
		t.Errorf("%d of %d frames late under 0.5%% clock drift", h.s.late, n)
	}
	if got := h.s.slack; got < delay.Seconds()-0.03 || got > delay.Seconds()+0.03 {
		t.Errorf("latency drifted to %.0fms; want about %v", got*1000, delay)
	}
}

// After a stall, a burst of old frames must not leave playback seconds
// behind live: it skips ahead once the backlog exceeds maxBacklog.
func TestSchedulerSkipsAheadAfterStall(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 20, 256)
	for i := range 100 {
		h.arrive(epoch.Add(ms(float64(i) * 50)))
	}
	// 3s stall, then the 60 frames from it arrive at once, then normal.
	burst := h.now.Add(3 * time.Second)
	for range 60 {
		h.arrive(burst)
	}
	for i := range 100 {
		h.arrive(burst.Add(ms(float64(i+1) * 50)))
	}
	if h.s.reanchors == 0 {
		t.Fatal("never skipped ahead")
	}
	if lat := h.s.lastDue.Sub(burst.Add(ms(100 * 50))); lat > 250*time.Millisecond+maxBacklog {
		t.Errorf("still %v behind after the burst", lat)
	}
}

// A frame arriving after its due time is shown right away, not dropped.
func TestSchedulerLateFrameShownOnArrival(t *testing.T) {
	h := newHarness(t, 100*time.Millisecond, 20, 64)
	h.arrive(epoch)
	h.arrive(epoch.Add(ms(400))) // due at 150ms, arrives at 400ms
	h.runUntil(epoch.Add(ms(420)))
	if len(h.shown) != 2 || h.shownAt[1].Sub(epoch) > ms(420) {
		t.Errorf("shown %v at %v", h.shown, h.shownAt)
	}
}

func TestSchedulerDropsOldestWhenFull(t *testing.T) {
	h := newHarness(t, time.Second, 20, 3)
	for range 5 {
		h.s.add(&frame{buf: []byte{0, 0}, arrival: epoch})
	}
	if len(h.s.pending) != 3 || h.s.dropped != 2 || h.released != 2 {
		t.Errorf("pending=%d dropped=%d released=%d", len(h.s.pending), h.s.dropped, h.released)
	}
}

// A wrong guess at the frame rate must be corrected soon after connecting,
// as the quick probe usually leaves only r_frame_rate to go on.
func TestSchedulerMeasuresFrameRateQuickly(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 40, 64)
	for i := range 50 { // 2.5s
		h.arrive(epoch.Add(ms(float64(i) * 50)))
	}
	if got := h.s.interval; got < ms(49) || got > ms(51) {
		t.Errorf("interval after 2.5s %v; want 50ms", got)
	}
}

// newest shows the latest frame without taking it from the queue.
func TestSchedulerNewest(t *testing.T) {
	h := newHarness(t, 250*time.Millisecond, 20, 64)
	if h.s.newest() != nil {
		t.Fatal("newest of an empty queue is not nil")
	}
	h.arrive(epoch)
	h.arrive(epoch.Add(ms(50)))
	if f := h.s.newest(); f == nil || f.buf[1] != 1 {
		t.Fatalf("newest = %v; want frame 1", f)
	}
	h.runUntil(epoch.Add(time.Second))
	if !slices.Equal(h.shown, []int{0, 1}) {
		t.Errorf("shown %v; want [0 1]", h.shown)
	}
}
