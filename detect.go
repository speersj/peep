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
	"io/fs"
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

const (
	modelInput = 640 // square input size of every model, in pixels

	ortAPIVersion = 23 // ONNX Runtime 1.23 or newer
	// ortThreads is 1 because ONNX Runtime's worker threads spin while
	// idle and the binding cannot turn that off: 4 threads ran YOLOX-s in
	// ~70ms but used over 3 cores, 1 thread takes ~230ms on under 1 core.
	ortThreads = 1

	detectInterval = 250 * time.Millisecond // how often a frame is analyzed
	nmsIoU         = 0.45                   // overlap above which boxes are merged

	confirmRuns   = 2                // runs in a row an object must be seen before notifying
	goneAfter     = 30 * time.Second // absence after which a reappearance is new
	boxesShownFor = time.Second      // boxes stay up this long after the last run

	// Saved detection images are deleted after keepEventsFor, and the
	// oldest pruneFraction of them while they total more than maxEventBytes.
	// Pruning runs at startup and every pruneEvery.
	keepEventsFor = 7 * 24 * time.Hour
	maxEventBytes = 1 << 30
	pruneFraction = 0.1
	pruneEvery    = 10 * time.Minute
)

// model is a detection model peep can run. All are trained on the 80 COCO
// classes, take a modelInput-square image and are downloaded on first use,
// checked against sha256.
type model struct {
	id       string // in settings and file names
	title    string // for people
	file     string // name in the models directory
	url      string
	sha256   string
	sizeMB   int
	stretch  bool    // input is the whole frame stretched square, RGB 0-1; else letterboxed BGR 0-255
	minScore float32 // confidence below which detections are dropped
	decode   func(d *detector, outs map[string]*ort.Value) ([]detection, error)
}

// models are the detection models, in the order t cycles through them.
var models = []model{
	{
		// YOLOX-s (Apache-2.0): fast, good in daylight.
		id: "yolox-s", title: "YOLOX-s", file: "yolox_s.onnx",
		url:    "https://github.com/Megvii-BaseDetection/YOLOX/releases/download/0.1.1rc0/yolox_s.onnx",
		sha256: "c5c2d13e59ae883e6af3b45daea64af4833a4951c92d116ec270d9ddbe998063",
		sizeMB: 36, minScore: 0.5, decode: (*detector).decodeYOLOX,
	},
	{
		// D-FINE-S (Apache-2.0), pretrained on Objects365 then trained on
		// COCO: about 10 AP more accurate than YOLOX-s for similar work.
		// Exported to ONNX by Hugging Face's onnx-community, pinned to a
		// revision.
		id: "dfine-s", title: "D-FINE-S", file: "dfine_s_obj2coco.onnx",
		url:    "https://huggingface.co/onnx-community/dfine_s_obj2coco-ONNX/resolve/f69c4ca98cba7ca58aa15b3d4600867808fecf1b/onnx/model.onnx",
		sha256: "b9e2e76610053aeeac3b2f1f685d8f9a1182a93a338f624b6c8cb7fb390cb532",
		sizeMB: 42, stretch: true, minScore: 0.5, decode: (*detector).decodeDFINE,
	},
}

// findModel returns the model with the given id.
func findModel(id string) (model, bool) {
	for _, m := range models {
		if m.id == id {
			return m, true
		}
	}
	return model{}, false
}

// nextModel is the model after id in models, or "" (off) after the last.
// Off, or an unknown id, is followed by the first.
func nextModel(id string) string {
	for i, m := range models {
		if m.id == id {
			if i+1 < len(models) {
				return models[i+1].id
			}
			return ""
		}
	}
	return models[0].id
}

// modelTitle names the model with the given id for people; "" is "off".
func modelTitle(id string) string {
	if m, ok := findModel(id); ok {
		return m.title
	}
	if id == "" {
		return "off"
	}
	return id
}

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
	model  model
	camera string
	geom   frameGeom
	matrix yuvMatrix

	rt   *ort.Runtime
	env  *ort.Env
	sess *ort.Session

	frames     chan []byte // frames waiting to be analyzed
	free       chan []byte // spare frame buffers
	lastSubmit time.Time

	tensor   []float32 // model input planes
	inW, inH int       // the part of the input the frame is scaled into
	track    tracker

	mu     sync.Mutex
	latest []detection
	at     time.Time
	took   time.Duration

	done chan struct{}
}

// newDetector loads ONNX Runtime and the model with id modelID,
// downloading it if needed.
func newDetector(modelID, camera string, geom frameGeom, matrix yuvMatrix) (*detector, error) {
	mod, ok := findModel(modelID)
	if !ok {
		return nil, fmt.Errorf("unknown detection model %q", modelID)
	}
	modelPath, err := ensureModel(mod)
	if err != nil {
		return nil, err
	}
	rt, err := ort.NewRuntime("", ortAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("loading ONNX Runtime (is it installed?): %w", err)
	}
	d := &detector{model: mod, camera: camera, geom: geom, matrix: matrix, rt: rt}
	if d.env, err = rt.NewEnv("peep", ort.LoggingLevelWarning); err != nil {
		d.close()
		return nil, err
	}
	if d.sess, err = rt.NewSession(d.env, modelPath, &ort.SessionOptions{IntraOpNumThreads: ortThreads}); err != nil {
		d.close()
		return nil, fmt.Errorf("loading %s: %w", modelPath, err)
	}

	d.inW, d.inH = modelInput, modelInput
	d.tensor = make([]float32, 3*modelInput*modelInput)
	if !mod.stretch {
		scale := d.letterboxScale()
		d.inW = max(1, int(math.Round(float64(geom.width)*float64(scale))))
		d.inH = max(1, int(math.Round(float64(geom.height)*float64(scale))))
		for i := range d.tensor {
			d.tensor[i] = 114 // YOLOX pads with grey
		}
	}
	d.frames = make(chan []byte, 1)
	d.free = make(chan []byte, 1)
	d.free <- make([]byte, geom.frameLen())
	d.done = make(chan struct{})
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

// letterboxScale is model pixels per frame pixel when the frame keeps its
// shape inside the model input.
func (d *detector) letterboxScale() float32 {
	return float32(math.Min(float64(modelInput)/float64(d.geom.width), float64(modelInput)/float64(d.geom.height)))
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
	return d.model.decode(d, outs)
}

// output fetches the model's output called name, checking it has the
// given number of values in its last dimension.
func (d *detector) output(outs map[string]*ort.Value, name string, last int64) ([]float32, error) {
	v, ok := outs[name]
	if !ok {
		return nil, fmt.Errorf("model has no output %q (has %v)", name, d.sess.OutputNames())
	}
	out, shape, err := ort.GetTensorData[float32](v)
	if err != nil {
		return nil, err
	}
	if len(shape) != 3 || shape[2] != last {
		return nil, fmt.Errorf("unexpected shape %v for model output %q", shape, name)
	}
	return out, nil
}

func (d *detector) decodeYOLOX(outs map[string]*ort.Value) ([]detection, error) {
	out, err := d.output(outs, d.sess.OutputNames()[0], 85)
	if err != nil {
		return nil, err
	}
	return decodeYOLOX(out, modelInput, d.letterboxScale(), d.model.minScore, d.geom.width, d.geom.height), nil
}

func (d *detector) decodeDFINE(outs map[string]*ort.Value) ([]detection, error) {
	logits, err := d.output(outs, "logits", int64(len(cocoNames)))
	if err != nil {
		return nil, err
	}
	boxes, err := d.output(outs, "pred_boxes", 4)
	if err != nil {
		return nil, err
	}
	return decodeDFINE(logits, boxes, d.model.minScore, d.geom.width, d.geom.height), nil
}

// prepare scales an NV12 frame into the top left d.inW x d.inH of the
// model's input tensor, averaging the luma under each output pixel.
func (d *detector) prepare(frame []byte) {
	g := d.geom
	tw, th := d.inW, d.inH
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
			i := y*modelInput + x
			if d.model.stretch {
				d.tensor[i], d.tensor[plane+i], d.tensor[2*plane+i] = float32(r)/255, float32(gg)/255, float32(b)/255
			} else {
				d.tensor[i], d.tensor[plane+i], d.tensor[2*plane+i] = float32(b), float32(gg), float32(r)
			}
		}
	}
}

// decodeYOLOX turns YOLOX output rows (cx, cy, w, h, objectness, 80 class
// scores, relative to a grid cell) into reported detections in frame
// coordinates, merging overlapping boxes.
func decodeYOLOX(out []float32, size int, scale, minScore float32, frameW, frameH int) []detection {
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

// decodeDFINE turns D-FINE's queries, each with 80 class logits and a box
// (cx, cy, w, h as fractions of the stretched frame), into reported
// detections in frame coordinates. D-FINE does not produce duplicates of
// one class, but nms still stops one pickup being both a car and a truck.
func decodeDFINE(logits, boxes []float32, minScore float32, frameW, frameH int) []detection {
	n := len(cocoNames)
	var cands []detection
	for q := range len(boxes) / 4 {
		best, class := float32(math.Inf(-1)), 0
		for c, l := range logits[q*n : (q+1)*n] {
			if l > best {
				best, class = l, c
			}
		}
		score := float32(1 / (1 + math.Exp(-float64(best))))
		if score < minScore || classKind(class) == kindNone {
			continue
		}
		b := boxes[q*4 : q*4+4]
		fw, fh := float32(frameW), float32(frameH)
		cx, cy, w, h := b[0]*fw, b[1]*fh, b[2]*fw, b[3]*fh
		cands = append(cands, detection{
			class: class, score: score,
			x0: clampF(cx-w/2, 0, fw), y0: clampF(cy-h/2, 0, fh),
			x1: clampF(cx+w/2, 0, fw), y1: clampF(cy+h/2, 0, fh),
		})
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
	body := fmt.Sprintf("%s · %s · %.0f%% confident (%s)", d.camera, now.Format("15:04:05"), best*100, d.model.title)
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
	name := fmt.Sprintf("%s-%s-%s-%s.jpg", now.Format("2006-01-02-15-04-05"), safeName(d.camera), d.model.id, strings.ReplaceAll(cocoNames[class], " ", "-"))
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

// pruneEventsPeriodically prunes the detection images now and then every
// pruneEvery until ctx is done.
func pruneEventsPeriodically(ctx context.Context) {
	dir, err := eventsDir()
	if err != nil {
		return
	}
	tick := time.NewTicker(pruneEvery)
	defer tick.Stop()
	for {
		if _, err := pruneEvents(dir, keepEventsFor, maxEventBytes, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "peep: pruning %s: %v\n", dir, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pruneEvents deletes files in dir older than maxAge, then, while the rest
// total more than maxBytes, the oldest pruneFraction of them (at least one).
// It returns how many files it deleted. A missing dir is not an error.
func pruneEvents(dir string, maxAge time.Duration, maxBytes int64, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	deleted := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f := file{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()}
		if now.Sub(f.mod) > maxAge {
			if os.Remove(f.path) == nil {
				deleted++
			}
			continue
		}
		files = append(files, f)
		total += f.size
	}
	slices.SortFunc(files, func(a, b file) int { return a.mod.Compare(b.mod) })
	for total > maxBytes && len(files) > 0 {
		n := max(1, int(float64(len(files))*pruneFraction))
		for _, f := range files[:n] {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return deleted, err
			}
			deleted++
			total -= f.size
		}
		files = files[n:]
	}
	return deleted, nil
}

// ensureModel returns the path to m, downloading and verifying it on first
// use.
func ensureModel(m model) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "peep", "models", m.file)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "peep: downloading the %s detection model (%d MB) to %s\n", m.title, m.sizeMB, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
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
	if sum := hex.EncodeToString(h.Sum(nil)); sum != m.sha256 {
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
