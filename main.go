// Command peep displays a live RTSP camera feed in a window.
//
// Video is decoded by ffmpeg, which streams raw RGBA frames over a pipe;
// nothing is ever written to disk. Press ESC in the window to quit.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"golang.org/x/term"
)

const (
	probeTimeout  = 15 * time.Second
	maxWindowW    = 1280
	maxWindowH    = 720
	maxCaptureDim = 4096
	stderrTail    = 8 * 1024
)

type config struct {
	host string
	user string
	pass string
	port int
	name string
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\nrun 'peep -h' for usage\n", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\n", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("peep", flag.ExitOnError)
	fs.StringVar(&cfg.user, "user", "", "RTSP username (optional)")
	fs.StringVar(&cfg.user, "u", "", "RTSP username (shorthand)")
	fs.StringVar(&cfg.pass, "password", "", "RTSP password (prompted when -user is set and this is empty)")
	fs.StringVar(&cfg.pass, "pw", "", "RTSP password (shorthand)")
	fs.IntVar(&cfg.port, "port", 554, "RTSP port")
	fs.IntVar(&cfg.port, "p", 554, "RTSP port (shorthand)")
	fs.StringVar(&cfg.name, "name", "", "RTSP stream name/path, e.g. \"live/ch0\"")
	fs.StringVar(&cfg.name, "n", "", "RTSP stream name/path (shorthand)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: peep [flags] <host>\n\nDisplays a live RTSP feed; ESC quits.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(normalizeArgs(args)) // ExitOnError: exits on bad flags or -h

	if fs.NArg() != 1 {
		return cfg, errors.New("exactly one <host> argument is required")
	}
	cfg.host = fs.Arg(0)
	if cfg.name == "" {
		return cfg, errors.New("-name is required, e.g. -n live/ch0")
	}
	if cfg.port < 1 || cfg.port > 65535 {
		return cfg, fmt.Errorf("-port out of range: %d", cfg.port)
	}
	if cfg.user != "" && cfg.pass == "" {
		pass, err := promptPassword()
		if err != nil {
			return cfg, err
		}
		cfg.pass = pass
	}
	return cfg, nil
}

// normalizeArgs lets the positional <host> appear before, between, or after
// flags by moving every non-flag argument to the end, where flag.Parse
// expects it. All of peep's flags take a value, so the token following a flag
// is carried along with it; -h/--help take none.
func normalizeArgs(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			flags = append(flags, arg) // keep the terminator for flag.Parse
			positional = append(positional, args[i+1:]...)
			return append(flags, positional...)
		case strings.HasPrefix(arg, "-") && arg != "-":
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && arg != "-h" && arg != "--help" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}

// promptPassword reads the RTSP password from the terminal without echo, or
// from stdin when it is not a terminal.
func promptPassword() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "RTSP password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return string(b), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// buildURL renders rtsp://user:password@host:port/name, escaping credentials
// and the stream path safely.
func buildURL(cfg config) string {
	u := url.URL{
		Scheme: "rtsp",
		Host:   net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		Path:   "/" + strings.TrimPrefix(cfg.name, "/"),
	}
	if cfg.user != "" {
		u.User = url.UserPassword(cfg.user, cfg.pass)
	}
	return u.String()
}

// probeDimensions asks ffprobe for the video size so the window can be sized
// before the first frame arrives. ffprobe disconnects before ffmpeg connects,
// which keeps cameras that allow a single RTSP session happy.
func probeDimensions(ctx context.Context, streamURL string) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-rtsp_transport", "tcp",
		"-analyzeduration", "1000000",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0:s=x",
		streamURL,
	)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, fmt.Errorf("probing stream: %w", ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			msg := strings.TrimSpace(string(exit.Stderr))
			if msg == "" {
				msg = exit.Error()
			}
			return 0, 0, fmt.Errorf("probing stream: %s", msg)
		}
		return 0, 0, fmt.Errorf("probing stream: %w", err)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("could not parse video dimensions from ffprobe output %q", strings.TrimSpace(string(out)))
	}
	return w, h, nil
}

type capture struct {
	mu     sync.Mutex
	latest []byte
	gen    uint64
	seen   uint64
	err    error
}

// writeLatest uploads the newest captured frame into img and reports whether
// a new frame was available. Holding the lock during the upload means the
// reader briefly waits instead of racing over the shared buffer.
func (c *capture) writeLatest(img *ebiten.Image) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == c.seen {
		return false
	}
	c.seen = c.gen
	img.WritePixels(c.latest)
	return true
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
	cap  *capture
	cmd  *exec.Cmd
	done chan struct{}
}

// startCapture spawns ffmpeg, which decodes the RTSP stream and writes raw
// RGBA frames to stdout. filter is an optional ffmpeg -vf expression ("" for
// none). Nothing is written to disk.
func startCapture(ctx context.Context, streamURL string, width, height int, filter string) (*streamer, error) {
	frameLen := width * height * 4
	sc := &capture{latest: make([]byte, frameLen)}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-rtsp_transport", "tcp",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-analyzeduration", "0",
		// Keep decoded frames at the coded resolution so they always match
		// the dimensions ffprobe reported, even for rotated streams.
		"-noautorotate",
		"-i", streamURL,
		"-an",
		"-sn",
	}
	if filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args, "-f", "rawvideo", "-pix_fmt", "rgba", "-")

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	tail := &tailWriter{limit: stderrTail}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	s := &streamer{cap: sc, cmd: cmd, done: make(chan struct{})}

	go func() {
		defer close(s.done)
		readErr := readFrames(stdout, sc, frameLen)
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
		sc.setErr(err)
	}()
	return s, nil
}

func (s *streamer) stop() {
	_ = s.cmd.Process.Kill()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
}

func readFrames(r io.Reader, c *capture, frameLen int) error {
	buf := make([]byte, frameLen)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		c.mu.Lock()
		copy(c.latest, buf)
		c.gen++
		c.mu.Unlock()
	}
}

type game struct {
	ctx    context.Context
	st     *streamer
	width  int
	height int
	img    *ebiten.Image
}

func (g *game) Update() error {
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) || g.ctx.Err() != nil {
		return ebiten.Termination
	}
	if err := g.st.cap.Err(); err != nil {
		return err
	}
	if g.img == nil {
		if max := ebiten.MaxImageSize(); max > 0 && (g.width > max || g.height > max) {
			return fmt.Errorf("video %dx%d exceeds the maximum texture size %d", g.width, g.height, max)
		}
		g.img = ebiten.NewImage(g.width, g.height)
	}
	g.st.cap.writeLatest(g.img)
	return nil
}

func (g *game) Draw(screen *ebiten.Image) {
	if g.img != nil {
		screen.DrawImage(g.img, nil)
	}
}

func (g *game) Layout(_, _ int) (int, int) {
	return g.width, g.height
}

func run(cfg config) error {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	streamURL := buildURL(cfg)
	width, height, err := probeDimensions(ctx, streamURL)
	if err != nil {
		return err
	}
	captureW, captureH, filter := captureSize(width, height, maxCaptureDim)
	st, err := startCapture(ctx, streamURL, captureW, captureH, filter)
	if err != nil {
		return err
	}
	defer st.stop()

	winW, winH := fitWindow(captureW, captureH, maxWindowW, maxWindowH)
	ebiten.SetWindowSize(winW, winH)
	ebiten.SetWindowTitle(fmt.Sprintf("peep - %s:%d/%s", cfg.host, cfg.port, strings.TrimPrefix(cfg.name, "/")))
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)

	g := &game{ctx: ctx, st: st, width: captureW, height: captureH}
	if err := ebiten.RunGame(g); err != nil && !errors.Is(err, ebiten.Termination) {
		return err
	}
	return nil
}

// captureSize decides the output dimensions for ffmpeg. Streams larger than
// maxDim on either axis are scaled down proportionally (keeping dimensions
// even), since GPUs reject oversized textures. It returns the ffmpeg -vf
// filter to apply, or "" when no scaling is needed.
func captureSize(w, h, maxDim int) (int, int, string) {
	if w <= maxDim && h <= maxDim {
		return w, h, ""
	}
	s := float64(maxDim) / float64(max(w, h))
	nw := int(math.Floor(float64(w)*s)) &^ 1
	nh := int(math.Floor(float64(h)*s)) &^ 1
	if nw < 2 {
		nw = 2
	}
	if nh < 2 {
		nh = 2
	}
	return nw, nh, fmt.Sprintf("scale=%d:%d", nw, nh)
}

// fitWindow scales w x h down to fit within maxW x maxH, preserving aspect.
func fitWindow(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	s := math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	nw := int(math.Round(float64(w) * s))
	nh := int(math.Round(float64(h) * s))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}
