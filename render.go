package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"sync"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"
)

// SDL texture creation properties, from SDL_render.h; the binding does not
// export them.
const (
	propTextureColorspace = "SDL.texture.create.colorspace"
	propTextureFormat     = "SDL.texture.create.format"
	propTextureAccess     = "SDL.texture.create.access"
	propTextureWidth      = "SDL.texture.create.width"
	propTextureHeight     = "SDL.texture.create.height"
)

// noVsyncDelay paces the loop when the renderer cannot wait for vsync.
const noVsyncDelay = 4 * time.Millisecond

// streamColorspace picks the YCbCr matrix for ffprobe's color_space. ffmpeg
// is told to deliver limited range. Untagged streams are assumed BT.709,
// which is standard for HD and larger.
func streamColorspace(colorSpace string) sdl.Colorspace {
	switch colorSpace {
	case "smpte170m", "bt470bg", "fcc":
		return sdl.COLORSPACE_BT601_LIMITED
	}
	return sdl.COLORSPACE_BT709_LIMITED
}

// initSDL loads the system SDL3 library and starts its video subsystem.
// SDL picks Wayland when it is available and falls back to X11.
func initSDL() (func(), error) {
	if err := sdl.LoadLibrary(sdl.Path()); err != nil {
		return nil, fmt.Errorf("loading SDL3 (is it installed?): %w", err)
	}
	// Wayland compositors use the app ID for window rules and icons.
	sdl.SetHint(sdl.HINT_APP_ID, "peep")
	// Ctrl+C is handled through run's signal context.
	sdl.SetHint(sdl.HINT_NO_SIGNAL_HANDLERS, "1")
	if err := sdl.Init(sdl.INIT_VIDEO); err != nil {
		sdl.CloseLibrary()
		return nil, fmt.Errorf("initializing SDL: %w", err)
	}
	return func() {
		sdl.Quit()
		sdl.CloseLibrary()
	}, nil
}

// player shows decoded frames in a window.
type player struct {
	ctx        context.Context
	st         *streamer
	sched      *scheduler
	geom       frameGeom
	colorspace sdl.Colorspace
	stats      bool
	det        *detector // nil unless detection is on
	camera     string    // saved camera name, for toggling detection
	matrix     yuvMatrix

	detLoading chan detectorResult // a detector being started, or nil

	toast      string // brief message shown over the video
	toastUntil time.Time

	renderer *sdl.Renderer
	yuv      *sdl.Texture // the current frame as decoded, NV12
	rgb      *sdl.Texture // the current frame converted to RGBA, full size
	vsync    bool         // Present waits for vsync
	incoming []*frame
	hasFrame bool
	saving   sync.WaitGroup // screenshots still being written

	statsAt    time.Time
	statsShown uint64
	statsRecv  uint64
	loops      uint64
	statsLoops uint64
}

// newRenderer creates the renderer for a window and the textures for
// frames of geom: video is letterboxed into the window at any size.
func (p *player) newRenderer(window *sdl.Window) error {
	r, err := window.CreateRenderer("")
	if err != nil {
		return fmt.Errorf("creating renderer: %w", err)
	}
	p.renderer = r
	// Show each video frame on the vsync closest to when it is due.
	p.vsync = r.SetVSync(1) == nil
	if err := r.SetLogicalPresentation(int32(p.geom.width), int32(p.geom.height), sdl.LOGICAL_PRESENTATION_LETTERBOX); err != nil {
		return fmt.Errorf("setting presentation: %w", err)
	}
	// Frames are converted to RGB once at full size, then scaled into the
	// window. Converting with nearest sampling repeats each chroma sample
	// across its two pixels, matching ffmpeg's own conversion; linear
	// sampling shifts colour edges by half a pixel. The RGB copy is scaled
	// linearly, so a window smaller than the video does not shimmer.
	p.yuv, err = p.newTexture(sdl.PIXELFORMAT_NV12, sdl.TEXTUREACCESS_STREAMING, p.geom.padW, p.geom.padH, p.colorspace)
	if err != nil {
		return fmt.Errorf("creating video texture: %w", err)
	}
	p.yuv.SetScaleMode(sdl.SCALEMODE_NEAREST)
	p.rgb, err = p.newTexture(sdl.PIXELFORMAT_RGBA32, sdl.TEXTUREACCESS_TARGET, p.geom.width, p.geom.height, sdl.COLORSPACE_SRGB)
	if err != nil {
		return fmt.Errorf("creating frame texture: %w", err)
	}
	p.rgb.SetScaleMode(sdl.SCALEMODE_LINEAR)
	return nil
}

func (p *player) newTexture(format sdl.PixelFormat, access sdl.TextureAccess, w, h int, cs sdl.Colorspace) (*sdl.Texture, error) {
	props, err := sdl.CreateProperties()
	if err != nil {
		return nil, err
	}
	defer props.Destroy()
	for name, v := range map[string]int64{
		propTextureFormat:     int64(format),
		propTextureAccess:     int64(access),
		propTextureWidth:      int64(w),
		propTextureHeight:     int64(h),
		propTextureColorspace: int64(cs),
	} {
		if err := props.SetNumberProperty(name, v); err != nil {
			return nil, err
		}
	}
	return p.renderer.CreateTextureWithProperties(props)
}

func (p *player) destroy() {
	if p.det != nil {
		p.det.close()
	}
	if p.rgb != nil {
		p.rgb.Destroy()
	}
	if p.yuv != nil {
		p.yuv.Destroy()
	}
	if p.renderer != nil {
		p.renderer.Destroy()
	}
}

// loop runs until the window is closed, a quit key is pressed or the
// stream fails.
func (p *player) loop() error {
	for {
		quit, err := p.handleEvents()
		if quit || err != nil || p.ctx.Err() != nil {
			return err
		}
		if err := p.st.cap.Err(); err != nil {
			return err
		}
		if err := p.update(time.Now()); err != nil {
			return err
		}
		if err := p.draw(); err != nil {
			return err
		}
		p.loops++
		if !p.vsync {
			time.Sleep(noVsyncDelay)
		}
	}
}

// handleEvents processes pending window events and reports whether to quit.
func (p *player) handleEvents() (bool, error) {
	var ev sdl.Event
	for sdl.PollEvent(&ev) {
		switch ev.Type {
		case sdl.EVENT_QUIT, sdl.EVENT_WINDOW_CLOSE_REQUESTED:
			return true, nil
		case sdl.EVENT_KEY_DOWN:
			k := ev.KeyboardEvent()
			if k.Repeat {
				continue
			}
			// Key is the keyboard layout's key, so a Caps Lock remapped
			// to Escape (e.g. XKB's caps:escape) arrives as K_ESCAPE.
			switch k.Key {
			case sdl.K_ESCAPE, sdl.K_Q:
				return true, nil
			case sdl.K_SPACE:
				p.screenshot()
			case sdl.K_T:
				p.toggleDetection()
			}
		case sdl.EVENT_MOUSE_BUTTON_DOWN:
			if ev.MouseButtonEvent().Button == uint8(sdl.BUTTON_LEFT) {
				p.screenshot()
			}
		}
	}
	return false, nil
}

// update uploads the frame that is due, if any.
func (p *player) update(now time.Time) error {
	p.pollDetector(now)
	p.incoming = p.st.cap.take(p.incoming[:0])
	for i, f := range p.incoming {
		p.sched.add(f)
		p.incoming[i] = nil
	}
	if f := p.sched.next(now); f != nil {
		err := p.uploadFrame(f.buf) // copies, so the buffer can be reused
		if p.det != nil {
			p.det.submit(f.buf, now)
		}
		p.st.cap.release(f.buf)
		if err != nil {
			return fmt.Errorf("uploading frame: %w", err)
		}
		p.hasFrame = true
	}
	if p.stats {
		p.printStats(now)
	}
	return nil
}

// uploadFrame loads an NV12 frame and converts it into p.rgb.
func (p *player) uploadFrame(buf []byte) error {
	pitch := p.geom.padW
	luma := pitch * p.geom.padH
	if err := p.yuv.UpdateNV(nil, buf[:luma], int32(pitch), buf[luma:], int32(pitch)); err != nil {
		return err
	}
	return p.withTarget(p.rgb, func() error {
		// Crop the padding ffmpeg added.
		src := sdl.FRect{W: float32(p.geom.width), H: float32(p.geom.height)}
		return p.renderer.RenderTexture(p.yuv, &src, &src)
	})
}

// withTarget runs fn with rendering directed into tex.
func (p *player) withTarget(tex *sdl.Texture, fn func() error) error {
	if err := p.renderer.SetRenderTarget(tex); err != nil {
		return fmt.Errorf("setting render target: %w", err)
	}
	err := fn()
	if rerr := p.renderer.SetRenderTarget(nil); err == nil && rerr != nil {
		err = fmt.Errorf("restoring render target: %w", rerr)
	}
	return err
}

func (p *player) draw() error {
	r := p.renderer
	r.SetDrawColor(0, 0, 0, 255)
	r.Clear()
	if p.hasFrame {
		if err := r.RenderTexture(p.rgb, nil, nil); err != nil {
			return fmt.Errorf("drawing frame: %w", err)
		}
		if p.det != nil {
			p.drawDetections(p.det.current(time.Now()))
		}
	}
	p.drawToast(time.Now())
	return r.Present()
}

// detectorResult is the outcome of starting a detector in the background.
type detectorResult struct {
	det *detector
	err error
}

// toggleDetection switches detection on or off for this camera and saves
// the choice. Starting the detector can involve downloading the model, so
// it happens in the background; stopping waits for an analysis in progress,
// so that happens in the background too.
func (p *player) toggleDetection() {
	now := time.Now()
	switch {
	case p.detLoading != nil:
		return // still starting
	case p.det != nil:
		det := p.det
		p.det = nil
		go det.close()
		p.showToast("Detection off", now)
		p.saveDetect(false)
	default:
		ch := make(chan detectorResult, 1)
		p.detLoading = ch
		camera, geom, matrix := p.camera, p.geom, p.matrix
		go func() {
			det, err := newDetector(camera, geom, matrix)
			ch <- detectorResult{det, err}
		}()
		p.showToast("Starting detection...", now)
	}
}

// pollDetector picks up a detector started by toggleDetection.
func (p *player) pollDetector(now time.Time) {
	if p.detLoading == nil {
		return
	}
	select {
	case res := <-p.detLoading:
		p.detLoading = nil
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "peep: detection is off: %v\n", res.err)
			p.showToast("Detection unavailable (see terminal)", now)
			return
		}
		p.det = res.det
		p.showToast("Detection on", now)
		p.saveDetect(true)
	default:
	}
}

// saveDetect records the detection setting for the camera.
func (p *player) saveDetect(on bool) {
	if p.camera == "" {
		return
	}
	if err := setCameraDetect(p.camera, on); err != nil {
		fmt.Fprintf(os.Stderr, "peep: saving detection setting: %v\n", err)
	}
}

const toastTime = 2 * time.Second

func (p *player) showToast(msg string, now time.Time) {
	p.toast, p.toastUntil = msg, now.Add(toastTime)
	if p.detLoading != nil {
		p.toastUntil = now.Add(time.Hour) // until the detector is ready
	}
}

// drawToast shows the current message in the top left corner.
func (p *player) drawToast(now time.Time) {
	if p.toast == "" || now.After(p.toastUntil) {
		return
	}
	r := p.renderer
	scale := max(1, float32(p.geom.width)/400)
	char := float32(sdl.DEBUG_TEXT_FONT_CHARACTER_SIZE) * scale
	pad := char / 2
	x, y := pad, pad
	r.SetDrawColor(0, 0, 0, 255)
	r.RenderFillRect(&sdl.FRect{X: x, Y: y, W: float32(len([]rune(p.toast)))*char + 2*pad, H: char + 2*pad})
	r.SetDrawColor(255, 255, 255, 255)
	r.SetScale(scale, scale)
	r.DebugText((x+pad)/scale, (y+pad)/scale, p.toast)
	r.SetScale(1, 1)
}

// drawDetections outlines and labels detected objects. Coordinates are in
// frame pixels, which the logical presentation maps onto the window.
func (p *player) drawDetections(dets []detection) {
	r := p.renderer
	w := float32(p.geom.width)
	thick := max(2, w/400)
	// Scale the 8px debug font with the frame so labels stay readable
	// when it is shown smaller.
	textScale := max(1, w/500)
	charSize := float32(sdl.DEBUG_TEXT_FONT_CHARACTER_SIZE) * textScale
	for _, d := range dets {
		c := kindColors[classKind(d.class)]
		r.SetDrawColor(c.R, c.G, c.B, 255)
		r.RenderFillRects([]sdl.FRect{
			{X: d.x0, Y: d.y0, W: d.x1 - d.x0, H: thick},
			{X: d.x0, Y: d.y1 - thick, W: d.x1 - d.x0, H: thick},
			{X: d.x0, Y: d.y0, W: thick, H: d.y1 - d.y0},
			{X: d.x1 - thick, Y: d.y0, W: thick, H: d.y1 - d.y0},
		})
		label := d.label()
		pad := charSize / 4
		lw, lh := float32(len(label))*charSize+2*pad, charSize+2*pad
		ly := d.y0 - lh
		if ly < 0 {
			ly = d.y0 // no room above the box
		}
		r.RenderFillRect(&sdl.FRect{X: d.x0, Y: ly, W: lw, H: lh})
		r.SetDrawColor(0, 0, 0, 255)
		r.SetScale(textScale, textScale)
		r.DebugText((d.x0+pad)/textScale, (ly+pad)/textScale, label)
		r.SetScale(1, 1)
	}
}

// readFrame reads the current frame back from the GPU as RGBA, at the
// stream's resolution.
func (p *player) readFrame() (*image.RGBA, error) {
	var surf *sdl.Surface
	err := p.withTarget(p.rgb, func() error {
		var err error
		surf, err = p.renderer.ReadPixels(nil)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading pixels: %w", err)
	}
	defer surf.Destroy()
	if surf.Format != sdl.PIXELFORMAT_RGBA32 {
		conv, err := surf.Convert(sdl.PIXELFORMAT_RGBA32)
		if err != nil {
			return nil, err
		}
		defer conv.Destroy()
		surf = conv
	}
	w, h := p.geom.width, p.geom.height
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pix, pitch := surf.Pixels(), int(surf.Pitch)
	for y := range h {
		copy(img.Pix[y*img.Stride:(y+1)*img.Stride], pix[y*pitch:])
	}
	return img, nil
}

func (p *player) printStats(now time.Time) {
	if p.statsAt.IsZero() {
		p.statsAt = now
		return
	}
	elapsed := now.Sub(p.statsAt).Seconds()
	if elapsed < 1 {
		return
	}
	recv := p.st.cap.Received()
	s := p.sched
	detect := ""
	if p.det != nil {
		detect = fmt.Sprintf(", detect %.0fms", float64(p.det.lastDuration())/float64(time.Millisecond))
	}
	fmt.Fprintf(os.Stderr, "recv %.1f fps, shown %.1f fps, cadence %.2f fps, queued %d, late %d, dropped %d, skips %d, latency %.0fms, loop %.0f/s%s\n",
		float64(recv-p.statsRecv)/elapsed, float64(s.shown-p.statsShown)/elapsed, float64(time.Second)/float64(s.interval),
		len(s.pending), s.late, s.dropped, s.reanchors, s.slack*1000, float64(p.loops-p.statsLoops)/elapsed, detect)
	p.statsAt, p.statsRecv, p.statsShown, p.statsLoops = now, recv, s.shown, p.loops
}
