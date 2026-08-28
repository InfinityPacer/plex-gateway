package mediainfo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFFProbePreservesPlexRelevantFields(t *testing.T) {
	body := []byte(`{
  "streams": [
    {
      "index": 0,
      "codec_name": "hevc",
      "codec_long_name": "H.265 / HEVC",
      "profile": "Main 10",
      "codec_type": "video",
      "codec_tag_string": "[0][0][0][0]",
      "width": 3840,
      "height": 2160,
      "sample_aspect_ratio": "1:1",
      "display_aspect_ratio": "16:9",
      "pix_fmt": "yuv420p10le",
      "level": 153,
      "color_range": "tv",
      "color_space": "bt2020nc",
      "color_transfer": "smpte2084",
      "color_primaries": "bt2020",
      "chroma_location": "topleft",
      "r_frame_rate": "24000/1001",
      "avg_frame_rate": "24000/1001",
      "time_base": "1/1000",
      "duration": "7200.125",
      "bit_rate": "45000000",
      "bits_per_raw_sample": "10",
      "disposition": {"default": 1},
      "side_data_list": [
        {"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,"dv_profile":8,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1},
        {"side_data_type":"Content light level metadata","max_content":1000,"max_average":400}
      ]
    },
    {
      "index": 1,
      "codec_name": "truehd",
      "codec_long_name": "TrueHD",
      "profile": "Dolby TrueHD + Dolby Atmos",
      "codec_type": "audio",
      "sample_fmt": "s32",
      "sample_rate": "48000",
      "channels": 8,
      "channel_layout": "7.1",
      "bit_rate": "5000000",
      "tags": {"language":"eng","title":"Dolby Atmos"},
      "disposition": {"default": 1}
    },
    {
      "index": 2,
      "codec_name": "hdmv_pgs_subtitle",
      "codec_type": "subtitle",
      "tags": {"language":"zho","title":"Chinese"},
      "disposition": {"forced": 1}
    }
  ],
  "format": {
    "format_name":"matroska,webm",
    "format_long_name":"Matroska / WebM",
    "start_time":"0.000000",
    "duration":"7200.125000",
    "size":"42949672960",
    "bit_rate":"47721858"
  }
}`)

	media, err := parseFFProbe(body)
	if err != nil {
		t.Fatal(err)
	}
	if !media.Complete || media.Container != "mkv" || media.DurationMS != 7_200_125 || media.Size != 42_949_672_960 {
		t.Fatalf("media summary = %#v", media)
	}
	if media.VideoCodec != "hevc" || media.VideoResolution != "4k" || media.Width != 3840 || media.Height != 2160 {
		t.Fatalf("video summary = %#v", media)
	}
	video := media.Streams[0]
	if video.BitDepth != 10 || video.HDRFormat != "dolby_vision" || video.DolbyVision == nil || video.DolbyVision.Profile != 8 || video.ContentLightLevel == nil || video.ContentLightLevel.MaxContent != 1000 {
		t.Fatalf("video stream = %#v", video)
	}
	audio := media.Streams[1]
	if !audio.Atmos || audio.Channels != 8 || audio.Language != "eng" || !audio.Disposition.Default {
		t.Fatalf("audio stream = %#v", audio)
	}
	subtitle := media.Streams[2]
	if subtitle.Language != "zho" || !subtitle.Disposition.Forced {
		t.Fatalf("subtitle stream = %#v", subtitle)
	}
}

func TestParseFFProbeDoesNotInferCodecFieldsFromDolbyVisionProfile(t *testing.T) {
	body := []byte(`{
  "streams": [
    {
      "index": 0,
      "codec_name": "hevc",
      "codec_type": "video",
      "width": 3840,
      "height": 2160,
      "side_data_list": [
        {"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,"dv_profile":5,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":0}
      ]
    }
  ],
  "format":{"format_name":"matroska","duration":"60"}
}`)

	media, err := parseFFProbe(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Streams) != 1 {
		t.Fatalf("streams = %#v", media.Streams)
	}
	video := media.Streams[0]
	if video.DolbyVision == nil || video.DolbyVision.Profile != 5 || video.HDRFormat != "dolby_vision" {
		t.Fatalf("Dolby Vision fields = %#v", video)
	}
	if video.Profile != "" || video.BitDepth != 0 || video.PixelFormat != "" ||
		video.ColorSpace != "" || video.ColorRange != "" ||
		video.ColorPrimaries != "" || video.ColorTransfer != "" {
		t.Fatalf("codec or color fields were inferred from Dolby Vision profile = %#v", video)
	}
}

func TestParseFFProbeInfersBitDepthFromPixelFormat(t *testing.T) {
	body := []byte(`{
  "streams": [
    {"index":0,"codec_name":"hevc","codec_type":"video","width":3840,"height":2160,"pix_fmt":"yuv420p10le"},
    {"index":1,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080,"pix_fmt":"p010le"},
    {"index":2,"codec_name":"h264","codec_type":"video","width":1280,"height":720,"pix_fmt":"yuv420p"}
  ],
  "format":{"format_name":"matroska","duration":"60"}
}`)

	media, err := parseFFProbe(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Streams) != 3 || media.Streams[0].BitDepth != 10 || media.Streams[1].BitDepth != 10 || media.Streams[2].BitDepth != 8 {
		t.Fatalf("inferred bit depths = %#v", media.Streams)
	}
}

func TestParseFFProbeRejectsIncompleteVideo(t *testing.T) {
	_, err := parseFFProbe([]byte(`{
      "streams":[{"index":0,"codec_type":"video","codec_name":"hevc"}],
      "format":{"format_name":"matroska","duration":"60"}
    }`))
	if !errors.Is(err, ErrProbeIncomplete) {
		t.Fatalf("parseFFProbe() error = %v", err)
	}
}

func TestFFProberRejectsCredentialedTarget(t *testing.T) {
	prober := &FFProber{binary: "ffprobe", timeout: defaultProbeTimeout, probeSize: defaultProbeSize, analyzeDuration: defaultAnalyzeDuration, outputLimit: defaultProbeOutputLimit}
	_, err := prober.Probe(t.Context(), "https://user:secret@example.test/movie.mkv", "Infuse-Library/8.4.4")
	if !errors.Is(err, ErrProbeTarget) {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestFFProberAgainstHTTPMedia(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}

	directory := t.TempDir()
	mediaPath := filepath.Join(directory, "sample.mkv")
	command := exec.CommandContext(t.Context(), ffmpeg,
		"-v", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=24:d=1",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=1",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", mediaPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v: %s", err, output)
	}

	server := httptest.NewServer(http.FileServer(http.Dir(directory)))
	defer server.Close()
	prober, err := NewFFProber(FFProbeOptions{
		Binary: ffprobe, Timeout: 5 * time.Second, ProbeSize: 2 << 20,
		AnalyzeDuration: time.Second, OutputLimit: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	media, err := prober.Probe(t.Context(), server.URL+"/sample.mkv", "Infuse-Library/8.4.4")
	if err != nil {
		t.Fatal(err)
	}
	if !media.Complete || media.Container != "mkv" || media.DurationMS <= 0 {
		t.Fatalf("media summary = %#v", media)
	}
	if media.VideoCodec != "mpeg4" || media.Width != 320 || media.Height != 180 || media.AudioCodec != "aac" {
		t.Fatalf("stream summary = %#v", media)
	}
}

func TestFFProberFillsMissingSizeFromContentRange(t *testing.T) {
	transport := &rangeProbeTransport{status: http.StatusPartialContent, contentRange: "bytes 0-0/123456"}
	prober := newOutputScriptProber(t, completeFFProbeJSONWithoutSize, &http.Client{Transport: transport})

	media, err := prober.Probe(t.Context(), "https://cdn.example.test/movie.mkv", "Infuse-Library/8.4.4")
	if err != nil {
		t.Fatal(err)
	}
	if media.Size != 123456 {
		t.Fatalf("media size = %d", media.Size)
	}
	if transport.calls.Load() != 1 || transport.request == nil {
		t.Fatalf("range requests = %d request=%#v", transport.calls.Load(), transport.request)
	}
	if transport.request.Method != http.MethodGet ||
		transport.request.Header.Get("Range") != "bytes=0-0" ||
		transport.request.Header.Get("User-Agent") != "Infuse-Library/8.4.4" {
		t.Fatalf("range request = %#v", transport.request)
	}
}

func TestFFProberSizeFallbackFailsOpenForInvalidResponses(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		contentRange string
	}{
		{name: "full response", status: http.StatusOK, contentRange: "bytes 0-0/123456"},
		{name: "missing total", status: http.StatusPartialContent, contentRange: "bytes 0-0/*"},
		{name: "invalid unit", status: http.StatusPartialContent, contentRange: "octets 0-0/123456"},
		{name: "invalid range", status: http.StatusPartialContent, contentRange: "bytes 0-0"},
		{name: "zero total", status: http.StatusPartialContent, contentRange: "bytes 0-0/0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &rangeProbeTransport{status: test.status, contentRange: test.contentRange}
			prober := newOutputScriptProber(t, completeFFProbeJSONWithoutSize, &http.Client{Transport: transport})
			media, err := prober.Probe(t.Context(), "https://cdn.example.test/movie.mkv", "Infuse-Library/8.4.4")
			if err != nil {
				t.Fatal(err)
			}
			if media.Size != 0 {
				t.Fatalf("invalid size was accepted = %d", media.Size)
			}
			if transport.calls.Load() != 1 {
				t.Fatalf("range requests = %d", transport.calls.Load())
			}
		})
	}
}

func TestFFProberSizeFallbackTimeoutFailsOpen(t *testing.T) {
	transport := &deadlineErrorTransport{}
	prober := newOutputScriptProber(t, completeFFProbeJSONWithoutSize, &http.Client{Transport: transport})
	media, err := prober.Probe(t.Context(), "https://cdn.example.test/movie.mkv", "Infuse-Library/8.4.4")
	if err != nil {
		t.Fatal(err)
	}
	if media.Size != 0 {
		t.Fatalf("timeout changed size = %d", media.Size)
	}
	if transport.calls.Load() != 1 || transport.deadline.IsZero() {
		t.Fatalf("timeout request calls=%d deadline=%s", transport.calls.Load(), transport.deadline)
	}
	if transport.deadline.After(transport.observedAt.Add(defaultSizeProbeTimeout + 100*time.Millisecond)) {
		t.Fatalf("size probe deadline = %s, exceeds %s cap", transport.deadline.Sub(transport.observedAt), defaultSizeProbeTimeout)
	}
}

func TestFFProberDoesNotRangeProbeExistingSize(t *testing.T) {
	transport := &rangeProbeTransport{status: http.StatusPartialContent, contentRange: "bytes 0-0/123456"}
	prober := newOutputScriptProber(t, completeFFProbeJSONWithSize, &http.Client{Transport: transport})
	media, err := prober.Probe(t.Context(), "https://cdn.example.test/movie.mkv", "Infuse-Library/8.4.4")
	if err != nil {
		t.Fatal(err)
	}
	if media.Size != 987654 {
		t.Fatalf("existing size changed = %d", media.Size)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("unexpected range requests = %d", transport.calls.Load())
	}
}

func TestFFProberPreservesCancellation(t *testing.T) {
	prober := newScriptProber(t, "exec sleep 5", 5*time.Second, defaultProbeOutputLimit)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := prober.Probe(ctx, "https://example.test/movie.mkv", "Infuse-Library/8.4.4")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestFFProberReportsTimeout(t *testing.T) {
	prober := newScriptProber(t, "exec sleep 5", 20*time.Millisecond, defaultProbeOutputLimit)
	_, err := prober.Probe(t.Context(), "https://example.test/movie.mkv", "Infuse-Library/8.4.4")
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestFFProberBoundsProcessOutput(t *testing.T) {
	prober := newScriptProber(t, "while true; do echo 0123456789; done", 5*time.Second, 1)
	_, err := prober.Probe(t.Context(), "https://example.test/movie.mkv", "Infuse-Library/8.4.4")
	if !errors.Is(err, ErrProbeOutput) {
		t.Fatalf("Probe() error = %v", err)
	}
}

func newScriptProber(t *testing.T, body string, timeout time.Duration, outputLimit int64) *FFProber {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	prober, err := NewFFProber(FFProbeOptions{
		Binary: path, Timeout: timeout, ProbeSize: defaultProbeSize,
		AnalyzeDuration: defaultAnalyzeDuration, OutputLimit: outputLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return prober
}

const (
	completeFFProbeJSONWithoutSize = `{"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080}],"format":{"format_name":"matroska","duration":"60"}}`
	completeFFProbeJSONWithSize    = `{"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080}],"format":{"format_name":"matroska","duration":"60","size":"987654"}}`
)

func newOutputScriptProber(t *testing.T, output string, client *http.Client) *FFProber {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe-output-test")
	script := "#!/bin/sh\nprintf '%s' '" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	prober, err := NewFFProber(FFProbeOptions{
		Binary: path, Timeout: 5 * time.Second, ProbeSize: defaultProbeSize,
		AnalyzeDuration: defaultAnalyzeDuration, OutputLimit: defaultProbeOutputLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	prober.sizeClient = client
	return prober
}

type rangeProbeTransport struct {
	calls        atomic.Int64
	request      *http.Request
	status       int
	contentRange string
}

func (transport *rangeProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	transport.request = request.Clone(request.Context())
	header := make(http.Header)
	if transport.contentRange != "" {
		header.Set("Content-Range", transport.contentRange)
	}
	return &http.Response{
		StatusCode: transport.status,
		Header:     header,
		Body:       unreadableResponseBody{},
		Request:    request,
	}, nil
}

type deadlineErrorTransport struct {
	calls      atomic.Int64
	deadline   time.Time
	observedAt time.Time
}

func (transport *deadlineErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	transport.observedAt = time.Now()
	transport.deadline, _ = request.Context().Deadline()
	return nil, context.DeadlineExceeded
}

type unreadableResponseBody struct{}

func (unreadableResponseBody) Read([]byte) (int, error) {
	panic("media response body was read")
}

func (unreadableResponseBody) Close() error { return nil }
