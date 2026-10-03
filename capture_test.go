package main

import (
	"context"
	"os/exec"
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
	st, err := startCapture(ctx, input, "none", geom.filter, geom.frameLen(), 4, nil)
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
