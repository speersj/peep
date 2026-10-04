package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDecodeDFINE(t *testing.T) {
	const queries = 4
	logits := make([]float32, queries*80)
	for i := range logits {
		logits[i] = -10 // sigmoid ~0
	}
	boxes := make([]float32, queries*4)
	set := func(q, class int, logit float32, cx, cy, w, h float32) {
		logits[q*80+class] = logit
		copy(boxes[q*4:], []float32{cx, cy, w, h})
	}
	set(0, 0, 2, 0.25, 0.5, 0.1, 0.2)   // person, sigmoid(2) = 0.881
	set(1, 7, 1, 0.75, 0.5, 0.2, 0.2)   // truck, 0.731
	set(2, 2, 0.5, 0.75, 0.5, 0.2, 0.2) // the same vehicle as a car: merged
	set(3, 56, 5, 0.5, 0.5, 0.1, 0.1)   // a chair: not reported

	dets := decodeDFINE(logits, boxes, 0.5, 1280, 720)
	if len(dets) != 2 || dets[0].class != 0 || dets[1].class != 7 {
		t.Fatalf("got %+v; want a person and a truck", dets)
	}
	p := dets[0]
	if abs32(p.x0-256) > 0.5 || abs32(p.y0-288) > 0.5 || abs32(p.x1-384) > 0.5 || abs32(p.y1-432) > 0.5 {
		t.Errorf("person box %+v; want 256,288-384,432", p)
	}
	if abs32(p.score-0.881) > 0.001 {
		t.Errorf("score = %v; want 0.881", p.score)
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

// An object that comes and goes without moving, like a garden lamp taken
// for a bird, is announced once; one that reappears elsewhere is new.
func TestTrackerIgnoresObjectsThatDoNotMove(t *testing.T) {
	var tr tracker
	start := time.Now()
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	lamp := detection{class: 14, score: 0.55, x0: 720, y0: 670, x1: 760, y1: 740}
	jiggled := detection{class: 14, score: 0.52, x0: 722, y0: 668, x1: 761, y1: 742}
	elsewhere := detection{class: 14, score: 0.8, x0: 100, y0: 100, x1: 140, y1: 170}

	tr.update([]detection{lamp}, at(0))
	if got := tr.update([]detection{lamp}, at(0.25)); len(got) != 1 {
		t.Fatalf("first sighting not announced: %v", got)
	}
	// Lost for longer than goneAfter, then back in the same place.
	tr.update(nil, at(1))
	tr.update(nil, at(40))
	tr.update([]detection{jiggled}, at(41))
	if got := tr.update([]detection{jiggled}, at(41.25)); got != nil {
		t.Fatalf("reappearance in the same place announced: %v", got)
	}
	// Gone again, then a bird somewhere else, with the lamp still there.
	tr.update(nil, at(42))
	tr.update(nil, at(80))
	tr.update([]detection{lamp, elsewhere}, at(81))
	if got := tr.update([]detection{lamp, elsewhere}, at(81.25)); len(got) != 1 {
		t.Fatalf("bird in a new place not announced: %v", got)
	}
	// After spotMemory without being seen, the place is forgotten.
	tr.update(nil, at(82))
	later := 82 + spotMemory.Seconds() + 60
	tr.update(nil, at(later))
	tr.update([]detection{lamp}, at(later+1))
	if got := tr.update([]detection{lamp}, at(later+1.25)); len(got) != 1 {
		t.Fatalf("not announced after the place was forgotten: %v", got)
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

func TestPruneEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, size int, age time.Duration) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	// 20 files of 100 bytes, f00 the oldest, plus one past the age limit.
	for i := range 20 {
		write(fmt.Sprintf("f%02d.jpg", i), 100, time.Duration(20-i)*time.Hour)
	}
	write("ancient.jpg", 100, 8*24*time.Hour)

	// Under the size limit: only the old file goes.
	if n, err := pruneEvents(dir, 7*24*time.Hour, 5000, now); err != nil || n != 1 {
		t.Fatalf("pruneEvents = %d, %v; want 1 aged out", n, err)
	}
	// 2000 bytes over a 1900 limit: the oldest 10% (2 files) go.
	if n, err := pruneEvents(dir, 7*24*time.Hour, 1900, now); err != nil || n != 2 {
		t.Fatalf("pruneEvents = %d, %v; want 2", n, err)
	}
	for _, gone := range []string{"f00.jpg", "f01.jpg", "ancient.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
	// Far over the limit: rounds of 10% repeat until under it.
	if _, err := pruneEvents(dir, 7*24*time.Hour, 500, now); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 5 || len(entries) == 0 {
		t.Fatalf("%d files left; want at most 5 (500 bytes) and the newest kept", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "f19.jpg")); err != nil {
		t.Error("newest file was deleted")
	}
	// A missing directory is fine.
	if _, err := pruneEvents(filepath.Join(dir, "nope"), time.Hour, 1, now); err != nil {
		t.Errorf("missing dir: %v", err)
	}
}
