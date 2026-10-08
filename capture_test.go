package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestCaptureFFmpeg runs the real ffmpeg pipeline on a synthetic source and
// checks frames arrive whole and once each.
func TestCaptureFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// Odd width exercises padding; the white source checks range handling.
	geom := planGeom(318, 180, maxCaptureDim)
	input := []string{"-f", "lavfi", "-i", "color=white:size=318x180:rate=25:duration=0.4"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := startCapture(ctx, input, decoders("none", geom, false)[0], geom.frameLen(), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.stop()

	frames := 0
	for frames < 10 {
		got := st.cap.take(nil)
		for _, f := range got {
			if len(f.buf) != geom.frameLen() {
				t.Fatalf("frame is %d bytes; want %d", len(f.buf), geom.frameLen())
			}
			if y := f.buf[0]; y != 235 {
				t.Errorf("white luma = %d; want limited-range 235", y)
			}
			frames++
			st.cap.release(f.buf) // pool of 4 forces buffer reuse
		}
		if err := st.cap.Err(); err != nil && len(got) == 0 {
			t.Logf("capture ended: %v", err)
			break
		}
		if ctx.Err() != nil {
			t.Fatal("timed out waiting for frames")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if frames != 10 {
		t.Fatalf("got %d frames; want 10", frames)
	}
	select {
	case <-st.done:
	case <-ctx.Done():
		t.Fatal("capture did not finish at end of stream")
	}
	if err := st.cap.Err(); err == nil || err.Error() != "video stream ended" {
		t.Errorf("end-of-stream error = %v", err)
	}
}

func TestDecoders(t *testing.T) {
	g := planGeom(642, 361, maxCaptureDim)
	names := func(decs []decoder) []string {
		var out []string
		for _, d := range decs {
			out = append(out, d.name)
		}
		return out
	}
	for _, tt := range []struct {
		hwaccel string
		vaapi   bool
		want    []string
	}{
		{"none", true, []string{"software"}},
		{"auto", true, []string{"VA-API", "software"}},
		{"auto", false, []string{"ffmpeg auto"}},
		{"vaapi", true, []string{"VA-API", "software"}},
		{"vaapi", false, []string{"software"}},
		{"cuda", true, []string{"cuda"}},
	} {
		if got := names(decoders(tt.hwaccel, g, tt.vaapi)); !slices.Equal(got, tt.want) {
			t.Errorf("decoders(%q, vaapi %v) = %v; want %v", tt.hwaccel, tt.vaapi, got, tt.want)
		}
	}
	if got, want := g.vaapiFilter(), "scale_vaapi=w=642:h=361:format=nv12:out_range=tv,hwdownload,format=nv12,pad=644:362"; got != want {
		t.Errorf("vaapiFilter = %q; want %q", got, want)
	}
}

// grabFrame captures the first frame of input with dec.
func grabFrame(t *testing.T, input []string, dec decoder, g frameGeom) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := startCapture(ctx, input, dec, g.frameLen(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.stop()
	if err := st.waitStarted(ctx, 15*time.Second); err != nil {
		t.Fatalf("%s: %v", dec.name, err)
	}
	frames := st.cap.take(nil)
	if len(frames) == 0 {
		t.Fatalf("%s: no frame", dec.name)
	}
	return frames[0].buf
}

// TestVAAPIMatchesSoftware checks the GPU pipeline produces the same frames
// as software decoding, including padding. It needs a working VA-API
// driver. Full-range sources are not compared: scale_vaapi leaves their
// range unconverted, so run never uses VA-API for them.
func TestVAAPIMatchesSoftware(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if !vaapiAvailable(context.Background()) {
		t.Skip("VA-API not available")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	// Limited-range H.264 with an odd width, which needs padding.
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=642x362:rate=10",
		"-frames:v", "5", "-c:v", "libx264", "-pix_fmt", "yuv420p", clip).CombinedOutput()
	if err != nil {
		t.Skipf("cannot encode test clip: %v %s", err, out)
	}
	g := planGeom(642, 362, maxCaptureDim)
	input := []string{"-i", clip}
	decs := decoders("vaapi", g, true)
	gpu := grabFrame(t, input, decs[0], g)
	cpu := grabFrame(t, input, decs[1], g)
	var sum, worst int
	for i := range cpu {
		d := int(gpu[i]) - int(cpu[i])
		d = max(d, -d)
		sum += d
		worst = max(worst, d)
	}
	mean := float64(sum) / float64(len(cpu))
	t.Logf("VA-API vs software: mean abs difference %.2f, worst %d", mean, worst)
	if mean > 0.5 {
		t.Errorf("VA-API frame differs from software: mean abs difference %.2f", mean)
	}
}

// TestStartDecodingFallsBack checks that a decoder failing before the first
// frame gives way to the next: VA-API cannot decode FFV1, so ffmpeg feeds
// software frames to scale_vaapi, which rejects them.
func TestStartDecodingFallsBack(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if !vaapiAvailable(context.Background()) {
		t.Skip("VA-API not available")
	}
	clip := filepath.Join(t.TempDir(), "clip.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10",
		"-frames:v", "20", "-c:v", "ffv1", clip).CombinedOutput(); err != nil {
		t.Skipf("cannot encode test clip: %v %s", err, out)
	}
	g := planGeom(320, 180, maxCaptureDim)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, dec, err := startDecoding(ctx, []string{"-i", clip}, decoders("vaapi", g, true), g.frameLen(), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.stop()
	if dec.name != "software" {
		t.Fatalf("decoding with %s; want the software fallback", dec.name)
	}
	if st.cap.Received() == 0 {
		t.Fatal("no frames from the fallback")
	}
}

func TestReconnect(t *testing.T) {
	fake := func() *streamer {
		done := make(chan struct{})
		close(done)
		return &streamer{cap: newCapture(16, 4), cancel: func() {}, done: done}
	}
	var started []*streamer
	p := &player{
		st:       fake(),
		hasFrame: true, // no preview upload, which needs a renderer
		restart: func() (*streamer, error) {
			st := fake()
			started = append(started, st)
			return st, nil
		},
		newSched: func(release func([]byte)) *scheduler { return newScheduler(time.Second, 20, 8, release) },
	}
	p.sched = p.newSched(p.st.cap.release)
	start := time.Now()
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	arrive := func(s float64) {
		p.st.cap.push(&frame{buf: make([]byte, 16), arrival: at(s)})
		if err := p.update(at(s)); err != nil {
			t.Fatal(err)
		}
	}

	// Before the first frame, ffmpeg failing is an error.
	p.st.cap.setErr(errors.New("401 Unauthorized"))
	if err := p.checkStream(at(0)); err == nil {
		t.Fatal("failure before the first frame was not an error")
	}
	p.st = fake()
	p.sched = p.newSched(p.st.cap.release)

	arrive(0)
	if p.checkStream(at(9)); len(started) != 0 {
		t.Fatal("reconnected before stallTimeout")
	}
	// The camera stops sending without closing the connection.
	if p.checkStream(at(10)); len(started) != 1 || p.st != started[0] || p.attempts != 1 {
		t.Fatalf("stall: %d restarts, attempts %d; want a reconnection", len(started), p.attempts)
	}
	// The new connection fails at once; the next try waits 2s.
	p.st.cap.setErr(errors.New("connection refused"))
	if p.checkStream(at(11)); len(started) != 1 {
		t.Fatal("retried too soon")
	}
	if p.checkStream(at(12)); len(started) != 2 || p.attempts != 2 {
		t.Fatalf("%d restarts, attempts %d; want a second attempt", len(started), p.attempts)
	}
	// This one connects but sends nothing: given stallTimeout, then 4s.
	if p.checkStream(at(21)); len(started) != 2 {
		t.Fatal("gave up on a connection before stallTimeout")
	}
	if p.checkStream(at(22)); len(started) != 3 || p.attempts != 3 {
		t.Fatalf("%d restarts, attempts %d; want a third attempt", len(started), p.attempts)
	}
	arrive(23)
	if p.attempts != 0 || p.toast != "Reconnected" {
		t.Fatalf("attempts %d, toast %q after frames arrived; want reconnected", p.attempts, p.toast)
	}
	if p.checkStream(at(32)); len(started) != 3 {
		t.Fatal("reconnected a working stream")
	}
}
