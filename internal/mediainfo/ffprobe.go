package mediainfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultProbeTimeout     = 20 * time.Second
	defaultProbeSize        = 8 << 20
	defaultAnalyzeDuration  = 5 * time.Second
	defaultProbeOutputLimit = 2 << 20
)

var (
	ErrProbeUnavailable = errors.New("ffprobe is unavailable")
	ErrProbeTarget      = errors.New("ffprobe target is invalid")
	ErrProbeTimeout     = errors.New("ffprobe timed out")
	ErrProbeOutput      = errors.New("ffprobe output exceeded the limit")
	ErrProbeFailed      = errors.New("ffprobe failed")
	ErrProbeIncomplete  = errors.New("ffprobe result is incomplete")
	ErrProbeUserAgent   = errors.New("ffprobe User-Agent is invalid")
)

// FFProbeOptions bounds one external ffprobe process. ProbeSize and
// AnalyzeDuration affect format analysis; Timeout remains independent from the
// shorter HTTP cold-wait budget used by metadata enrichment.
type FFProbeOptions struct {
	Binary          string
	Timeout         time.Duration
	ProbeSize       int64
	AnalyzeDuration time.Duration
	OutputLimit     int64
}

// FFProber executes ffprobe with a fixed JSON contract and no inherited Plex
// credentials. The direct URL is never included in returned errors or logs.
type FFProber struct {
	binary          string
	timeout         time.Duration
	probeSize       int64
	analyzeDuration time.Duration
	outputLimit     int64
}

// NewFFProber resolves the ffprobe binary and validates all resource limits.
func NewFFProber(options FFProbeOptions) (*FFProber, error) {
	binary := strings.TrimSpace(options.Binary)
	if binary == "" {
		binary = "ffprobe"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, ErrProbeUnavailable
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeSize := options.ProbeSize
	if probeSize <= 0 {
		probeSize = defaultProbeSize
	}
	analyzeDuration := options.AnalyzeDuration
	if analyzeDuration <= 0 {
		analyzeDuration = defaultAnalyzeDuration
	}
	outputLimit := options.OutputLimit
	if outputLimit <= 0 {
		outputLimit = defaultProbeOutputLimit
	}
	return &FFProber{
		binary:          resolved,
		timeout:         timeout,
		probeSize:       probeSize,
		analyzeDuration: analyzeDuration,
		outputLimit:     outputLimit,
	}, nil
}

// Probe returns a complete normalized technical record or a bounded error. The
// User-Agent must match the MediaVault request that produced the direct URL.
func (prober *FFProber) Probe(ctx context.Context, directURL, userAgent string) (Media, error) {
	if prober == nil || prober.binary == "" {
		return Media{}, ErrProbeUnavailable
	}
	parsed, err := url.Parse(strings.TrimSpace(directURL))
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Media{}, ErrProbeTarget
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" || strings.ContainsAny(userAgent, "\r\n") {
		return Media{}, ErrProbeUserAgent
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, prober.timeout)
	defer cancel()
	commandCtx, cancelCommand := context.WithCancel(probeCtx)
	defer cancelCommand()

	stdout := &limitedBuffer{limit: prober.outputLimit, cancel: cancelCommand}
	stderr := &limitedBuffer{limit: 64 << 10, cancel: cancelCommand}
	command := exec.CommandContext(commandCtx, prober.binary, prober.arguments(parsed.String(), userAgent)...)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		switch {
		case stdout.exceeded || stderr.exceeded:
			return Media{}, ErrProbeOutput
		case errors.Is(probeCtx.Err(), context.Canceled):
			return Media{}, context.Canceled
		case errors.Is(probeCtx.Err(), context.DeadlineExceeded):
			return Media{}, ErrProbeTimeout
		default:
			return Media{}, ErrProbeFailed
		}
	}
	if stdout.exceeded {
		return Media{}, ErrProbeOutput
	}
	media, err := parseFFProbe(stdout.Bytes())
	if err != nil {
		return Media{}, err
	}
	return media, nil
}

func (prober *FFProber) arguments(directURL, userAgent string) []string {
	showEntries := strings.Join([]string{
		"format=format_name,format_long_name,start_time,duration,size,bit_rate",
		"stream=index,codec_name,codec_long_name,profile,codec_type,codec_tag_string,codec_tag,level,time_base,start_time,duration,bit_rate,nb_frames,width,height,sample_aspect_ratio,display_aspect_ratio,r_frame_rate,avg_frame_rate,field_order,refs,pix_fmt,bits_per_sample,bits_per_raw_sample,color_space,color_range,color_primaries,color_transfer,chroma_location,sample_fmt,sample_rate,channels,channel_layout,audio_service_type",
		"stream_tags=language,title",
		"stream_disposition",
		"stream_side_data",
	}, ":")
	return []string{
		"-v", "error",
		"-hide_banner",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-show_entries", showEntries,
		"-probesize", strconv.FormatInt(prober.probeSize, 10),
		"-analyzeduration", strconv.FormatInt(prober.analyzeDuration.Microseconds(), 10),
		"-rw_timeout", strconv.FormatInt(prober.timeout.Microseconds(), 10),
		"-user_agent", userAgent,
		directURL,
	}
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	written  int64
	exceeded bool
	cancel   context.CancelFunc
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer.exceeded {
		return 0, ErrProbeOutput
	}
	remaining := buffer.limit - buffer.written
	if int64(len(value)) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(value[:remaining])
			buffer.written += remaining
		}
		buffer.exceeded = true
		if buffer.cancel != nil {
			buffer.cancel()
		}
		return int(remaining), ErrProbeOutput
	}
	written, err := buffer.buffer.Write(value)
	buffer.written += int64(written)
	return written, err
}

func (buffer *limitedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

type scalar string

func (value *scalar) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*value = ""
		return nil
	}
	var stringValue string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &stringValue); err != nil {
			return err
		}
		*value = scalar(stringValue)
		return nil
	}
	*value = scalar(string(data))
	return nil
}

func (value scalar) string() string {
	return strings.TrimSpace(string(value))
}

func (value scalar) integer() int64 {
	parsed, _ := strconv.ParseInt(value.string(), 10, 64)
	return parsed
}

func (value scalar) milliseconds() int64 {
	parsed, _ := strconv.ParseFloat(value.string(), 64)
	return int64(parsed*1000 + 0.5)
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeFormat struct {
	FormatName     string `json:"format_name"`
	FormatLongName string `json:"format_long_name"`
	StartTime      scalar `json:"start_time"`
	Duration       scalar `json:"duration"`
	Size           scalar `json:"size"`
	Bitrate        scalar `json:"bit_rate"`
}

type ffprobeStream struct {
	Index              int                `json:"index"`
	CodecName          string             `json:"codec_name"`
	CodecLongName      string             `json:"codec_long_name"`
	Profile            string             `json:"profile"`
	CodecType          string             `json:"codec_type"`
	CodecTagString     string             `json:"codec_tag_string"`
	CodecTag           string             `json:"codec_tag"`
	Level              int                `json:"level"`
	TimeBase           string             `json:"time_base"`
	StartTime          scalar             `json:"start_time"`
	Duration           scalar             `json:"duration"`
	Bitrate            scalar             `json:"bit_rate"`
	FrameCount         scalar             `json:"nb_frames"`
	Width              int                `json:"width"`
	Height             int                `json:"height"`
	SampleAspectRatio  string             `json:"sample_aspect_ratio"`
	DisplayAspectRatio string             `json:"display_aspect_ratio"`
	FrameRate          string             `json:"r_frame_rate"`
	AverageFrameRate   string             `json:"avg_frame_rate"`
	FieldOrder         string             `json:"field_order"`
	ReferenceFrames    int                `json:"refs"`
	PixelFormat        string             `json:"pix_fmt"`
	BitsPerSample      int                `json:"bits_per_sample"`
	BitsPerRawSample   scalar             `json:"bits_per_raw_sample"`
	ColorSpace         string             `json:"color_space"`
	ColorRange         string             `json:"color_range"`
	ColorPrimaries     string             `json:"color_primaries"`
	ColorTransfer      string             `json:"color_transfer"`
	ChromaLocation     string             `json:"chroma_location"`
	SampleFormat       string             `json:"sample_fmt"`
	SampleRate         scalar             `json:"sample_rate"`
	Channels           int                `json:"channels"`
	ChannelLayout      string             `json:"channel_layout"`
	AudioServiceType   string             `json:"audio_service_type"`
	Tags               map[string]string  `json:"tags"`
	Disposition        ffprobeDisposition `json:"disposition"`
	SideDataList       []map[string]any   `json:"side_data_list"`
}

type ffprobeDisposition struct {
	Default         int `json:"default"`
	Dub             int `json:"dub"`
	Original        int `json:"original"`
	Comment         int `json:"comment"`
	Lyrics          int `json:"lyrics"`
	Karaoke         int `json:"karaoke"`
	Forced          int `json:"forced"`
	HearingImpaired int `json:"hearing_impaired"`
	VisualImpaired  int `json:"visual_impaired"`
	CleanEffects    int `json:"clean_effects"`
	AttachedPicture int `json:"attached_pic"`
	TimedThumbnails int `json:"timed_thumbnails"`
	Captions        int `json:"captions"`
	Descriptions    int `json:"descriptions"`
	Metadata        int `json:"metadata"`
	Dependent       int `json:"dependent"`
	StillImage      int `json:"still_image"`
}

func parseFFProbe(body []byte) (Media, error) {
	var output ffprobeOutput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		return Media{}, ErrProbeFailed
	}
	media := Media{
		Container:      normalizeContainer(output.Format.FormatName),
		FormatName:     output.Format.FormatName,
		FormatLongName: output.Format.FormatLongName,
		StartTimeMS:    output.Format.StartTime.milliseconds(),
		DurationMS:     output.Format.Duration.milliseconds(),
		Size:           output.Format.Size.integer(),
		Bitrate:        output.Format.Bitrate.integer(),
		Streams:        make([]Stream, 0, len(output.Streams)),
	}
	for _, raw := range output.Streams {
		stream := normalizeStream(raw)
		media.Streams = append(media.Streams, stream)
		applyMediaSummary(&media, stream)
	}
	media.Complete = mediaComplete(media)
	if !media.Complete {
		return Media{}, ErrProbeIncomplete
	}
	return media, nil
}

func normalizeStream(raw ffprobeStream) Stream {
	bitDepth := raw.BitsPerSample
	if raw.BitsPerRawSample.integer() > int64(bitDepth) {
		bitDepth = int(raw.BitsPerRawSample.integer())
	}
	stream := Stream{
		Index:              raw.Index,
		Type:               strings.ToLower(raw.CodecType),
		Codec:              strings.ToLower(raw.CodecName),
		CodecLongName:      raw.CodecLongName,
		CodecTag:           firstNonEmpty(raw.CodecTagString, raw.CodecTag),
		Profile:            raw.Profile,
		Level:              raw.Level,
		TimeBase:           raw.TimeBase,
		StartTimeMS:        raw.StartTime.milliseconds(),
		DurationMS:         raw.Duration.milliseconds(),
		Bitrate:            raw.Bitrate.integer(),
		FrameCount:         raw.FrameCount.integer(),
		Width:              raw.Width,
		Height:             raw.Height,
		SampleAspectRatio:  raw.SampleAspectRatio,
		DisplayAspectRatio: raw.DisplayAspectRatio,
		FrameRate:          raw.FrameRate,
		AverageFrameRate:   raw.AverageFrameRate,
		FieldOrder:         raw.FieldOrder,
		ReferenceFrames:    raw.ReferenceFrames,
		PixelFormat:        raw.PixelFormat,
		BitDepth:           bitDepth,
		ColorSpace:         raw.ColorSpace,
		ColorRange:         raw.ColorRange,
		ColorPrimaries:     raw.ColorPrimaries,
		ColorTransfer:      raw.ColorTransfer,
		ChromaLocation:     raw.ChromaLocation,
		SampleFormat:       raw.SampleFormat,
		SampleRate:         int(raw.SampleRate.integer()),
		Channels:           raw.Channels,
		ChannelLayout:      raw.ChannelLayout,
		AudioServiceType:   raw.AudioServiceType,
		Language:           raw.Tags["language"],
		Title:              raw.Tags["title"],
		Disposition:        normalizeDisposition(raw.Disposition),
	}
	stream.DolbyVision, stream.MasteringDisplay, stream.ContentLightLevel = normalizeSideData(raw.SideDataList)
	stream.HDRFormat = hdrFormat(stream)
	searchableAudio := strings.ToLower(strings.Join([]string{stream.Codec, stream.CodecLongName, stream.Profile, stream.Title}, " "))
	stream.Atmos = strings.Contains(searchableAudio, "atmos")
	stream.DTSX = strings.Contains(searchableAudio, "dts:x") || strings.Contains(searchableAudio, "dts-x")
	return stream
}

func normalizeDisposition(raw ffprobeDisposition) Disposition {
	return Disposition{
		Default: raw.Default != 0, Dub: raw.Dub != 0, Original: raw.Original != 0,
		Comment: raw.Comment != 0, Lyrics: raw.Lyrics != 0, Karaoke: raw.Karaoke != 0,
		Forced: raw.Forced != 0, HearingImpaired: raw.HearingImpaired != 0,
		VisualImpaired: raw.VisualImpaired != 0, CleanEffects: raw.CleanEffects != 0,
		AttachedPicture: raw.AttachedPicture != 0, TimedThumbnails: raw.TimedThumbnails != 0,
		Captions: raw.Captions != 0, Descriptions: raw.Descriptions != 0,
		Metadata: raw.Metadata != 0, Dependent: raw.Dependent != 0, StillImage: raw.StillImage != 0,
	}
}

func normalizeSideData(items []map[string]any) (*DolbyVision, *MasteringDisplay, *ContentLightLevel) {
	var dolbyVision *DolbyVision
	var mastering *MasteringDisplay
	var contentLight *ContentLightLevel
	for _, item := range items {
		typeName := strings.ToLower(anyString(item["side_data_type"]))
		switch {
		case strings.Contains(typeName, "dovi"):
			dolbyVision = &DolbyVision{
				VersionMajor: anyInt(item["dv_version_major"]), VersionMinor: anyInt(item["dv_version_minor"]),
				Profile: anyInt(item["dv_profile"]), Level: anyInt(item["dv_level"]),
				RPUPresent: anyInt(item["rpu_present_flag"]), ELPresent: anyInt(item["el_present_flag"]),
				BLPresent: anyInt(item["bl_present_flag"]), BLCompatID: anyInt(item["dv_bl_signal_compatibility_id"]),
			}
		case strings.Contains(typeName, "mastering display"):
			mastering = &MasteringDisplay{
				RedX: anyString(item["red_x"]), RedY: anyString(item["red_y"]),
				GreenX: anyString(item["green_x"]), GreenY: anyString(item["green_y"]),
				BlueX: anyString(item["blue_x"]), BlueY: anyString(item["blue_y"]),
				WhitePointX: anyString(item["white_point_x"]), WhitePointY: anyString(item["white_point_y"]),
				MinLuminance: anyString(item["min_luminance"]), MaxLuminance: anyString(item["max_luminance"]),
			}
		case strings.Contains(typeName, "content light"):
			contentLight = &ContentLightLevel{
				MaxContent: anyInt(item["max_content"]), MaxAverage: anyInt(item["max_average"]),
			}
		}
	}
	return dolbyVision, mastering, contentLight
}

func hdrFormat(stream Stream) string {
	if stream.DolbyVision != nil {
		return "dolby_vision"
	}
	switch strings.ToLower(stream.ColorTransfer) {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	default:
		return ""
	}
}

func applyMediaSummary(media *Media, stream Stream) {
	if media == nil {
		return
	}
	switch stream.Type {
	case "video":
		if media.VideoCodec == "" || stream.Disposition.Default {
			media.VideoCodec = stream.Codec
			media.VideoProfile = stream.Profile
			media.Width = stream.Width
			media.Height = stream.Height
			media.VideoResolution = videoResolution(stream.Width, stream.Height)
			media.AspectRatio = stream.DisplayAspectRatio
			media.FrameRate = firstNonEmpty(stream.AverageFrameRate, stream.FrameRate)
		}
	case "audio":
		if media.AudioCodec == "" || stream.Disposition.Default {
			media.AudioCodec = stream.Codec
			media.AudioProfile = stream.Profile
			media.AudioChannels = stream.Channels
		}
	}
}

func mediaComplete(media Media) bool {
	if media.Container == "" || media.DurationMS <= 0 || len(media.Streams) == 0 {
		return false
	}
	mediaStreams := 0
	for _, stream := range media.Streams {
		switch stream.Type {
		case "video":
			mediaStreams++
			if stream.Codec == "" || stream.Width <= 0 || stream.Height <= 0 {
				return false
			}
		case "audio":
			mediaStreams++
			if stream.Codec == "" || stream.Channels <= 0 {
				return false
			}
		case "subtitle":
			if stream.Codec == "" {
				return false
			}
		}
	}
	return mediaStreams > 0
}

func normalizeContainer(formatName string) string {
	formats := strings.Split(strings.ToLower(formatName), ",")
	for _, format := range formats {
		switch strings.TrimSpace(format) {
		case "matroska":
			return "mkv"
		case "mov", "mp4":
			return "mp4"
		case "mpegts":
			return "mpegts"
		case "avi", "flv", "webm", "ogg", "wav", "mp3", "flac":
			return strings.TrimSpace(format)
		}
	}
	if len(formats) > 0 {
		return strings.TrimSpace(formats[0])
	}
	return ""
}

func videoResolution(width, height int) string {
	switch {
	case width >= 3800 || height >= 2100:
		return "4k"
	case height >= 1400:
		return "2k"
	case height >= 1000:
		return "1080"
	case height >= 700:
		return "720"
	case height > 0:
		return "sd"
	default:
		return ""
	}
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func anyInt(value any) int {
	parsed, _ := strconv.Atoi(anyString(value))
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func probeErrorKind(err error) string {
	switch {
	case errors.Is(err, ErrProbeUnavailable):
		return "unavailable"
	case errors.Is(err, ErrProbeTarget):
		return "target"
	case errors.Is(err, ErrProbeTimeout):
		return "timeout"
	case errors.Is(err, ErrProbeOutput):
		return "output_limit"
	case errors.Is(err, ErrProbeIncomplete):
		return "incomplete"
	default:
		return "failed"
	}
}

func (prober *FFProber) String() string {
	if prober == nil {
		return "ffprobe(unavailable)"
	}
	return fmt.Sprintf("ffprobe(timeout=%s,probesize=%d,analyzeduration=%s)", prober.timeout, prober.probeSize, prober.analyzeDuration)
}
