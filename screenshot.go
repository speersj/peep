package main

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
)

// screenshotDir is where screenshots are saved: ~/Pictures.
func screenshotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Pictures"), nil
}

// createScreenshot creates a new file named peep-yyyy-mm-dd-hh-mm.png in
// dir, using t's time zone. Later shots in the same minute get a -2, -3, …
// suffix rather than overwriting earlier ones.
func createScreenshot(dir string, t time.Time) (*os.File, error) {
	base := "peep-" + t.Format("2006-01-02-15-04")
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		f, err := os.OpenFile(filepath.Join(dir, name+".png"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// writeScreenshot saves img as a PNG in ~/Pictures and returns its path.
func writeScreenshot(img image.Image, t time.Time) (string, error) {
	dir, err := screenshotDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := createScreenshot(dir, t)
	if err != nil {
		return "", err
	}
	err = png.Encode(f, img)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// screenshot captures the current frame at the stream's resolution and
// saves it in the background, so encoding does not stall playback.
func (g *game) screenshot() {
	if !g.hasFrame {
		return
	}
	t := time.Now()
	w, h := g.geom.width, g.geom.height
	if g.shot == nil {
		g.shot = ebiten.NewImage(w, h)
	}
	g.drawFrame(g.shot)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	g.shot.ReadPixels(img.Pix) // the shader outputs opaque pixels, so no unpremultiplying
	g.saving.Go(func() {
		path, err := writeScreenshot(img, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peep: saving screenshot: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "peep: saved %s\n", path)
	})
}
