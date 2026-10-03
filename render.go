package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

// nv12Shader converts a packed NV12 frame to RGB on the GPU. ebiten images
// are RGBA only, so the frame's bytes are uploaded unchanged into an image
// stride/4 pixels wide: rows [0, LumaHeight) hold luma, four samples per
// pixel, and the LumaHeight/2 rows after them hold interleaved U,V pairs.
const nv12Shader = `//kage:unit pixels

package main

// LumaHeight is the number of luma rows in the packed image.
var LumaHeight float

// Coeffs holds the YCbCr->RGB matrix terms: R+=x*V, G-=y*U+z*V, B+=w*U.
var Coeffs vec4

func channel(t vec4, i float) float {
	if i < 0.5 {
		return t.r
	}
	if i < 1.5 {
		return t.g
	}
	if i < 2.5 {
		return t.b
	}
	return t.a
}

func Fragment(dstPos vec4, srcPos vec2, color vec4) vec4 {
	origin := imageSrc0Origin()
	p := floor(dstPos.xy - imageDstOrigin())

	lx := floor(p.x / 4)
	y := channel(imageSrc0UnsafeAt(origin+vec2(lx, p.y)+0.5), p.x-lx*4)

	cx := floor(p.x/2) * 2
	ux := floor(cx / 4)
	uv := imageSrc0UnsafeAt(origin + vec2(ux, LumaHeight+floor(p.y/2)) + 0.5)
	u := uv.r
	v := uv.g
	if cx-ux*4 > 1 {
		u = uv.b
		v = uv.a
	}

	// Limited ("tv") range; ffmpeg is told to normalize to it.
	yy := (y - 16.0/255.0) * (255.0 / 219.0)
	uu := (u - 128.0/255.0) * (255.0 / 224.0)
	vv := (v - 128.0/255.0) * (255.0 / 224.0)
	rgb := vec3(yy+Coeffs.x*vv, yy-Coeffs.y*uu-Coeffs.z*vv, yy+Coeffs.w*uu)
	return vec4(clamp(rgb, 0, 1), 1)
}
`

// Matrix coefficients for the shader's Coeffs uniform.
var (
	bt709Coeffs = []float32{1.5748, 0.187324, 0.468124, 1.8556}
	bt601Coeffs = []float32{1.402, 0.344136, 0.714136, 1.772}
)

// colorCoeffs picks the YCbCr matrix for ffprobe's color_space. Untagged
// streams are assumed BT.709, which is standard for HD and larger.
func colorCoeffs(colorSpace string) []float32 {
	switch colorSpace {
	case "smpte170m", "bt470bg", "fcc":
		return bt601Coeffs
	}
	return bt709Coeffs
}

type game struct {
	ctx    context.Context
	st     *streamer
	sched  *scheduler
	geom   frameGeom
	coeffs []float32
	stats  bool

	packed   *ebiten.Image
	shader   *ebiten.Shader
	uniforms map[string]any
	vertices []ebiten.Vertex
	incoming []*frame
	hasFrame bool

	statsAt    time.Time
	statsShown uint64
	statsRecv  uint64
}

func (g *game) init() error {
	if max := ebiten.MaxImageSize(); max > 0 && (g.geom.width > max || g.geom.height > max) {
		return fmt.Errorf("video %dx%d exceeds the maximum texture size %d", g.geom.width, g.geom.height, max)
	}
	shader, err := ebiten.NewShader([]byte(nv12Shader))
	if err != nil {
		return fmt.Errorf("compiling shader: %w", err)
	}
	g.shader = shader
	pw, ph := g.geom.packedSize()
	g.packed = ebiten.NewImageWithOptions(image.Rect(0, 0, pw, ph), &ebiten.NewImageOptions{Unmanaged: true})
	g.uniforms = map[string]any{"LumaHeight": float32(g.geom.padH), "Coeffs": g.coeffs}
	w, h := float32(g.geom.width), float32(g.geom.height)
	sw, sh := float32(pw), float32(ph)
	g.vertices = []ebiten.Vertex{
		{DstX: 0, DstY: 0, SrcX: 0, SrcY: 0, ColorR: 1, ColorG: 1, ColorB: 1, ColorA: 1},
		{DstX: w, DstY: 0, SrcX: sw, SrcY: 0, ColorR: 1, ColorG: 1, ColorB: 1, ColorA: 1},
		{DstX: 0, DstY: h, SrcX: 0, SrcY: sh, ColorR: 1, ColorG: 1, ColorB: 1, ColorA: 1},
		{DstX: w, DstY: h, SrcX: sw, SrcY: sh, ColorR: 1, ColorG: 1, ColorB: 1, ColorA: 1},
	}
	return nil
}

func (g *game) Update() error {
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) || g.ctx.Err() != nil {
		return ebiten.Termination
	}
	if err := g.st.cap.Err(); err != nil {
		return err
	}
	if g.packed == nil {
		if err := g.init(); err != nil {
			return err
		}
	}
	g.incoming = g.st.cap.take(g.incoming[:0])
	for i, f := range g.incoming {
		g.sched.add(f)
		g.incoming[i] = nil
	}
	now := time.Now()
	if f := g.sched.next(now); f != nil {
		g.packed.WritePixels(f.buf) // copies, so the buffer can be reused
		g.st.cap.release(f.buf)
		g.hasFrame = true
	}
	if g.stats {
		g.printStats(now)
	}
	return nil
}

func (g *game) printStats(now time.Time) {
	if g.statsAt.IsZero() {
		g.statsAt = now
		return
	}
	elapsed := now.Sub(g.statsAt).Seconds()
	if elapsed < 1 {
		return
	}
	recv := g.st.cap.Received()
	s := g.sched
	fmt.Fprintf(os.Stderr, "recv %.1f fps, shown %.1f fps, cadence %.2f fps, queued %d, late %d, dropped %d, skips %d, latency %.0fms, tps %.0f\n",
		float64(recv-g.statsRecv)/elapsed, float64(s.shown-g.statsShown)/elapsed, float64(time.Second)/float64(s.interval),
		len(s.pending), s.late, s.dropped, s.reanchors, s.slack*1000, ebiten.ActualTPS())
	g.statsAt, g.statsRecv, g.statsShown = now, recv, s.shown
}

func (g *game) Draw(screen *ebiten.Image) {
	if !g.hasFrame {
		return
	}
	screen.DrawTrianglesShader(g.vertices, []uint16{0, 1, 2, 1, 2, 3}, g.shader, &ebiten.DrawTrianglesShaderOptions{
		Uniforms: g.uniforms,
		Images:   [4]*ebiten.Image{g.packed},
	})
}

func (g *game) Layout(_, _ int) (int, int) {
	return g.geom.width, g.geom.height
}
