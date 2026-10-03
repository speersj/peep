package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// yoloxRow writes one YOLOX output row: offsets within the grid cell, log
// size in cells, objectness and a single class score.
func yoloxRow(out []float32, row int, dx, dy, logW, logH, obj float32, class int, score float32) {
	p := out[row*85 : (row+1)*85]
	p[0], p[1], p[2], p[3], p[4] = dx, dy, logW, logH, obj
	p[5+class] = score
}

func TestDecodeYOLOX(t *testing.T) {
	const size = 640
	rows := (size/8)*(size/8) + (size/16)*(size/16) + (size/32)*(size/32)
	out := make([]float32, rows*85)
	// A person in stride-8 cell (10, 5): centre (10.5, 5.5) cells = (84, 44)
	// model pixels, 8x16 model pixels in size.
	yoloxRow(out, 5*(size/8)+10, 0.5, 0.5, 0, 0.6931472, 0.9, 0, 0.9)
	// A near-duplicate that should be merged away.
	yoloxRow(out, 5*(size/8)+11, -0.4, 0.5, 0, 0.6931472, 0.8, 0, 0.8)
	// A chair, which is not reported.
	yoloxRow(out, 100, 0.5, 0.5, 1, 1, 0.99, 56, 0.99)
	// A weak dog, below minScore.
	yoloxRow(out, 200, 0.5, 0.5, 1, 1, 0.5, 16, 0.5)

	// Frame is twice the model resolution.
	dets := decodeYOLOX(out, size, 0.5, 1280, 720)
	if len(dets) != 1 {
		t.Fatalf("got %d detections %+v; want 1 person", len(dets), dets)
	}
	d := dets[0]
	want := detection{class: 0, x0: 160, y0: 72, x1: 176, y1: 104}
	if d.class != want.class || abs32(d.x0-want.x0) > 0.5 || abs32(d.y0-want.y0) > 0.5 || abs32(d.x1-want.x1) > 0.5 || abs32(d.y1-want.y1) > 0.5 {
		t.Fatalf("got %+v; want box %+v", d, want)
	}
	if abs32(d.score-0.81) > 0.001 {
		t.Errorf("score = %v; want 0.81", d.score)
	}
}

func abs32(v float32) float32 { return max(v, -v) }

func TestNMS(t *testing.T) {
	a := detection{class: 0, score: 0.9, x0: 0, y0: 0, x1: 10, y1: 10}
	b := detection{class: 0, score: 0.8, x0: 1, y0: 1, x1: 11, y1: 11} // overlaps a
	c := detection{class: 2, score: 0.7, x0: 1, y0: 1, x1: 11, y1: 11} // same object, other class
	d := detection{class: 0, score: 0.6, x0: 50, y0: 50, x1: 60, y1: 60}
	got := nms([]detection{d, b, c, a}, 0.45)
	if len(got) != 2 || got[0] != a || got[1] != d {
		t.Fatalf("nms = %+v; want [a d]", got)
	}
}

func TestTracker(t *testing.T) {
	var tr tracker
	person := []detection{{class: 0}}
	start := time.Now()
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }

	if got := tr.update(person, at(0)); got != nil {
		t.Fatalf("announced after one sighting: %v", got)
	}
	if got := tr.update(person, at(0.25)); len(got) != 1 || got[0] != 0 {
		t.Fatalf("not announced after two sightings: %v", got)
	}
	if got := tr.update(person, at(0.5)); got != nil {
		t.Fatalf("announced again while present: %v", got)
	}
	// Briefly lost and found again: not a new arrival.
	tr.update(nil, at(1))
	tr.update(person, at(5))
	if got := tr.update(person, at(5.25)); got != nil {
		t.Fatalf("announced after a short gap: %v", got)
	}
	// Gone for longer than goneAfter: a new arrival.
	tr.update(nil, at(5.5))
	tr.update(nil, at(40))
	tr.update(person, at(41))
	if got := tr.update(person, at(41.25)); len(got) != 1 {
		t.Fatalf("not announced after a long absence: %v", got)
	}
	// A blip that is never confirmed is ignored.
	if got := tr.update([]detection{{class: 16}}, at(42)); got != nil {
		t.Fatalf("unconfirmed dog announced: %v", got)
	}
}

// TestDetectorOnFrame runs the real model on an image, so it needs ONNX
// Runtime, ffmpeg and the model (downloaded on first use). Run it with
// PEEP_DETECT_TEST=<image> and PEEP_DETECT_EXPECT=<class>, e.g. truck.
func TestDetectorOnFrame(t *testing.T) {
	img := os.Getenv("PEEP_DETECT_TEST")
	if img == "" {
		t.Skip("set PEEP_DETECT_TEST to an image to run")
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "default=noprint_wrappers=1", img).Output()
	if err != nil {
		t.Fatal(err)
	}
	info, err := parseProbe(string(probe))
	if err != nil {
		t.Fatal(err)
	}
	geom := planGeom(info.width, info.height, maxCaptureDim)
	frame, err := exec.Command("ffmpeg", "-v", "error", "-i", img,
		"-vf", "scale=out_color_matrix=bt709:out_range=tv,"+geom.filter,
		"-f", "rawvideo", "-pix_fmt", "nv12", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDetector("test", geom, bt709)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	start := time.Now()
	dets, err := d.analyze(frame)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("analyzed in %v", time.Since(start))
	found := false
	for _, det := range dets {
		t.Logf("%s at %.0f,%.0f-%.0f,%.0f", det.label(), det.x0, det.y0, det.x1, det.y1)
		found = found || cocoNames[det.class] == os.Getenv("PEEP_DETECT_EXPECT")
	}
	if want := os.Getenv("PEEP_DETECT_EXPECT"); want != "" && !found {
		t.Errorf("no %s detected", want)
	}
}
