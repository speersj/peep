// Command peep displays a live RTSP camera feed in a window.

// Video is decoded by ffmpeg, which streams raw NV12 frames over a pipe;
// no video is written to disk. Frames are held in a short jitter buffer
// and shown on their own timestamps, then converted to RGB on the GPU.
// Press ESC in the window to quit, and space or click to save a screenshot
// to ~/Pictures.
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
	"syscall"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"golang.org/x/term"
)

const (
	probeTimeout  = 15 * time.Second
	maxWindowW    = 1280
	maxWindowH    = 720
	maxCaptureDim = 4096
	stderrTail    = 8 * 1024
	defaultFPS    = 30
)

type config struct {
	host string
	user string
	pass string
	port int
	name string

	passChoice passChoice // what to do with pass in the keyring once opened

	buffer  time.Duration
	hwaccel string
	stats   bool
}

// boolFlags are the flags that take no value, which normalizeArgs must know.
var boolFlags = map[string]bool{"h": true, "help": true, "stats": true}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\nrun 'peep -h' for usage\n", err)
		os.Exit(2)
	}
	if cfg.host == "" {
		cfg, err = pickCamera(cfg)
		if errors.Is(err, errCanceled) {
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "peep: %v\n", err)
			os.Exit(1)
		}
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
	fs.StringVar(&cfg.pass, "password", "", "RTSP password (when -user is set and this is empty, the keyring is tried, then a prompt)")
	fs.StringVar(&cfg.pass, "pw", "", "RTSP password (shorthand)")
	fs.StringVar(&cfg.name, "name", "", "RTSP stream name/path, e.g. \"live/ch0\"")
	fs.StringVar(&cfg.name, "n", "", "RTSP stream name/path (shorthand)")
	fs.DurationVar(&cfg.buffer, "buffer", 250*time.Millisecond, "playback delay used to smooth out network jitter (0 for lowest latency)")
	fs.StringVar(&cfg.hwaccel, "hwaccel", "auto", "ffmpeg hardware decoding method, e.g. auto, vaapi, cuda; \"none\" to disable")
	fs.BoolVar(&cfg.stats, "stats", false, "print playback statistics to stderr once a second")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: peep [flags] [<host>[:port]]\n\n"+
			"Displays a live RTSP feed; ESC quits. Space or a click saves a\n"+
			"screenshot to ~/Pictures. The port defaults to %d.\n"+
			"Without a host, an interactive picker offers previously opened\n"+
			"cameras or walks through entering a new one.\n\n", defaultPort)
		fs.PrintDefaults()
	}
	fs.Parse(normalizeArgs(args)) // ExitOnError: exits on bad flags or -h

	if cfg.buffer < 0 || cfg.buffer > 10*time.Second {
		return cfg, fmt.Errorf("-buffer out of range: %v", cfg.buffer)
	}
	if fs.NArg() > 1 {
		return cfg, errors.New("at most one <host> argument is allowed")
	}
	if fs.NArg() == 0 {
		return cfg, nil // no host: the interactive picker fills in the rest
	}
	var err error
	if cfg.host, cfg.port, err = parseHost(fs.Arg(0)); err != nil {
		return cfg, err
	}
	if cfg.name == "" {
		return cfg, errors.New("-name is required, e.g. -n live/ch0")
	}
	if cfg.user != "" && cfg.pass == "" {
		if pass, ok := rememberedPassword(cfg); ok {
			cfg.pass = pass
			return cfg, nil
		}
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
// expects it. The token following a flag is carried along with it as its
// value, except for the boolean flags in boolFlags.
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
			if !strings.Contains(arg, "=") && !boolFlags[strings.TrimLeft(arg, "-")] && i+1 < len(args) {
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

// streamInfo is what ffprobe reports about the video stream.
type streamInfo struct {
	width, height int
	fps           float64 // 0 when unknown
	colorSpace    string
}

// probeStream asks ffprobe for the video size, frame rate and colour space
// so the window and buffers can be set up before the first frame arrives.
// ffprobe disconnects before ffmpeg connects, which keeps cameras that allow
// a single RTSP session happy.
func probeStream(ctx context.Context, streamURL string) (streamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-rtsp_transport", "tcp",
		"-analyzeduration", "1000000",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,avg_frame_rate,r_frame_rate,color_space",
		"-of", "default=noprint_wrappers=1",
		streamURL,
	)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return streamInfo{}, fmt.Errorf("probing stream: %w", ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			msg := strings.TrimSpace(string(exit.Stderr))
			if msg == "" {
				msg = exit.Error()
			}
			return streamInfo{}, fmt.Errorf("probing stream: %s", msg)
		}
		return streamInfo{}, fmt.Errorf("probing stream: %w", err)
	}
	return parseProbe(string(out))
}

// parseProbe parses ffprobe's key=value output for one stream.
func parseProbe(out string) (streamInfo, error) {
	var info streamInfo
	var avgFPS, rFPS float64
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "width":
			info.width, _ = strconv.Atoi(val)
		case "height":
			info.height, _ = strconv.Atoi(val)
		case "avg_frame_rate":
			avgFPS = parseRate(val)
		case "r_frame_rate":
			rFPS = parseRate(val)
		case "color_space":
			info.colorSpace = val
		}
	}
	if info.width <= 0 || info.height <= 0 {
		return info, fmt.Errorf("could not parse video dimensions from ffprobe output %q", strings.TrimSpace(out))
	}
	info.fps = avgFPS
	if info.fps == 0 {
		info.fps = rFPS
	}
	return info, nil
}

// parseRate parses an ffprobe rational such as "30000/1001", returning 0
// for unknown or implausible rates.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		den = "1"
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	if r := n / d; r > 0 && r <= 240 {
		return r
	}
	return 0
}

// poolSize is how many frame buffers to keep: enough to hold the playback
// delay with headroom for bursts, plus a few in flight.
func poolSize(delay time.Duration, fps float64) int {
	return int(math.Ceil(delay.Seconds()*fps*1.5)) + 4
}

func run(cfg config) error {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	streamURL := buildURL(cfg)
	info, err := probeStream(ctx, streamURL)
	if err != nil {
		return err
	}
	if err := recordOpened(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "peep: remembering camera: %v\n", err)
	}
	fps := info.fps
	if fps == 0 {
		fps = defaultFPS
	}
	geom := planGeom(info.width, info.height, maxCaptureDim)
	pool := poolSize(cfg.buffer, fps)
	input := []string{
		// No -fflags nobuffer or -flags low_delay: the jitter buffer
		// handles latency, and both can make the decoder lose frames on
		// streams with B-frames.
		"-rtsp_transport", "tcp",
		"-analyzeduration", "0",
		// Keep decoded frames at the coded resolution so they always match
		// the dimensions ffprobe reported, even for rotated streams.
		"-noautorotate",
		"-i", streamURL,
	}
	var ffmpegLog io.Writer
	if cfg.stats {
		ffmpegLog = os.Stderr
	}
	st, err := startCapture(ctx, input, cfg.hwaccel, geom.filter, geom.frameLen(), pool, ffmpegLog)
	if err != nil {
		return err
	}
	defer st.stop()

	winW, winH := fitWindow(geom.width, geom.height, maxWindowW, maxWindowH)
	ebiten.SetWindowSize(winW, winH)
	ebiten.SetWindowTitle("peep - " + cameraFromConfig(cfg).label())
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	// Run Update once per displayed frame so each video frame is shown on
	// the vsync closest to when it is due.
	ebiten.SetTPS(ebiten.SyncWithFPS)

	g := &game{
		ctx:    ctx,
		st:     st,
		sched:  newScheduler(cfg.buffer, fps, pool-2, st.cap.release),
		geom:   geom,
		coeffs: colorCoeffs(info.colorSpace),
		stats:  cfg.stats,
	}
	err = ebiten.RunGame(g)
	g.saving.Wait() // let screenshots in progress finish writing
	if err != nil && !errors.Is(err, ebiten.Termination) {
		return err
	}
	return nil
}

// frameGeom describes the frames ffmpeg delivers.
type frameGeom struct {
	width, height int    // displayed picture size
	padW, padH    int    // NV12 plane size: width a multiple of 4, height even
	filter        string // ffmpeg -vf expression producing them
}

func (g frameGeom) frameLen() int { return g.padW * g.padH * 3 / 2 }

// packedSize is the size of the RGBA image the NV12 bytes are uploaded into.
func (g frameGeom) packedSize() (int, int) { return g.padW / 4, g.padH * 3 / 2 }

// planGeom works out the output geometry and ffmpeg filter for a w x h
// stream: downscale if too large, normalize to limited colour range (the
// shader assumes it), and pad so NV12 rows pack into whole RGBA pixels.
func planGeom(w, h, maxDim int) frameGeom {
	cw, ch, scale := captureSize(w, h, maxDim)
	g := frameGeom{width: cw, height: ch, padW: (cw + 3) &^ 3, padH: (ch + 1) &^ 1}
	if scale == "" {
		g.filter = "scale=out_range=tv"
	} else {
		g.filter = scale + ":out_range=tv"
	}
	if g.padW != cw || g.padH != ch {
		g.filter += fmt.Sprintf(",pad=%d:%d", g.padW, g.padH)
	}
	return g
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
