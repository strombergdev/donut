package entities

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/asticode/go-astiav"
	pionv3 "github.com/pion/webrtc/v3"
	pionv4 "github.com/pion/webrtc/v4"
)

const (
	MetadataChannelID string = "metadata"
)

type WebRTCSetupResponse struct {
	Connection *pionv3.PeerConnection
	Video      *pionv3.TrackLocalStaticSample
	Audio      *pionv3.TrackLocalStaticSample
	Data       *pionv3.DataChannel
	LocalSDP   *pionv3.SessionDescription
}

type RequestParams struct {
	StreamURL string
	StreamID  string
	Offer     pionv3.SessionDescription
}

func (p *RequestParams) Valid() error {
	if p == nil {
		return ErrMissingParamsOffer
	}

	if p.StreamID == "" {
		return ErrMissingStreamID
	}

	if p.StreamURL == "" {
		return ErrMissingStreamURL
	}
	isRTMP := strings.Contains(strings.ToLower(p.StreamURL), "rtmp")
	isSRT := strings.Contains(strings.ToLower(p.StreamURL), "srt")

	if !(isRTMP || isSRT) {
		return ErrUnsupportedStreamURL
	}

	return nil
}

type RequestParamsV4 struct {
	StreamURL string
	StreamID  string
	Offer     pionv4.SessionDescription
}

func (p *RequestParamsV4) Valid() error {
	if p == nil {
		return ErrMissingParamsOffer
	}

	if p.StreamID == "" {
		return ErrMissingStreamID
	}

	if p.StreamURL == "" {
		return ErrMissingStreamURL
	}
	isRTMP := strings.Contains(strings.ToLower(p.StreamURL), "rtmp")
	isSRT := strings.Contains(strings.ToLower(p.StreamURL), "srt")

	if !(isRTMP || isSRT) {
		return ErrUnsupportedStreamURL
	}

	return nil
}
func (p *RequestParams) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("RequestParams {StreamURL: %s, StreamID: %s}", p.StreamURL, p.StreamID)
}

type MessageType string

const (
	MessageTypeMetadata MessageType = "metadata"
)

type Message struct {
	Type    MessageType
	Message string
}

type Codec string
type MediaType string

const (
	UnknownCodec Codec = "unknownCodec"
	H264         Codec = "h264"
	H265         Codec = "h265"
	VP8          Codec = "vp8"
	VP9          Codec = "vp9"
	AV1          Codec = "av1"
	AAC          Codec = "aac"
	Opus         Codec = "opus"
)

const (
	UnknownType MediaType = "unknownMediaType"
	VideoType   MediaType = "video"
	AudioType   MediaType = "audio"
)

type Stream struct {
	Codec Codec
	Type  MediaType
	Id    uint16
	Index uint16
}

type MediaFrameContext struct {
	// DTS decoding timestamp
	DTS int
	// PTS presentation timestamp
	PTS int
	// Media frame duration
	Duration time.Duration
}

type StreamInfo struct {
	Streams []Stream
}

func (s *StreamInfo) VideoStreams() []Stream {
	var result []Stream
	for _, s := range s.Streams {
		if s.Type == VideoType {
			result = append(result, s)
		}
	}
	return result
}

func (s *StreamInfo) AudioStreams() []Stream {
	var result []Stream
	for _, s := range s.Streams {
		if s.Type == AudioType {
			result = append(result, s)
		}
	}
	return result
}

type Cue struct {
	Type      string
	StartTime int64
	Text      string
}

type DonutParameters struct {
	Cancel context.CancelFunc
	Ctx    context.Context

	Recipe DonutRecipe

	OnClose      func()
	OnError      func(err error)
	OnStream     func(st *Stream) error
	OnVideoFrame func(data []byte, c MediaFrameContext) error
	OnAudioFrame func(data []byte, c MediaFrameContext) error
}

type DonutMediaTaskAction string

var DonutTranscode DonutMediaTaskAction = "transcode"
var DonutBypass DonutMediaTaskAction = "bypass"

type DonutBitStreamFilter string

var DonutH264AnnexB DonutBitStreamFilter = "h264_mp4toannexb"

type DonutStreamFilter string

func AudioResamplerFilter(sampleRate int) *DonutStreamFilter {
	filter := DonutStreamFilter(fmt.Sprintf("aresample=%d", sampleRate))
	return &filter
}

// TODO: split entities per domain or files avoiding name collision.

// DonutMediaTask is a transformation template to apply over a media.
type DonutMediaTask struct {
	// Action the action that needs to be performed
	Action DonutMediaTaskAction
	// Codec is the main codec, it might be used depending on the action.
	Codec Codec
	// CodecContextOptions is a list of options to be applied on codec context.
	// If no value is provided ffmpeg will use defaults.
	// For instance, if one does not provide bit rate, it'll fallback to 64000 bps (opus)
	CodecContextOptions []LibAVOptionsCodecContext

	// DonutBitStreamFilter is the bitstream filter
	DonutBitStreamFilter *DonutBitStreamFilter

	// DonutStreamFilter is a regular filter
	DonutStreamFilter *DonutStreamFilter
}

type DonutInputOptionKey string

func (d DonutInputOptionKey) String() string {
	return string(d)
}

var DonutSRTStreamID DonutInputOptionKey = "srt_streamid"
var DonutSRTsmoother DonutInputOptionKey = "smoother"
var DonutSRTTranstype DonutInputOptionKey = "transtype"

var DonutRTMPLive DonutInputOptionKey = "rtmp_live"

type DonutInputFormat string

func (d DonutInputFormat) String() string {
	return string(d)
}

var DonutMpegTSFormat DonutInputFormat = "mpegts"
var DonutFLVFormat DonutInputFormat = "flv"

type DonutAppetizer struct {
	URL     string
	Format  DonutInputFormat
	Options map[DonutInputOptionKey]string
}

type DonutRecipe struct {
	Input DonutAppetizer
	Video DonutMediaTask
	Audio DonutMediaTask
}

type LibAVOptionsCodecContext func(c *astiav.CodecContext)

func SetSampleRate(sampleRate int) LibAVOptionsCodecContext {
	return func(c *astiav.CodecContext) {
		c.SetSampleRate(sampleRate)
	}
}

func SetTimeBase(num, den int) LibAVOptionsCodecContext {
	return func(c *astiav.CodecContext) {
		c.SetTimeBase(astiav.NewRational(num, den))
	}
}

func SetBitRate(bitRate int64) LibAVOptionsCodecContext {
	return func(c *astiav.CodecContext) {
		c.SetBitRate(bitRate)
	}
}

func SetBaselineProfile() LibAVOptionsCodecContext {
	return func(c *astiav.CodecContext) {
		c.SetProfile(astiav.ProfileH264Baseline)
	}
}

func SetGopSize(gopSize int) LibAVOptionsCodecContext {
	return func(c *astiav.CodecContext) {
		c.SetGopSize(gopSize)
	}
}

// SetSampleFormat sets sample format,
// CAUTION it only contains partial list of fmt
// TODO: move it to mappers
func SetSampleFormat(fmt string) LibAVOptionsCodecContext {
	var sf astiav.SampleFormat
	if fmt == "fltp" {
		sf = astiav.SampleFormatFltp
	} else if fmt == "flt" {
		sf = astiav.SampleFormatFlt
	} else {
		// DANGER: assuming a default value
		sf = astiav.SampleFormatS16
	}
	return func(c *astiav.CodecContext) {
		c.SetSampleFormat(sf)
	}
}

// TODO: implement proper matching
// DonutTransformRecipe
//  AudioTask: {Action: Transcode, From: AAC, To: Opus}
//  VideoTask: {Action: Bypass, From: H264, To: H264}

type Config struct {
	HTTPPort       int32  `required:"true" default:"8080"`
	HTTPHost       string `required:"true" default:"0.0.0.0"`
	PproffHTTPPort int32  `required:"true" default:"6060"`

	TCPICEPort         int      `required:"true" default:"8081"`
	UDPICEPort         int      `required:"true" default:"8094"`
	ICEReadBufferSize  int      `required:"true" default:"8"`
	ICEExternalIPsDNAT []string `required:"true" default:"127.0.0.1"`
	EnableICEMux       bool     `require:"true" default:"false"`
	StunServers        []string `required:"true" default:"stun:stun.l.google.com:19302,stun:stun1.l.google.com:19302,stun:stun2.l.google.com:19302,stun:stun4.l.google.com:19302"`

	SRTConnectionLatencyMS int32 `required:"true" default:"300"`
	// MPEG-TS consists of single units of 188 bytes. Multiplying 188*7 we get 1316,
	// which is the maximum product of 188 that is less than MTU 1500 (188*8=1504)
	// ref https://github.com/Haivision/srt/blob/master/docs/features/live-streaming.md#transmitting-mpeg-ts-binary-protocol-over-srt
	SRTReadBufferSizeBytes int `required:"true" default:"1316"`

	ProbingSize int `required:"true" default:"120"`

	// Add the following fields to fix the compilation error
	DefaultStreamURL string `required:"true" default:"srt://localhost:40053"`
	DefaultStreamID  string `required:"true" default:"stream-id"`
}
