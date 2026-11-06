//go:build !darwin || !cgo

package graphics

import (
	"context"
	"errors"
	"image"
	"image/draw"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/graphics/feed"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

// The calls of SWVideoCalls.m and SWFindCalls.m on recordings, answered here rather than by
// internal/graphics/raster.
var videoCalls = map[string]func(g *Graphics, ctx context.Context, env environment, it *prepared, arguments raster.Arguments) (any, error){
	"record":         (*Graphics).callRecord,
	"recording.stop": (*Graphics).callRecordingStop,
	"video.open":     (*Graphics).callVideoOpen,
	"video.grid":     (*Graphics).callVideoGrid,
	"video.diff":     (*Graphics).callVideoDiff,
	"video.find":     (*Graphics).callVideoFind,
	"video.export":   (*Graphics).callVideoExport,
}

func typeError(message ...any) *raster.Error {
	return &raster.Error{Name: raster.NameType, Message: E.New(message...).Error()}
}

func pathArgument(arguments raster.Arguments) (string, error) {
	path, ok := arguments.String("path")
	if !ok {
		return "", typeError("graphics: the call needs a path")
	}
	return path, nil
}

// reportRecording logs the failure of a recording that was let go of, and tells the app.
func (g *Graphics) reportRecording(message string) {
	g.options.Logger.Error("save the abandoned recording: ", message)
	if g.options.OnRecordingFailed != nil {
		g.options.OnRecordingFailed(message)
	}
}

func (g *Graphics) callRecord(ctx context.Context, env environment, _ *prepared, arguments raster.Arguments) (any, error) {
	path, err := pathArgument(arguments)
	if err != nil {
		return nil, err
	}
	if err = env.Authorize(path, true); err != nil {
		return nil, err
	}
	if g.feed == nil {
		if err = feed.Available(); err != nil {
			return nil, err
		}
		return nil, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: there is no device screen to record"}
	}
	session, err := startRecording(ctx, g.feed, path, env.call.WatchTimeline(), g.reportRecording)
	if err != nil {
		return nil, err
	}
	env.engine.addRecording(session)
	return map[string]int64{"recording": env.engine.keep(session)}, nil
}

func (g *Graphics) callRecordingStop(ctx context.Context, env environment, it *prepared, _ raster.Arguments) (any, error) {
	var session *recordingSession
	if len(it.objects) > 0 {
		session, _ = it.objects[0].(*recordingSession)
	}
	if session == nil {
		return nil, typeError("stop needs a Recording")
	}
	model, err := session.stop(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]int64{"video": env.engine.keep(model)}, nil
}

func (g *Graphics) callVideoOpen(ctx context.Context, env environment, _ *prepared, arguments raster.Arguments) (any, error) {
	path, err := pathArgument(arguments)
	if err != nil {
		return nil, err
	}
	if err = env.Authorize(path, false); err != nil {
		return nil, err
	}
	if _, err = os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, &raster.Error{Name: raster.NameNotFound, Message: "there is no file at " + path}
	}
	model, err := openVideo(ctx, path, func(message string) {
		g.options.Logger.Error(message)
	})
	if err != nil {
		return nil, err
	}
	return map[string]int64{"video": env.engine.keep(model)}, nil
}

func videoOf(it *prepared) (*videoModel, error) {
	var model *videoModel
	if len(it.objects) > 0 {
		model, _ = it.objects[0].(*videoModel)
	}
	if model == nil {
		return nil, typeError("graphics: ", it.method, " needs a Video")
	}
	return model, nil
}

func (g *Graphics) callVideoGrid(ctx context.Context, env environment, it *prepared, arguments raster.Arguments) (any, error) {
	model, err := videoOf(it)
	if err != nil {
		return nil, err
	}
	times, ok := arguments.Numbers("times")
	if !ok {
		times = model.defaultGridTimes()
	}
	grid, err := raster.VideoGrid(ctx, times, arguments.Integer("columns", 4), arguments.Integer("width", 240), model.pixelsAt)
	if err != nil {
		return nil, err
	}
	return raster.ImageResult{Image: env.engine.keep(grid)}, nil
}

func (g *Graphics) callVideoDiff(ctx context.Context, _ environment, it *prepared, arguments raster.Arguments) (any, error) {
	model, err := videoOf(it)
	if err != nil {
		return nil, err
	}
	return raster.Difference(ctx, model.nodeAt(arguments.Float("from", 0)), model.nodeAt(arguments.Float("to", 0)), arguments.Integer("threshold", raster.DefaultDifferenceThreshold))
}

// videoMatch is a match of screen.find with the time into the recording of its frame.
type videoMatch struct {
	raster.ScreenMatch
	Time float64 `json:"time"`
}

func (g *Graphics) callVideoFind(ctx context.Context, env environment, it *prepared, arguments raster.Arguments) (any, error) {
	started := raster.NowMilliseconds()
	model, err := videoOf(it)
	if err != nil {
		return nil, err
	}
	search, err := raster.NewCallSearch(it.method, it.arguments, it.objects, 1, env)
	if err != nil {
		return nil, err
	}
	var result *videoMatch
	var found *raster.Hit
	err = model.scan(ctx, arguments.Float("from", 0), arguments.Float("to", model.duration), func(pixels *image.NRGBA, time float64) (bool, error) {
		frame := raster.FromFrame(pixels, model.scale, time, model.start+time)
		hit, err := search.Check(ctx, frame)
		if err != nil || hit == nil {
			return err == nil, err
		}
		found = hit
		result = &videoMatch{ScreenMatch: hit.Match(env.engine.keep(frame)), Time: raster.Rounded(time)}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	search.Trace("recording.find", started, found, math.NaN())
	if result == nil {
		return nil, nil
	}
	return result, nil
}

func (g *Graphics) callVideoExport(ctx context.Context, env environment, it *prepared, arguments raster.Arguments) (any, error) {
	model, err := videoOf(it)
	if err != nil {
		return nil, err
	}
	path, ok := arguments.String("path")
	if !ok {
		path = strings.TrimSuffix(model.path, filepath.Ext(model.path)) + "-export.mp4"
	}
	if err = env.Authorize(path, true); err != nil {
		return nil, err
	}
	if err = exportVideo(ctx, model, arguments, path); err != nil {
		return nil, err
	}
	return map[string]string{"path": path}, nil
}

// exportVideo writes the frames of a stretch of the recording at a frame rate, cropped to a
// region and scaled, as an H.264 MPEG-4 movie, an HEVC one for mov, or a GIF (SWVideoExport.m).
func exportVideo(ctx context.Context, model *videoModel, arguments raster.Arguments, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return E.Cause(err, "graphics: create the directory of ", target)
	}
	os.Remove(target)
	format, ok := arguments.String("format")
	if !ok {
		format = "mp4"
	}
	gif := format == "gif"
	from := arguments.Float("from", 0)
	to := arguments.Float("to", model.duration)
	rate := arguments.Float("frameRate", map[bool]float64{true: 10, false: 30}[gif])
	factor := arguments.Float("scale", 1)
	region, hasRegion := arguments.Rect("region")
	step := 1000 / rate
	var times []float64
	for index := 0; step > 0 && !math.IsInf(step, 0) && from+float64(index)*step <= to; index++ {
		times = append(times, from+float64(index)*step)
	}
	if len(times) == 0 {
		return &raster.Error{Name: raster.NameRange, Message: "graphics: the range has no frames"}
	}
	frame := func(time float64) (*image.NRGBA, error) {
		node := model.nodeAt(time)
		if hasRegion {
			cropped, err := node.Crop(region)
			if err != nil {
				return nil, err
			}
			node = cropped
		}
		width := max(int(math.Round(float64(node.PixelWidth())*factor)), 2)
		height := max(int(math.Round(float64(node.PixelHeight())*factor)), 2)
		if !gif {
			width, height = width&^1, height&^1
		}
		pixels, err := node.Resize(width, height).Pixels(ctx)
		if err != nil {
			return nil, err
		}
		return colorPixels(pixels), nil
	}
	var err error
	if gif {
		err = exportGIF(ctx, target, times, step, frame)
	} else {
		err = exportMovie(ctx, target, format == "mov", times, from, rate, frame)
	}
	if err != nil {
		os.Remove(target)
	}
	return err
}

func colorPixels(pixels image.Image) *image.NRGBA {
	if converted, isColor := pixels.(*image.NRGBA); isColor {
		return converted
	}
	converted := image.NewNRGBA(pixels.Bounds())
	draw.Draw(converted, converted.Rect, pixels, pixels.Bounds().Min, draw.Src)
	return converted
}

func aborted(ctx context.Context) error {
	if ctx.Err() != nil {
		return &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	return nil
}

func exportGIF(ctx context.Context, target string, times []float64, step float64, frame func(float64) (*image.NRGBA, error)) error {
	file, err := os.Create(target)
	if err != nil {
		return E.Cause(err, "graphics: create the GIF")
	}
	defer file.Close()
	writer := newGIFWriter(file)
	delay := int(math.Round(step / 10))
	for _, time := range times {
		if err = aborted(ctx); err != nil {
			return err
		}
		pixels, err := frame(time)
		if err != nil {
			return err
		}
		if err = writer.frame(pixels, delay); err != nil {
			return E.Cause(err, "graphics: write the GIF")
		}
	}
	if err = writer.close(); err != nil {
		return E.Cause(err, "graphics: write the GIF")
	}
	return file.Close()
}

func exportMovie(ctx context.Context, target string, hevc bool, times []float64, from, rate float64, frame func(float64) (*image.NRGBA, error)) error {
	first, err := frame(times[0])
	if err != nil {
		return err
	}
	encoder, err := feed.NewEncoder(feed.EncoderOptions{Path: target, HEVC: hevc, Width: first.Rect.Dx(), Height: first.Rect.Dy(), FrameRate: rate})
	if err != nil {
		return err
	}
	milliseconds := func(index int) time.Duration {
		return time.Duration(math.Round(times[index]-from)) * time.Millisecond
	}
	for index := range times {
		if err = aborted(ctx); err != nil {
			encoder.Close()
			return err
		}
		pixels := first
		if index > 0 {
			if pixels, err = frame(times[index]); err != nil {
				encoder.Close()
				return err
			}
		}
		at := milliseconds(index)
		duration := time.Duration(math.Round(1000/rate)) * time.Millisecond
		if index+1 < len(times) {
			duration = milliseconds(index+1) - at
		}
		if err = encoder.Write(pixels, at, duration); err != nil {
			encoder.Close()
			return E.Cause(err, "graphics: write the video")
		}
	}
	if err = encoder.Finish(); err != nil {
		return E.Cause(err, "graphics: finish the video")
	}
	return nil
}
