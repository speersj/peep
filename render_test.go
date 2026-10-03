package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/hajimehoshi/ebiten/v2"
)

// shaderHarness renders one NV12 frame through the game's shader into an
// offscreen image and reads it back.
type shaderHarness struct {
	g    *game
	nv12 []byte
	dst  *ebiten.Image
	out  []byte
	err  error
}

func (h *shaderHarness) Update() error {
	if h.g.packed == nil {
		if h.err = h.g.init(); h.err != nil {
			return h.err
		}
		h.g.packed.WritePixels(h.nv12)
		h.g.hasFrame = true
		h.dst = ebiten.NewImage(h.g.geom.width, h.g.geom.height)
		h.g.Draw(h.dst)
		return nil
	}
	h.out = make([]byte, 4*h.g.geom.width*h.g.geom.height)
	h.dst.ReadPixels(h.out)
	return ebiten.Termination
}

func (h *shaderHarness) Draw(*ebiten.Image)         {}
func (h *shaderHarness) Layout(int, int) (int, int) { return 64, 64 }

// TestShaderMatchesSource needs a GPU and opens a window for a moment, so it
// only runs with PEEP_GPU_TEST=1.
func TestShaderMatchesSource(t *testing.T) {
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

	hs := &shaderHarness{g: &game{geom: geom, coeffs: bt709Coeffs}, nv12: nv12}
	ebiten.SetWindowSize(64, 64)
	if err := ebiten.RunGame(hs); err != nil && !errors.Is(err, ebiten.Termination) {
		t.Fatal(err)
	}

	var sum, bad int
	for i := 0; i < len(want); i += 4 {
		for c := range 3 {
			d := int(hs.out[i+c]) - int(want[i+c])
			sum += abs(d)
			if abs(d) > 12 {
				bad++
			}
		}
		if hs.out[i+3] != 255 {
			t.Fatalf("pixel %d alpha = %d; want 255", i/4, hs.out[i+3])
		}
	}
	mean := float64(sum) / float64(w*h*3)
	t.Logf("mean abs error %.2f, %d of %d samples off by more than 12", mean, bad, w*h*3)
	if mean > 2.5 || bad > w*h*3/200 {
		t.Errorf("shader output differs from source: mean abs error %.2f, %d samples off by >12", mean, bad)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
