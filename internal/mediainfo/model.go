// Package mediainfo owns the gateway's rebuildable technical-media records and
// the bounded analysis lifecycle that produces them.
package mediainfo

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// SchemaVersion changes when the durable record contract is incompatible.
	SchemaVersion = 1
	// ProviderMediaVaultFFProbe identifies the redirect plus bounded ffprobe path.
	ProviderMediaVaultFFProbe = "mediavault-ffprobe"
	// ProviderRevisionFFProbeJSONV1 identifies the normalized ffprobe contract.
	ProviderRevisionFFProbeJSONV1 = "ffprobe-json-v1"
	maxSTRMFingerprintBytes       = 64 << 10
)

// Key identifies one analysis result without depending on a short-lived CDN
// URL. A changed STRM control file creates a new key for the same Plex Part.
type Key struct {
	PlexServerID    string `json:"plex_server_id"`
	PartID          string `json:"part_id"`
	STRMFingerprint string `json:"strm_fingerprint"`
}

// Validate rejects keys that cannot safely participate in persistent cache
// identity or singleflight deduplication.
func (key Key) Validate() error {
	if strings.TrimSpace(key.PlexServerID) == "" {
		return errors.New("Plex server identity is required")
	}
	if strings.TrimSpace(key.PartID) == "" {
		return errors.New("Plex Part identity is required")
	}
	if len(key.STRMFingerprint) != sha256.Size*2 {
		return errors.New("STRM fingerprint must be a SHA-256 digest")
	}
	if _, err := hex.DecodeString(key.STRMFingerprint); err != nil {
		return errors.New("STRM fingerprint must be hexadecimal")
	}
	return nil
}

func (key Key) cacheKey() string {
	return key.PlexServerID + "\x00" + key.PartID + "\x00" + key.STRMFingerprint
}

// Record is the durable, rebuildable result for one exact Plex Part and STRM
// fingerprint. Signed media URLs and Plex credentials are never retained.
type Record struct {
	Key                Key       `json:"key"`
	RatingKey          string    `json:"rating_key,omitempty"`
	Provider           string    `json:"provider"`
	ContentFingerprint string    `json:"content_fingerprint,omitempty"`
	SchemaVersion      int       `json:"schema_version"`
	ProviderRevision   string    `json:"provider_revision"`
	Media              Media     `json:"media"`
	ProbedAt           time.Time `json:"probed_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	LastAccessedAt     time.Time `json:"last_accessed_at"`
	RetainUntil        time.Time `json:"retain_until"`
}

// Retained reports whether an identity-matched known-good record remains
// usable as stale fallback after its freshness window expires.
func (record Record) Retained(now time.Time) bool {
	return record.Key.Validate() == nil &&
		strings.TrimSpace(record.Provider) != "" &&
		strings.TrimSpace(record.ProviderRevision) != "" &&
		record.SchemaVersion == SchemaVersion &&
		record.Media.Complete &&
		!record.ProbedAt.IsZero() &&
		record.ExpiresAt.After(record.ProbedAt) &&
		!record.LastAccessedAt.IsZero() &&
		!record.RetainUntil.Before(record.ExpiresAt) &&
		record.RetainUntil.After(record.LastAccessedAt) &&
		now.Before(record.RetainUntil)
}

// Fresh reports whether a retained record can satisfy a request without
// scheduling conservative background revalidation.
func (record Record) Fresh(now time.Time) bool {
	return record.Retained(now) && now.Before(record.ExpiresAt)
}

// Media contains the technical fields needed for Plex Media, Part, and Stream
// projection. Complete means the bounded probe obtained a trustworthy minimum
// record; incomplete output is never stored as a successful result.
type Media struct {
	Complete        bool     `json:"complete"`
	Container       string   `json:"container,omitempty"`
	FormatName      string   `json:"format_name,omitempty"`
	FormatLongName  string   `json:"format_long_name,omitempty"`
	DurationMS      int64    `json:"duration_ms,omitempty"`
	StartTimeMS     int64    `json:"start_time_ms,omitempty"`
	Size            int64    `json:"size,omitempty"`
	Bitrate         int64    `json:"bitrate,omitempty"`
	VideoCodec      string   `json:"video_codec,omitempty"`
	VideoProfile    string   `json:"video_profile,omitempty"`
	VideoResolution string   `json:"video_resolution,omitempty"`
	Width           int      `json:"width,omitempty"`
	Height          int      `json:"height,omitempty"`
	AspectRatio     string   `json:"aspect_ratio,omitempty"`
	FrameRate       string   `json:"frame_rate,omitempty"`
	AudioCodec      string   `json:"audio_codec,omitempty"`
	AudioProfile    string   `json:"audio_profile,omitempty"`
	AudioChannels   int      `json:"audio_channels,omitempty"`
	Streams         []Stream `json:"streams"`
}

// Stream is a normalized ffprobe stream. Fields remain codec-neutral so new
// formats can be cached without a schema migration before Plex mapping exists.
type Stream struct {
	Index              int                `json:"index"`
	Type               string             `json:"type"`
	Codec              string             `json:"codec,omitempty"`
	CodecLongName      string             `json:"codec_long_name,omitempty"`
	CodecTag           string             `json:"codec_tag,omitempty"`
	Profile            string             `json:"profile,omitempty"`
	Level              int                `json:"level,omitempty"`
	TimeBase           string             `json:"time_base,omitempty"`
	StartTimeMS        int64              `json:"start_time_ms,omitempty"`
	DurationMS         int64              `json:"duration_ms,omitempty"`
	Bitrate            int64              `json:"bitrate,omitempty"`
	FrameCount         int64              `json:"frame_count,omitempty"`
	Width              int                `json:"width,omitempty"`
	Height             int                `json:"height,omitempty"`
	SampleAspectRatio  string             `json:"sample_aspect_ratio,omitempty"`
	DisplayAspectRatio string             `json:"display_aspect_ratio,omitempty"`
	FrameRate          string             `json:"frame_rate,omitempty"`
	AverageFrameRate   string             `json:"average_frame_rate,omitempty"`
	FieldOrder         string             `json:"field_order,omitempty"`
	ReferenceFrames    int                `json:"reference_frames,omitempty"`
	PixelFormat        string             `json:"pixel_format,omitempty"`
	BitDepth           int                `json:"bit_depth,omitempty"`
	ColorSpace         string             `json:"color_space,omitempty"`
	ColorRange         string             `json:"color_range,omitempty"`
	ColorPrimaries     string             `json:"color_primaries,omitempty"`
	ColorTransfer      string             `json:"color_transfer,omitempty"`
	ChromaLocation     string             `json:"chroma_location,omitempty"`
	HDRFormat          string             `json:"hdr_format,omitempty"`
	DolbyVision        *DolbyVision       `json:"dolby_vision,omitempty"`
	MasteringDisplay   *MasteringDisplay  `json:"mastering_display,omitempty"`
	ContentLightLevel  *ContentLightLevel `json:"content_light_level,omitempty"`
	SampleFormat       string             `json:"sample_format,omitempty"`
	SampleRate         int                `json:"sample_rate,omitempty"`
	Channels           int                `json:"channels,omitempty"`
	ChannelLayout      string             `json:"channel_layout,omitempty"`
	AudioServiceType   string             `json:"audio_service_type,omitempty"`
	Atmos              bool               `json:"atmos,omitempty"`
	DTSX               bool               `json:"dtsx,omitempty"`
	Language           string             `json:"language,omitempty"`
	Title              string             `json:"title,omitempty"`
	Disposition        Disposition        `json:"disposition"`
}

// DolbyVision preserves the DOVI configuration needed for later Plex mapping.
type DolbyVision struct {
	VersionMajor int `json:"version_major,omitempty"`
	VersionMinor int `json:"version_minor,omitempty"`
	Profile      int `json:"profile,omitempty"`
	Level        int `json:"level,omitempty"`
	RPUPresent   int `json:"rpu_present,omitempty"`
	ELPresent    int `json:"el_present,omitempty"`
	BLPresent    int `json:"bl_present,omitempty"`
	BLCompatID   int `json:"bl_compat_id,omitempty"`
}

// MasteringDisplay retains ffprobe's mastering-display values without lossy
// floating-point conversion. Plex mapping can consume them when supported.
type MasteringDisplay struct {
	RedX         string `json:"red_x,omitempty"`
	RedY         string `json:"red_y,omitempty"`
	GreenX       string `json:"green_x,omitempty"`
	GreenY       string `json:"green_y,omitempty"`
	BlueX        string `json:"blue_x,omitempty"`
	BlueY        string `json:"blue_y,omitempty"`
	WhitePointX  string `json:"white_point_x,omitempty"`
	WhitePointY  string `json:"white_point_y,omitempty"`
	MinLuminance string `json:"min_luminance,omitempty"`
	MaxLuminance string `json:"max_luminance,omitempty"`
}

// ContentLightLevel contains HDR MaxCLL and MaxFALL values when present.
type ContentLightLevel struct {
	MaxContent int `json:"max_content,omitempty"`
	MaxAverage int `json:"max_average,omitempty"`
}

// Disposition mirrors stable ffprobe stream flags used by Plex clients.
type Disposition struct {
	Default         bool `json:"default,omitempty"`
	Dub             bool `json:"dub,omitempty"`
	Original        bool `json:"original,omitempty"`
	Comment         bool `json:"comment,omitempty"`
	Lyrics          bool `json:"lyrics,omitempty"`
	Karaoke         bool `json:"karaoke,omitempty"`
	Forced          bool `json:"forced,omitempty"`
	HearingImpaired bool `json:"hearing_impaired,omitempty"`
	VisualImpaired  bool `json:"visual_impaired,omitempty"`
	CleanEffects    bool `json:"clean_effects,omitempty"`
	AttachedPicture bool `json:"attached_picture,omitempty"`
	TimedThumbnails bool `json:"timed_thumbnails,omitempty"`
	Captions        bool `json:"captions,omitempty"`
	Descriptions    bool `json:"descriptions,omitempty"`
	Metadata        bool `json:"metadata,omitempty"`
	Dependent       bool `json:"dependent,omitempty"`
	StillImage      bool `json:"still_image,omitempty"`
}

// FingerprintSTRM hashes the normalized first control target. Formatting and a
// MediaVault redirect origin change do not invalidate the same media pointer.
func FingerprintSTRM(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open STRM control file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxSTRMFingerprintBytes)
	for scanner.Scan() {
		target := strings.TrimSpace(scanner.Text())
		if target == "" {
			continue
		}
		return FingerprintSTRMTarget(target)
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("STRM control target exceeds fingerprint limit")
	}
	return "", errors.New("STRM control file is empty")
}

// FingerprintSTRMTarget hashes a target already read and validated from a STRM
// file. Callers that also need the target can avoid a second filesystem read,
// keeping the analysis identity bound to the exact control value being probed.
func FingerprintSTRMTarget(target string) (string, error) {
	normalized, err := normalizeSTRMTarget(strings.TrimSpace(target))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:]), nil
}

func normalizeSTRMTarget(target string) (string, error) {
	if strings.HasPrefix(target, "/") {
		return "path:" + target, nil
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", errors.New("STRM control target is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("STRM control target must use HTTP(S)")
	}
	parsed.Fragment = ""
	parsed.RawQuery = parsed.Query().Encode()
	if parsed.Path == "/redirect" || strings.HasPrefix(parsed.Path, "/redirect/") {
		parsed.Scheme = "mediavault"
		parsed.Host = "redirect"
		parsed.RawPath = ""
	}
	return parsed.String(), nil
}
