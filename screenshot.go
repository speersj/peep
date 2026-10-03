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

// screenshot captures the current frame at the stream's resolution, then in
// the background, so encoding does not stall playback, saves it, copies it
// to the clipboard and shows a desktop notification that opens it.
func (p *player) screenshot() {
	if !p.hasFrame {
		return
	}
	t := time.Now()
	img, err := p.readFrame()
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: capturing screenshot: %v\n", err)
		return
	}
	p.saving.Go(func() {
		path, err := writeScreenshot(img, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peep: saving screenshot: %v\n", err)
			return
		}
		copyErr := copyImageFile(path)
		if copyErr != nil {
			fmt.Fprintf(os.Stderr, "peep: saved %s (not copied to the clipboard: %v)\n", path, copyErr)
		} else {
			fmt.Fprintf(os.Stderr, "peep: saved %s and copied it to the clipboard\n", path)
		}
		if err := notifyScreenshot(path, copyErr == nil); err != nil {
			fmt.Fprintf(os.Stderr, "peep: screenshot notification: %v\n", err)
		}
	})
}
