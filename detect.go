package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

// The detection model is YOLOX-s (Apache-2.0), trained on the 80 COCO
// classes. It is downloaded on first use and checked against modelSHA256.
const (
	modelName   = "yolox_s.onnx"
	modelURL    = "https://github.com/Megvii-BaseDetection/YOLOX/releases/download/0.1.1rc0/yolox_s.onnx"
	modelSHA256 = "c5c2d13e59ae883e6af3b45daea64af4833a4951c92d116ec270d9ddbe998063"
	modelInput  = 640 // square input size, in pixels

	ortAPIVersion = 23 // ONNX Runtime 1.23 or newer
	// ortThreads is 1 because ONNX Runtime's worker threads spin while
	// idle and the binding cannot turn that off: 4 threads ran YOLOX-s in
	// ~70ms but used over 3 cores, 1 thread takes ~230ms on under 1 core.
	ortThreads = 1

	detectInterval = 250 * time.Millisecond // how often a frame is analyzed
	minScore       = 0.5                    // objectness × class confidence
	nmsIoU         = 0.45                   // overlap above which boxes are merged

	confirmRuns   = 2                // runs in a row an object must be seen before notifying
	goneAfter     = 30 * time.Second // absence after which a reappearance is new
	boxesShownFor = time.Second      // boxes stay up this long after the last run
	keepEventsFor = 7 * 24 * time.Hour
)

// cocoNames are the model's classes, in output order.
var cocoNames = [80]string{
	"person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck", "boat", "traffic light",
	"fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat", "dog", "horse", "sheep", "cow",
	"elephant", "bear", "zebra", "giraffe", "backpack", "umbrella", "handbag", "tie", "suitcase", "frisbee",
	"skis", "snowboard", "sports ball", "kite", "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket", "bottle",
	"wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
	"broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "couch", "potted plant", "bed",
	"dining table", "toilet", "tv", "laptop", "mouse", "remote", "keyboard", "cell phone", "microwave", "oven",
	"toaster", "sink", "refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier", "toothbrush",
}

// kind groups the classes peep reports. Other classes are ignored.
type kind int

const (
	kindNone kind = iota
	kindPerson
	kindVehicle
	kindAnimal
)

func classKind(class int) kind {
	switch cocoNames[class] {
	case "person":
		return kindPerson
	case "bicycle", "car", "motorcycle", "bus", "truck":
		return kindVehicle
	case "bird", "cat", "dog", "horse", "sheep", "cow", "bear":
		return kindAnimal
	}
	return kindNone
}

// kindColors are the box colours for each kind.
var kindColors = map[kind]color.RGBA{
	kindPerson:  {255, 64, 64, 255},
	kindVehicle: {64, 200, 255, 255},
	kindAnimal:  {255, 200, 0, 255},
}

// detection is one object found in a frame, in frame pixel coordinates.
type detection struct {
	class          int
	score          float32
	x0, y0, x1, y1 float32
}

func (d detection) label() string {
	return fmt.Sprintf("%s %.0f%%", cocoNames[d.class], d.score*100)
}

// detector analyzes frames in the background. Frames are handed over with
// submit, at most every detectInterval and never while one is in progress,
// so playback never waits for it.
type detector struct {
	camera string
	geom   frameGeom
	matrix yuvMatrix

	rt   *ort.Runtime
	env  *ort.Env
	sess *ort.Session

	frames     chan []byte // frames waiting to be analyzed
	free       chan []byte // spare frame buffers
	lastSubmit time.Time

	tensor []float32   // model input, BGR planes
	thumb  *image.RGBA // the frame at model resolution, for scaling
	scale  float32     // model pixels per frame pixel
	track  tracker

	mu     sync.Mutex
	latest []detection
	at     time.Time
	took   time.Duration

	done chan struct{}
}

// newDetector loads ONNX Runtime and the model, downloading it if needed.
func newDetector(camera string, geom frameGeom, matrix yuvMatrix) (*detector, error) {
	modelPath, err := ensureModel()
	if err != nil {
		return nil, err
	}
	rt, err := ort.NewRuntime("", ortAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("loading ONNX Runtime (is it installed?): %w", err)
	}
	d := &detector{camera: camera, geom: geom, matrix: matrix, rt: rt}
	if d.env, err = rt.NewEnv("peep", ort.LoggingLevelWarning); err != nil {
		d.close()
		return nil, err
	}
	if d.sess, err = rt.NewSession(d.env, modelPath, &ort.SessionOptions{IntraOpNumThreads: ortThreads}); err != nil {
		d.close()
		return nil, fmt.Errorf("loading %s: %w", modelPath, err)
	}

	d.scale = float32(math.Min(float64(modelInput)/float64(geom.width), float64(modelInput)/float64(geom.height)))
	tw := max(1, int(math.Round(float64(geom.width)*float64(d.scale))))
	th := max(1, int(math.Round(float64(geom.height)*float64(d.scale))))
	d.thumb = image.NewRGBA(image.Rect(0, 0, tw, th))
	d.tensor = make([]float32, 3*modelInput*modelInput)
	for i := range d.tensor {
		d.tensor[i] = 114 // YOLOX pads with grey
	}
	d.frames = make(chan []byte, 1)
	d.free = make(chan []byte, 1)
	d.free <- make([]byte, geom.frameLen())
	d.done = make(chan struct{})
	pruneEvents()
	go d.run()
	return d, nil
}

func (d *detector) close() {
	if d.frames != nil {
		close(d.frames)
		<-d.done
	}
	if d.sess != nil {
		d.sess.Close()
	}
	if d.env != nil {
		d.env.Close()
	}
	d.rt.Close()
}

// submit hands a copy of an NV12 frame to the detector if it is due for
// one and idle.
func (d *detector) submit(frame []byte, now time.Time) {
	if now.Sub(d.lastSubmit) < detectInterval {
		return
	}
	select {
	case buf := <-d.free:
		copy(buf, frame)
		d.frames <- buf
		d.lastSubmit = now
	default: // still busy with the previous frame
	}
}

// current returns the latest detections, or nil once they are stale.
func (d *detector) current(now time.Time) []detection {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.at) > boxesShownFor {
		return nil
	}
	return d.latest
}

// lastDuration is how long the most recent analysis took.
func (d *detector) lastDuration() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.took
}

func (d *detector) run() {
	defer close(d.done)
	for buf := range d.frames {
		start := time.Now()
		dets, err := d.analyze(buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peep: detection: %v\n", err)
		}
		now := time.Now()
		d.mu.Lock()
		d.latest, d.at, d.took = dets, now, now.Sub(start)
		d.mu.Unlock()
		for _, class := range d.track.update(dets, now) {
			d.announce(buf, class, dets, now)
		}
		d.free <- buf
	}
}

// analyze runs the model on an NV12 frame.
func (d *detector) analyze(frame []byte) ([]detection, error) {
	d.prepare(frame)
	in, err := ort.NewTensorValue(d.rt, d.tensor, []int64{1, 3, modelInput, modelInput})
	if err != nil {
		return nil, err
	}
	defer in.Close()
	outs, err := d.sess.Run(context.Background(), map[string]*ort.Value{d.sess.InputNames()[0]: in})
	if err != nil {
		return nil, err
	}
	for _, v := range outs {
		defer v.Close()
	}
	out, shape, err := ort.GetTensorData[float32](outs[d.sess.OutputNames()[0]])
	if err != nil {
		return nil, err
	}
	if len(shape) != 3 || shape[2] != 85 {
		return nil, fmt.Errorf("unexpected model output shape %v", shape)
	}
	return decodeYOLOX(out, modelInput, d.scale, d.geom.width, d.geom.height), nil
}

// prepare scales an NV12 frame down into d.thumb and the model's input
// tensor, averaging the luma under each output pixel.
func (d *detector) prepare(frame []byte) {
	g := d.geom
	tw, th := d.thumb.Rect.Dx(), d.thumb.Rect.Dy()
	plane := modelInput * modelInput
	uv := frame[g.padW*g.padH:]
	for y := range th {
		sy0, sy1 := y*g.height/th, max((y+1)*g.height/th, y*g.height/th+1)
		for x := range tw {
			sx0, sx1 := x*g.width/tw, max((x+1)*g.width/tw, x*g.width/tw+1)
			var sum, n int
			for sy := sy0; sy < sy1; sy++ {
				row := frame[sy*g.padW:]
				for sx := sx0; sx < sx1; sx++ {
					sum += int(row[sx])
				}
				n += sx1 - sx0
			}
			cx, cy := (sx0+sx1)/2/2, (sy0+sy1)/2/2
			c := uv[cy*g.padW+cx*2:]
			r, gg, b := d.matrix.rgb(uint8(sum/n), c[0], c[1])
			o := d.thumb.PixOffset(x, y)
			d.thumb.Pix[o], d.thumb.Pix[o+1], d.thumb.Pix[o+2], d.thumb.Pix[o+3] = r, gg, b, 255
			i := y*modelInput + x
			d.tensor[i], d.tensor[plane+i], d.tensor[2*plane+i] = float32(b), float32(gg), float32(r)
		}
	}
}

// decodeYOLOX turns YOLOX output rows (cx, cy, w, h, objectness, 80 class
// scores, relative to a grid cell) into reported detections in frame
// coordinates, merging overlapping boxes.
func decodeYOLOX(out []float32, size int, scale float32, frameW, frameH int) []detection {
	var cands []detection
	row := 0
	for _, stride := range []int{8, 16, 32} {
		cells := size / stride
		for gy := range cells {
			for gx := range cells {
				p := out[row*85 : (row+1)*85]
				row++
				best, class := float32(0), 0
				for c, s := range p[5:] {
					if s > best {
						best, class = s, c
					}
				}
				score := p[4] * best
				if score < minScore || classKind(class) == kindNone {
					continue
				}
				s := float32(stride) / scale
				cx, cy := (p[0]+float32(gx))*s, (p[1]+float32(gy))*s
				w := float32(math.Exp(float64(p[2]))) * s
				h := float32(math.Exp(float64(p[3]))) * s
				cands = append(cands, detection{
					class: class, score: score,
					x0: clampF(cx-w/2, 0, float32(frameW)), y0: clampF(cy-h/2, 0, float32(frameH)),
					x1: clampF(cx+w/2, 0, float32(frameW)), y1: clampF(cy+h/2, 0, float32(frameH)),
				})
			}
		}
	}
	return nms(cands, nmsIoU)
}

// nms keeps the best of each group of boxes overlapping by more than iou.
// Classes are ignored, so one pickup is not reported as a truck and a car.
func nms(cands []detection, iou float32) []detection {
	slices.SortFunc(cands, func(a, b detection) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	var kept []detection
	for _, c := range cands {
		if !slices.ContainsFunc(kept, func(k detection) bool { return overlap(k, c) > iou }) {
			kept = append(kept, c)
		}
	}
	return kept
}

// overlap is the intersection over union of two boxes.
func overlap(a, b detection) float32 {
	iw := min(a.x1, b.x1) - max(a.x0, b.x0)
	ih := min(a.y1, b.y1) - max(a.y0, b.y0)
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	return inter / ((a.x1-a.x0)*(a.y1-a.y0) + (b.x1-b.x0)*(b.y1-b.y0) - inter)
}

func clampF(v, lo, hi float32) float32 { return max(lo, min(v, hi)) }

// tracker decides when a class has newly appeared: seen in confirmRuns
// analyses in a row after being absent for goneAfter. Objects that stay in
// view, like a parked car, are announced once.
type tracker struct {
	classes map[int]*classState
}

type classState struct {
	streak   int
	lastSeen time.Time
	present  bool
}

// update records one analysis and returns the classes that just appeared.
func (t *tracker) update(dets []detection, now time.Time) []int {
	if t.classes == nil {
		t.classes = map[int]*classState{}
	}
	seen := map[int]bool{}
	for _, d := range dets {
		seen[d.class] = true
	}
	var appeared []int
	for class := range seen {
		st := t.classes[class]
		if st == nil {
			st = &classState{}
			t.classes[class] = st
		}
		st.streak++
		st.lastSeen = now
		if !st.present && st.streak >= confirmRuns {
			st.present = true
			appeared = append(appeared, class)
		}
	}
	for class, st := range t.classes {
		if seen[class] {
			continue
		}
		st.streak = 0
		if st.present && now.Sub(st.lastSeen) > goneAfter {
			st.present = false
		}
	}
	slices.Sort(appeared)
	return appeared
}

// announce saves the frame with its boxes and shows a notification that
// class was detected.
func (d *detector) announce(frame []byte, class int, dets []detection, now time.Time) {
	var best float32
	count := 0
	for _, det := range dets {
		if det.class == class {
			count++
			best = max(best, det.score)
		}
	}
	path, err := d.saveEvent(frame, class, dets, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: saving detection: %v\n", err)
		return
	}
	name := cocoNames[class]
	summary := strings.ToUpper(name[:1]) + name[1:] + " detected"
	if count > 1 {
		summary = fmt.Sprintf("%d × %s detected", count, name)
	}
	body := fmt.Sprintf("%s · %s · %.0f%% confident", d.camera, now.Format("15:04:05"), best*100)
	fmt.Fprintf(os.Stderr, "peep: %s (%s)\n", strings.ToLower(summary), path)
	err = notifyImage(notification{image: path, summary: summary, body: body, category: "device"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: detection notification: %v\n", err)
	}
}

// saveEvent writes the full frame, with boxes around the detections, as a
// JPEG in the events directory.
func (d *detector) saveEvent(frame []byte, class int, dets []detection, now time.Time) (string, error) {
	dir, err := eventsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	img := nv12ToRGBA(frame, d.geom, d.matrix)
	thick := max(2, d.geom.width/400)
	for _, det := range dets {
		drawBox(img, det, kindColors[classKind(det.class)], thick)
	}
	name := fmt.Sprintf("%s-%s-%s.jpg", now.Format("2006-01-02-15-04-05"), safeName(d.camera), strings.ReplaceAll(cocoNames[class], " ", "-"))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	err = jpeg.Encode(f, img, &jpeg.Options{Quality: 85})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// safeName makes a camera name safe for use in a file name.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == os.PathSeparator || r < ' ' {
			return '_'
		}
		return r
	}, s)
}

// drawBox outlines det on img.
func drawBox(img *image.RGBA, det detection, c color.RGBA, thick int) {
	r := image.Rect(int(det.x0), int(det.y0), int(det.x1), int(det.y1)).Intersect(img.Rect)
	for _, edge := range []image.Rectangle{
		image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+thick),
		image.Rect(r.Min.X, r.Max.Y-thick, r.Max.X, r.Max.Y),
		image.Rect(r.Min.X, r.Min.Y, r.Min.X+thick, r.Max.Y),
		image.Rect(r.Max.X-thick, r.Min.Y, r.Max.X, r.Max.Y),
	} {
		edge = edge.Intersect(img.Rect)
		for y := edge.Min.Y; y < edge.Max.Y; y++ {
			for x := edge.Min.X; x < edge.Max.X; x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// eventsDir holds the images saved for detections.
func eventsDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "peep", "detections"), nil
}

// pruneEvents deletes detection images older than keepEventsFor.
func pruneEvents() {
	dir, err := eventsDir()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > keepEventsFor {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// ensureModel returns the path to the detection model, downloading and
// verifying it on first use.
func ensureModel() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "peep", "models", modelName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "peep: downloading the detection model (36 MB) to %s\n", path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading model: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".model-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("downloading model: %w", err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != modelSHA256 {
		return "", errors.New("downloaded model failed its checksum; not using it")
	}
	return path, os.Rename(tmp.Name(), path)
}

// yuvMatrix converts limited-range YCbCr to RGB.
type yuvMatrix struct{ rv, gu, gv, bu float32 }

var (
	bt601 = yuvMatrix{1.402, 0.344136, 0.714136, 1.772}
	bt709 = yuvMatrix{1.5748, 0.187324, 0.468124, 1.8556}
)

// streamMatrix picks the matrix for ffprobe's color_space, as
// streamColorspace does for the renderer.
func streamMatrix(colorSpace string) yuvMatrix {
	switch colorSpace {
	case "smpte170m", "bt470bg", "fcc":
		return bt601
	}
	return bt709
}

func (m yuvMatrix) rgb(y, u, v uint8) (uint8, uint8, uint8) {
	yy := (float32(y) - 16) * (255.0 / 219)
	uu := (float32(u) - 128) * (255.0 / 224)
	vv := (float32(v) - 128) * (255.0 / 224)
	return clamp8(yy + m.rv*vv), clamp8(yy - m.gu*uu - m.gv*vv), clamp8(yy + m.bu*uu)
}

func clamp8(v float32) uint8 { return uint8(clampF(v+0.5, 0, 255)) }

// nv12ToRGBA converts a whole NV12 frame, without its padding.
func nv12ToRGBA(frame []byte, g frameGeom, m yuvMatrix) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, g.width, g.height))
	uv := frame[g.padW*g.padH:]
	for y := range g.height {
		row, crow := frame[y*g.padW:], uv[(y/2)*g.padW:]
		for x := range g.width {
			r, gg, b := m.rgb(row[x], crow[x&^1], crow[x|1])
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, gg, b, 255
		}
	}
	return img
}
