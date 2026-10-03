package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateScreenshot(t *testing.T) {
	dir := t.TempDir()
	// The name uses the timestamp's own zone, not UTC.
	zone := time.FixedZone("test", -7*60*60)
	ts := time.Date(2026, 10, 3, 9, 5, 59, 0, time.UTC).In(zone)
	want := []string{"peep-2026-10-03-02-05.png", "peep-2026-10-03-02-05-2.png", "peep-2026-10-03-02-05-3.png"}
	for _, name := range want {
		f, err := createScreenshot(dir, ts)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if got := filepath.Base(f.Name()); got != name {
			t.Errorf("created %s; want %s", got, name)
		}
	}
}

func TestWriteScreenshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	img.Pix[0], img.Pix[3] = 200, 255
	path, err := writeScreenshot(img, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(home, "Pictures") {
		t.Fatalf("saved to %s; want it in ~/Pictures", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != img.Bounds() {
		t.Fatalf("bounds = %v; want %v", got.Bounds(), img.Bounds())
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r>>8 != 200 {
		t.Fatalf("pixel red = %d; want 200", r>>8)
	}
}
