package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/Zyko0/go-sdl3/sdl"
)

// TestRenderMatchesSource needs SDL and a GPU and creates a hidden window, so
// it only runs with PEEP_GPU_TEST=1. It checks the renderer's NV12 to RGB
// conversion and the screenshot readback against ffmpeg's own conversion.
func TestRenderMatchesSource(t *testing.T) {
	if os.Getenv("PEEP_GPU_TEST") == "" {
		t.Skip("set PEEP_GPU_TEST=1 to run (opens a window)")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// Width 636 is not a multiple of 4, so this also checks padding/cropping.
	const w, h = 636, 360
	src := "testsrc2=size=636x360:rate=1,gblur=sigma=3"
	ffmpeg := func(args ...string) []byte {
		base := []string{"-v", "error", "-f", "lavfi", "-i", src, "-frames:v", "1"}
		out, err := exec.Command("ffmpeg", append(base, args...)...).Output()
		if err != nil {
			t.Fatalf("ffmpeg %v: %v", args, err)
		}
		return out
	}
	geom := planGeom(w, h, maxCaptureDim)
	want := ffmpeg("-f", "rawvideo", "-pix_fmt", "rgba", "-")
	nv12 := ffmpeg("-vf", "scale=out_color_matrix=bt709:out_range=tv,"+geom.filter, "-f", "rawvideo", "-pix_fmt", "nv12", "-")
	if len(nv12) != geom.frameLen() {
		t.Fatalf("nv12 frame is %d bytes; want %d", len(nv12), geom.frameLen())
	}

	// GL contexts belong to one OS thread; tests run on goroutines that
	// otherwise move between threads.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	quitSDL, err := initSDL()
	if err != nil {
		t.Fatal(err)
	}
	defer quitSDL()
	window, err := sdl.CreateWindow("peep test", 64, 64, sdl.WINDOW_HIDDEN)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Destroy()
	p := &player{geom: geom, colorspace: streamColorspace("bt709")}
	defer p.destroy()
	if err := p.newRenderer(window); err != nil {
		t.Fatal(err)
	}
	t.Logf("video driver %s", sdl.GetCurrentVideoDriver())
	if err := p.uploadFrame(nv12); err != nil {
		t.Fatal(err)
	}
	img, err := p.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	out := img.Pix

	var sum, bad int
	for i := 0; i < len(want); i += 4 {
		for c := range 3 {
			d := int(out[i+c]) - int(want[i+c])
			sum += abs(d)
			if abs(d) > 12 {
				bad++
			}
		}
		if out[i+3] != 255 {
			t.Fatalf("pixel %d alpha = %d; want 255", i/4, out[i+3])
		}
	}
	mean := float64(sum) / float64(w*h*3)
	t.Logf("mean abs error %.2f, %d of %d samples off by more than 12", mean, bad, w*h*3)
	if mean > 2.5 || bad > w*h*3/200 {
		t.Errorf("rendered output differs from source: mean abs error %.2f, %d samples off by >12", mean, bad)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
