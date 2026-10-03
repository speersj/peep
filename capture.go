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

// captureArgs builds the ffmpeg command line: decode input (with optional
// hardware acceleration) and emit every frame exactly once as raw NV12 on
// stdout. filter is an ffmpeg -vf
// expression ("" for none); verbose raises the log level to include
// warnings such as decode errors. Nothing is written to disk.
func captureArgs(input []string, hwaccel, filter string, verbose bool) []string {
	level := "error"
	if verbose {
		level = "warning"
	}
	args := []string{"-hide_banner", "-loglevel", level, "-nostdin"}
	if hwaccel != "" && hwaccel != "none" {
		args = append(args, "-hwaccel", hwaccel)
	}
	args = append(args, input...)
	args = append(args, "-an", "-sn")
	if filter != "" {
		args = append(args, "-vf", filter)
	}
	return append(args,
		// rawvideo carries no timestamps, so without passthrough ffmpeg
		// assumes a constant frame rate and duplicates or drops frames.
		"-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "nv12", "-",
	)
}

// startCapture spawns ffmpeg with input as its input arguments and starts
// feeding frames of frameLen bytes into a capture with a pool of poolSize
// buffers. When logTo is non-nil, ffmpeg's warnings are also copied to it.
func startCapture(ctx context.Context, input []string, hwaccel, filter string, frameLen, poolSize int, logTo io.Writer) (*streamer, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "ffmpeg", captureArgs(input, hwaccel, filter, logTo != nil)...)

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
