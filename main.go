// Command peep displays a live RTSP camera feed in a window.

// Video is decoded by ffmpeg, which streams raw NV12 frames over a pipe;
// no video is written to disk. Frames are held in a short jitter buffer
// and shown on their own timestamps, then converted to RGB on the GPU.
// Press ESC or q in the window to quit, space or click to save a screenshot
// to ~/Pictures and copy it to the clipboard, and t to turn object
// detection on or off.
package main

import (
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"
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
	camera string // the camera's name
	host   string
	user   string
	pass   string
	port   int
	stream string // RTSP stream path, e.g. "live/ch0"

	passChoice passChoice // what to do with pass in the keyring once opened
	replaces   string     // the camera's previous name, when it was renamed

	buffer  time.Duration
	hwaccel string
	stats   bool
	detect  bool // the camera's object detection setting
}

// SDL, and the OpenGL context it renders with, must stay on one OS thread,
// so keep the main goroutine, which runs the window, on the main thread.
func init() { runtime.LockOSThread() }

// boolFlags are the flags that take no value, which normalizeArgs must know.
var boolFlags = map[string]bool{"h": true, "help": true, "stats": true}

func main() {
	runNotifierIfRequested()
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\nrun 'peep -h' for usage\n", err)
		os.Exit(2)
	}
	cfg, err = pickCamera(cfg)
	if errors.Is(err, errCanceled) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\n", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\n", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("peep", flag.ExitOnError)
	fs.DurationVar(&cfg.buffer, "buffer", 250*time.Millisecond, "playback delay used to smooth out network jitter (0 for lowest latency)")
	fs.StringVar(&cfg.hwaccel, "hwaccel", "auto", "ffmpeg hardware decoding method, e.g. auto, vaapi, cuda; \"none\" to disable")
	fs.BoolVar(&cfg.stats, "stats", false, "print playback statistics to stderr once a second")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: peep [flags] [camera]\n\n"+
			"Displays a live RTSP camera feed; ESC or q quits. Space or a\n"+
			"click saves a screenshot to ~/Pictures and copies it to the\n"+
			"clipboard. t turns object detection on or off for the camera.\n\n"+
			"With a camera name, peep connects to that saved camera, asking\n"+
			"for its password unless one is saved; an unknown name starts a\n"+
			"wizard to add it. Without one, it lists the saved cameras.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(normalizeArgs(args)) // ExitOnError: exits on bad flags or -h

	if cfg.buffer < 0 || cfg.buffer > 10*time.Second {
		return cfg, fmt.Errorf("-buffer out of range: %v", cfg.buffer)
	}
	if fs.NArg() > 1 {
		return cfg, errors.New("at most one camera name is allowed")
	}
	cfg.camera = strings.TrimSpace(fs.Arg(0))
	return cfg, nil
}

// normalizeArgs lets the positional camera name appear before, between, or after
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

// buildURL renders rtsp://user:password@host:port/stream, escaping credentials
// and the stream path safely.
func buildURL(cfg config) string {
	u := url.URL{
		Scheme: "rtsp",
		Host:   net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		Path:   "/" + strings.TrimPrefix(cfg.stream, "/"),
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
	fullRange     bool // full ("pc"/JPEG) rather than limited colour range
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
		"-show_entries", "stream=width,height,avg_frame_rate,r_frame_rate,color_space,color_range,pix_fmt",
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
		case "color_range":
			info.fullRange = info.fullRange || val == "pc"
		case "pix_fmt":
			info.fullRange = info.fullRange || strings.HasPrefix(val, "yuvj")
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
	// scale_vaapi does not convert full-range video to limited range, so
	// such streams are decoded in software.
	vaapiOK := (cfg.hwaccel == "auto" || cfg.hwaccel == "vaapi") && !info.fullRange && vaapiAvailable(ctx)
	st, dec, err := startDecoding(ctx, input, decoders(cfg.hwaccel, geom, vaapiOK), geom.frameLen(), pool, ffmpegLog)
	if err != nil {
		return err
	}
	defer st.stop()
	if cfg.stats {
		fmt.Fprintf(os.Stderr, "peep: decoding with %s\n", dec.name)
	}

	var det *detector
	if cfg.detect {
		// Before the window opens, as the first run downloads the model.
		if det, err = newDetector(cfg.camera, geom, streamMatrix(info.colorSpace)); err != nil {
			fmt.Fprintf(os.Stderr, "peep: detection is off: %v\n", err)
		}
	}
	quitSDL, err := initSDL()
	if err != nil {
		return err
	}
	defer quitSDL()
	winW, winH := fitWindow(geom.width, geom.height, maxWindowW, maxWindowH)
	window, err := sdl.CreateWindow("peep - "+cfg.camera, winW, winH,
		sdl.WINDOW_RESIZABLE|sdl.WINDOW_HIGH_PIXEL_DENSITY)
	if err != nil {
		return fmt.Errorf("creating window: %w", err)
	}
	defer window.Destroy()

	p := &player{
		ctx:        ctx,
		st:         st,
		sched:      newScheduler(cfg.buffer, fps, pool-2, st.cap.release),
		geom:       geom,
		colorspace: streamColorspace(info.colorSpace),
		stats:      cfg.stats,
		det:        det,
		camera:     cfg.camera,
		matrix:     streamMatrix(info.colorSpace),
	}
	defer p.destroy()
	if err := p.newRenderer(window); err != nil {
		return err
	}
	err = p.loop()
	p.saving.Wait() // let screenshots in progress finish writing
	return err
}

// frameGeom describes the frames ffmpeg delivers.
type frameGeom struct {
	width, height int    // displayed picture size
	padW, padH    int    // NV12 plane size: width a multiple of 4, height even
	filter        string // ffmpeg -vf expression producing them
}

func (g frameGeom) frameLen() int { return g.padW * g.padH * 3 / 2 }

// vaapiFilter produces the same frames as filter from VA-API surfaces:
// scaling and range conversion run on the GPU, which then hands back NV12.
func (g frameGeom) vaapiFilter() string {
	f := fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=nv12:out_range=tv,hwdownload,format=nv12", g.width, g.height)
	if g.padW != g.width || g.padH != g.height {
		f += fmt.Sprintf(",pad=%d:%d", g.padW, g.padH)
	}
	return f
}

// planGeom works out the output geometry and ffmpeg filter for a w x h
// stream: downscale if too large, normalize to limited colour range (the
// texture is tagged as limited range), and pad to even dimensions, as NV12
// requires, with 4-byte aligned rows.
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
