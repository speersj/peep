package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// pipeSize is the kernel buffer requested for ffmpeg's frame pipe; larger
// than the 64KiB default so multi-megabyte frames move in fewer wakeups.
const pipeSize = 1 << 20

// capture hands decoded frames from the ffmpeg reader goroutine to the
// render loop. Frame buffers cycle through a fixed-size pool, so frames are
// never copied after being read from the pipe.
type capture struct {
	frameLen int
	poolSize int
	free     chan []byte
	alloc    int // buffers created so far; reader goroutine only

	mu       sync.Mutex
	queue    []*frame
	received uint64
	err      error
}

func newCapture(frameLen, poolSize int) *capture {
	return &capture{frameLen: frameLen, poolSize: poolSize, free: make(chan []byte, poolSize)}
}

// getBuf returns a free frame buffer, allocating lazily up to poolSize and
// then waiting for the renderer to release one.
func (c *capture) getBuf(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.free:
		return b, nil
	default:
	}
	if c.alloc < c.poolSize {
		c.alloc++
		return make([]byte, c.frameLen), nil
	}
	select {
	case b := <-c.free:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *capture) release(b []byte) {
	select {
	case c.free <- b:
	default: // cannot happen: the pool never holds more than poolSize
	}
}

func (c *capture) push(f *frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, f)
	c.received++
}

// take moves every queued frame into dst and returns it.
func (c *capture) take(dst []*frame) []*frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	dst = append(dst, c.queue...)
	clear(c.queue)
	c.queue = c.queue[:0]
	return dst
}

func (c *capture) Received() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.received
}

func (c *capture) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// tailWriter keeps only the last limit bytes written to it, so a failed
// ffmpeg can report why without flooding the terminal.
type tailWriter struct {
	mu    sync.Mutex
	tail  []byte
	limit int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = append(w.tail, p...)
	if len(w.tail) > w.limit {
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-w.limit:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.tail)
}

type streamer struct {
	cap    *capture
	cancel context.CancelFunc
	done   chan struct{}
}

// decoder is how ffmpeg decodes the stream and turns it into the frames
// described by a frameGeom.
type decoder struct {
	name   string   // for messages
	hwArgs []string // ffmpeg input options selecting hardware decoding
	filter string   // ffmpeg -vf expression producing NV12 frames
}

// decoders lists the decoders to try for -hwaccel, best first. VA-API
// decodes and converts on the GPU, copying back only the finished NV12
// frames; when it is available it is tried first, with software decoding
// as the fallback. Letting ffmpeg download VA-API frames and convert them
// on the CPU, as plain -hwaccel vaapi does, cost more CPU than decoding in
// software for a 2560x1440 H.264 camera.
func decoders(hwaccel string, g frameGeom, vaapiOK bool) []decoder {
	software := decoder{name: "software", filter: g.filter}
	vaapi := decoder{
		name:   "VA-API",
		hwArgs: []string{"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi"},
		filter: g.vaapiFilter(),
	}
	switch hwaccel {
	case "", "none":
		return []decoder{software}
	case "auto":
		if vaapiOK {
			return []decoder{vaapi, software}
		}
		return []decoder{{name: "ffmpeg auto", hwArgs: []string{"-hwaccel", "auto"}, filter: g.filter}}
	case "vaapi":
		if vaapiOK {
			return []decoder{vaapi, software}
		}
		return []decoder{software}
	}
	return []decoder{{name: hwaccel, hwArgs: []string{"-hwaccel", hwaccel}, filter: g.filter}}
}

// vaapiAvailable reports whether ffmpeg can open a VA-API device.
func vaapiAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-v", "error", "-nostdin",
		"-init_hw_device", "vaapi", "-f", "lavfi", "-i", "nullsrc=s=16x16", "-frames:v", "1", "-f", "null", "-")
	return cmd.Run() == nil
}

// startDecoding starts capture with the first of decs that delivers a
// frame, falling back to the next when ffmpeg fails before then, e.g. when
// the GPU cannot decode the stream's codec. A decoder still connecting
// after startupWait is kept.
func startDecoding(ctx context.Context, input []string, decs []decoder, frameLen, poolSize int, logTo io.Writer) (*streamer, decoder, error) {
	for i, dec := range decs {
		st, err := startCapture(ctx, input, dec, frameLen, poolSize, logTo)
		if err != nil {
			return nil, dec, err
		}
		err = st.waitStarted(ctx, startupWait)
		if err == nil || i == len(decs)-1 || ctx.Err() != nil {
			return st, dec, nil // a failure is reported by the render loop
		}
		st.stop()
		fmt.Fprintf(os.Stderr, "peep: %s decoding failed, trying %s: %v\n", dec.name, decs[i+1].name, err)
	}
	panic("no decoders")
}

// startupWait is how long startDecoding waits for a first frame.
const startupWait = 15 * time.Second

// waitStarted waits until the first frame arrives, returning ffmpeg's
// error if it fails first. Running out of time is not an error.
func (s *streamer) waitStarted(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s.cap.Received() > 0 {
			return nil
		}
		if err := s.cap.Err(); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// captureArgs builds the ffmpeg command line: decode input with dec and
// emit every frame exactly once as raw NV12 on stdout. verbose raises the
// log level to include warnings such as decode errors. Nothing is written
// to disk.
func captureArgs(input []string, dec decoder, verbose bool) []string {
	level := "error"
	if verbose {
		level = "warning"
	}
	args := []string{"-hide_banner", "-loglevel", level, "-nostdin"}
	args = append(args, dec.hwArgs...)
	args = append(args, input...)
	args = append(args, "-an", "-sn")
	if dec.filter != "" {
		args = append(args, "-vf", dec.filter)
	}
	return append(args,
		// rawvideo carries no timestamps, so without passthrough ffmpeg
		// assumes a constant frame rate and duplicates or drops frames.
		"-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "nv12", "-",
	)
}

// startCapture spawns ffmpeg with input as its input arguments, decoding
// with dec, and starts feeding frames of frameLen bytes into a capture with
// a pool of poolSize buffers. When logTo is non-nil, ffmpeg's warnings are
// also copied to it.
func startCapture(ctx context.Context, input []string, dec decoder, frameLen, poolSize int, logTo io.Writer) (*streamer, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "ffmpeg", captureArgs(input, dec, logTo != nil)...)

	frames, framesW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	setPipeSize(frames, pipeSize)
	cmd.Stdout = framesW
	tail := &tailWriter{limit: stderrTail}
	cmd.Stderr = tail
	if logTo != nil {
		cmd.Stderr = io.MultiWriter(tail, logTo)
	}
	err = cmd.Start()
	framesW.Close()
	if err != nil {
		cancel()
		frames.Close()
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	c := newCapture(frameLen, poolSize)
	s := &streamer{cap: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		readErr := readFrames(ctx, frames, c)
		frames.Close()
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return // killed during shutdown; not an error
		}
		err := errors.New("video stream ended")
		switch {
		case waitErr != nil:
			err = fmt.Errorf("ffmpeg exited: %v", waitErr)
		case readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF):
			err = fmt.Errorf("video stream ended: %v", readErr)
		}
		if msg := strings.TrimSpace(tail.String()); msg != "" {
			err = fmt.Errorf("%w\nffmpeg: %s", err, msg)
		}
		c.setErr(err)
	}()
	return s, nil
}

func (s *streamer) stop() {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
}

// readFrames reads whole frames into pooled buffers and queues them on c.
func readFrames(ctx context.Context, r io.Reader, c *capture) error {
	for {
		buf, err := c.getBuf(ctx)
		if err != nil {
			return err
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		c.push(&frame{buf: buf, arrival: time.Now()})
	}
}
