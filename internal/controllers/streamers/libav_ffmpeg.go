package streamers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/asticode/go-astikit"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	"github.com/pion/rtp"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type LibAVFFmpegStreamer struct {
	c *entities.Config
	l *zap.SugaredLogger
	m *mapper.Mapper

	lastAudioFrameDTS     float64
	currentAudioFrameSize float64
}

type LibAVFFmpegStreamerParams struct {
	fx.In
	C *entities.Config
	L *zap.SugaredLogger
	M *mapper.Mapper
}

type ResultLibAVFFmpegStreamer struct {
	fx.Out
	LibAVFFmpegStreamer DonutStreamer `group:"streamers"`
}

func NewLibAVFFmpegStreamer(p LibAVFFmpegStreamerParams) ResultLibAVFFmpegStreamer {
	return ResultLibAVFFmpegStreamer{
		LibAVFFmpegStreamer: &LibAVFFmpegStreamer{
			c: p.C,
			l: p.L,
			m: p.M,
		},
	}
}

func (c *LibAVFFmpegStreamer) Match(req *entities.RequestParams) bool {
	isRTMP := strings.Contains(strings.ToLower(req.StreamURL), "rtmp")
	isSRT := strings.Contains(strings.ToLower(req.StreamURL), "srt")

	return isRTMP || isSRT
}

type streamContext struct {
	// IN
	inputStream     *astiav.Stream
	decCodec        *astiav.Codec
	decCodecContext *astiav.CodecContext
	decFrame        *astiav.Frame

	// FILTER
	filterGraph       *astiav.FilterGraph
	buffersinkContext *astiav.FilterContext
	buffersrcContext  *astiav.FilterContext
	filterFrame       *astiav.Frame

	// OUT
	encCodec        *astiav.Codec
	encCodecContext *astiav.CodecContext
	encPkt          *astiav.Packet

	// Bit stream filter
	bsfContext *astiav.BitStreamFilterContext
	bsfPacket  *astiav.Packet
}

type libAVParams struct {
	inputFormatContext *astiav.FormatContext
	streams            map[int]*streamContext
}

func (c *LibAVFFmpegStreamer) Stream(donut *entities.DonutParameters) {
	c.l.Infof("streaming has started for %#v", donut)

	closer := astikit.NewCloser()
	defer closer.Close()

	p := &libAVParams{
		streams: make(map[int]*streamContext),
	}

	// it's useful for debugging
	// astiav.SetLogLevel(astiav.LogLevelDebug)
	astiav.SetLogLevel(astiav.LogLevelDebug)
	astiav.SetLogCallback(func(_ astiav.Classer, l astiav.LogLevel, fmt, msg string) {
		c.l.Infof("ffmpeg %s: - %s", c.libAVLogToString(l), strings.TrimSpace(msg))
	})

	c.l.Infof("preparing input")
	if err := c.prepareInput(p, closer, donut); err != nil {
		c.onError(err, donut)
		return
	}

	c.l.Infof("preparing output")
	if err := c.prepareOutput(p, closer, donut); err != nil {
		c.onError(err, donut)
		return
	}

	c.l.Infof("preparing filters")
	if err := c.prepareFilters(p, closer, donut); err != nil {
		c.onError(err, donut)
		return
	}

	c.l.Infof("preparing bit stream filters")
	if err := c.prepareBitStreamFilters(p, closer, donut); err != nil {
		c.onError(err, donut)
		return
	}

	inPkt := astiav.AllocPacket()
	closer.Add(inPkt.Free)

	for {
		select {
		case <-donut.Ctx.Done():
			if errors.Is(donut.Ctx.Err(), context.Canceled) {
				c.l.Info("streaming has stopped due cancellation")
				return
			}
			c.onError(donut.Ctx.Err(), donut)
			return
		default:
			if err := p.inputFormatContext.ReadFrame(inPkt); err != nil {
				if errors.Is(err, astiav.ErrEof) || errors.Is(err, io.EOF) {
					c.l.Info("End of stream reached")
					return
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe) {
					c.l.Info("Stream canceled or pipe closed")
					return
				}
				c.onError(err, donut)
				return
			}

			s, ok := p.streams[inPkt.StreamIndex()]
			if !ok {
				c.l.Warnf("skipping to process stream id=%d", inPkt.StreamIndex())
				continue
			}

			if s.bsfContext != nil {
				if err := c.applyBitStreamFilter(p, inPkt, s, donut); err != nil {
					c.onError(err, donut)
					return
				}
			} else {
				if err := c.processPacket(p, inPkt, s, donut); err != nil {
					c.onError(err, donut)
					return
				}
			}
			inPkt.Unref()
		}
	}
}

func (c *LibAVFFmpegStreamer) onError(err error, p *entities.DonutParameters) {
	if p.OnError != nil {
		p.OnError(err)
	}
}

func (c *LibAVFFmpegStreamer) prepareInput(p *libAVParams, closer *astikit.Closer, donut *entities.DonutParameters) error {
	if p.inputFormatContext = astiav.AllocFormatContext(); p.inputFormatContext == nil {
		return errors.New("ffmpeg/libav: input format context is nil")
	}
	closer.Add(p.inputFormatContext.Free)

	// Modify SRT URL to use 0.0.0.0 and remove query params
	inputURL := donut.Recipe.Input.URL
	if strings.Contains(strings.ToLower(inputURL), "srt://") {
		urlParts := strings.Split(inputURL, "://")
		if len(urlParts) == 2 {
			// Remove any query parameters and just get host:port
			hostPort := strings.Split(strings.Split(urlParts[1], "?")[0], ":")
			if len(hostPort) == 2 {
				inputURL = fmt.Sprintf("srt://0.0.0.0:%s", hostPort[1])
			}
		}
	}

	inputFormat, err := c.defineInputFormat(donut.Recipe.Input.Format.String())
	if err != nil {
		return err
	}
	// Create input options and add SRT listener mode
	inputOptions := &astiav.Dictionary{}
	closer.Add(inputOptions.Free)

	// Copy existing options if any
	if len(donut.Recipe.Input.Options) > 0 {
		for k, v := range donut.Recipe.Input.Options {
			inputOptions.Set(k.String(), v, 0)
		}
	}

	// Add SRT listener mode
	if strings.Contains(strings.ToLower(inputURL), "srt://") {
		inputOptions.Set("mode", "listener", 0)
	}

	if err := p.inputFormatContext.OpenInput(inputURL, inputFormat, inputOptions); err != nil {
		return fmt.Errorf("ffmpeg/libav: opening input failed %w", err)
	}
	closer.Add(p.inputFormatContext.CloseInput)

	if err := p.inputFormatContext.FindStreamInfo(nil); err != nil {
		return fmt.Errorf("ffmpeg/libav: finding stream info failed %w", err)
	}

	for _, is := range p.inputFormatContext.Streams() {
		if is.CodecParameters().MediaType() != astiav.MediaTypeAudio &&
			is.CodecParameters().MediaType() != astiav.MediaTypeVideo {
			c.l.Infof("skipping media type %s", is.CodecParameters().MediaType().String())
			continue
		}

		s := &streamContext{inputStream: is}

		// Log the time base and other timing info
		c.l.Infof("Stream #%d: type=%s codec=%s timebase=%v avg_frame_rate=%v r_frame_rate=%v",
			is.Index(),
			is.CodecParameters().MediaType().String(),
			is.CodecParameters().CodecID().String(),
			is.TimeBase().String(),
			is.AvgFrameRate().String(),
			is.RFrameRate().String())

		if s.decCodec = astiav.FindDecoder(is.CodecParameters().CodecID()); s.decCodec == nil {
			return errors.New("ffmpeg/libav: codec is missing")
		}

		if s.decCodecContext = astiav.AllocCodecContext(s.decCodec); s.decCodecContext == nil {
			return errors.New("ffmpeg/libav: codec context is nil")
		}
		closer.Add(s.decCodecContext.Free)

		if err := is.CodecParameters().ToCodecContext(s.decCodecContext); err != nil {
			return fmt.Errorf("ffmpeg/libav: updating codec context failed %w", err)
		}

		//FFMPEG_NEW
		s.decCodecContext.SetTimeBase(s.inputStream.TimeBase())

		if is.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			s.decCodecContext.SetFramerate(p.inputFormatContext.GuessFrameRate(is, nil))
		}

		if err := s.decCodecContext.Open(s.decCodec, nil); err != nil {
			return fmt.Errorf("ffmpeg/libav: opening codec context failed %w", err)
		}

		s.decFrame = astiav.AllocFrame()
		closer.Add(s.decFrame.Free)

		p.streams[is.Index()] = s

		if donut.OnStream != nil {
			stream := c.m.FromLibAVStreamToEntityStream(is)
			err := donut.OnStream(&stream)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *LibAVFFmpegStreamer) prepareOutput(p *libAVParams, closer *astikit.Closer, donut *entities.DonutParameters) error {
	for _, is := range p.inputFormatContext.Streams() {
		s, ok := p.streams[is.Index()]
		if !ok {
			c.l.Infof("skipping absent stream index = %d", is.Index())
			continue
		}

		isVideo := s.decCodecContext.MediaType() == astiav.MediaTypeVideo
		isVideoBypass := donut.Recipe.Video.Action == entities.DonutBypass
		if isVideo && isVideoBypass {
			c.l.Infof("bypass video for %+v", s.inputStream)
			continue
		}

		isAudio := s.decCodecContext.MediaType() == astiav.MediaTypeAudio
		isAudioBypass := donut.Recipe.Audio.Action == entities.DonutBypass
		if isAudio && isAudioBypass {
			c.l.Infof("bypass audio for %+v", s.inputStream)
			continue
		}

		var codecID astiav.CodecID
		if isAudio {
			audioCodecID, err := c.m.FromStreamCodecToLibAVCodecID(donut.Recipe.Audio.Codec)
			if err != nil {
				return err
			}
			codecID = audioCodecID
		}
		if isVideo {
			videoCodecID, err := c.m.FromStreamCodecToLibAVCodecID(donut.Recipe.Video.Codec)
			if err != nil {
				return err
			}
			codecID = videoCodecID
		}

		if s.encCodec = astiav.FindEncoder(codecID); s.encCodec == nil {
			// TODO: migrate error to entity
			return fmt.Errorf("cannot find a libav encoder for %+v", codecID)
		}

		if s.encCodecContext = astiav.AllocCodecContext(s.encCodec); s.encCodecContext == nil {
			return errors.New("ffmpeg/libav: codec context is nil")
		}
		closer.Add(s.encCodecContext.Free)

		if isAudio {
			if v := s.encCodec.ChannelLayouts(); len(v) > 0 {
				s.encCodecContext.SetChannelLayout(v[0])
			} else {
				s.encCodecContext.SetChannelLayout(s.decCodecContext.ChannelLayout())
			}
			s.encCodecContext.SetChannels(s.decCodecContext.Channels())
			s.encCodecContext.SetSampleRate(s.decCodecContext.SampleRate())
			if v := s.encCodec.SampleFormats(); len(v) > 0 {
				s.encCodecContext.SetSampleFormat(v[0])
			} else {
				s.encCodecContext.SetSampleFormat(s.decCodecContext.SampleFormat())
			}
			s.encCodecContext.SetTimeBase(s.decCodecContext.TimeBase())
		}

		if isVideo {
			if v := s.encCodec.PixelFormats(); len(v) > 0 {
				s.encCodecContext.SetPixelFormat(v[0])
			} else {
				s.encCodecContext.SetPixelFormat(s.decCodecContext.PixelFormat())
			}
			s.encCodecContext.SetSampleAspectRatio(s.decCodecContext.SampleAspectRatio())
			s.encCodecContext.SetTimeBase(s.decCodecContext.TimeBase())
			s.encCodecContext.SetHeight(s.decCodecContext.Height())
			s.encCodecContext.SetWidth(s.decCodecContext.Width())
			// s.encCodecContext.SetFramerate(s.inputStream.AvgFrameRate())

			// overriding with user provide config
			if len(donut.Recipe.Video.CodecContextOptions) > 0 {
				for _, opt := range donut.Recipe.Video.CodecContextOptions {
					opt(s.encCodecContext)
				}
			}
		}

		if s.decCodecContext.Flags().Has(astiav.CodecContextFlagGlobalHeader) {
			s.encCodecContext.SetFlags(s.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
		}

		if err := s.encCodecContext.Open(s.encCodec, nil); err != nil {
			return fmt.Errorf("opening encoder context failed: %w", err)
		}

		// Log input and output time bases
		if s.decCodecContext.MediaType() == astiav.MediaTypeAudio {
			c.l.Infof("Audio stream #%d: input_timebase=%v dec_timebase=%v sample_rate=%d",
				is.Index(),
				s.inputStream.TimeBase().String(),
				s.decCodecContext.TimeBase().String(),
				s.decCodecContext.SampleRate())
		}
		if s.decCodecContext.MediaType() == astiav.MediaTypeVideo {
			c.l.Infof("Video stream #%d: input_timebase=%v dec_timebase=%v framerate=%v",
				is.Index(),
				s.inputStream.TimeBase().String(),
				s.decCodecContext.TimeBase().String(),
				s.decCodecContext.Framerate().String())
		}
	}
	return nil
}

func (c *LibAVFFmpegStreamer) prepareFilters(p *libAVParams, closer *astikit.Closer, donut *entities.DonutParameters) error {
	for _, s := range p.streams {

		isVideo := s.decCodecContext.MediaType() == astiav.MediaTypeVideo
		isVideoBypass := donut.Recipe.Video.Action == entities.DonutBypass
		if isVideo && isVideoBypass {
			c.l.Infof("bypass video for %+v", s.inputStream)
			continue
		}

		isAudio := s.decCodecContext.MediaType() == astiav.MediaTypeAudio
		isAudioBypass := donut.Recipe.Audio.Action == entities.DonutBypass
		if isAudio && isAudioBypass {
			c.l.Infof("bypass audio for %+v", s.inputStream)
			continue
		}

		var args astiav.FilterArgs
		var buffersrc, buffersink *astiav.Filter
		var content string
		var err error

		if s.filterGraph = astiav.AllocFilterGraph(); s.filterGraph == nil {
			return errors.New("main: graph is nil")
		}
		closer.Add(s.filterGraph.Free)

		outputs := astiav.AllocFilterInOut()
		if outputs == nil {
			return errors.New("main: outputs is nil")
		}
		closer.Add(outputs.Free)

		inputs := astiav.AllocFilterInOut()
		if inputs == nil {
			return errors.New("main: inputs is nil")
		}
		closer.Add(inputs.Free)

		if isAudio {
			args = astiav.FilterArgs{
				"channel_layout": s.decCodecContext.ChannelLayout().String(),
				"sample_fmt":     s.decCodecContext.SampleFormat().Name(),
				"sample_rate":    strconv.Itoa(s.decCodecContext.SampleRate()),
				"time_base":      s.decCodecContext.TimeBase().String(),
			}
			buffersrc = astiav.FindFilterByName("abuffer")
			buffersink = astiav.FindFilterByName("abuffersink")
			if donut.Recipe.Audio.DonutStreamFilter != nil {
				content = string(*donut.Recipe.Audio.DonutStreamFilter)
			} else {
				content = "anull" /* passthrough (dummy) filter for audio */
			}
		}

		if isVideo {
			args = astiav.FilterArgs{
				"pix_fmt":      strconv.Itoa(int(s.decCodecContext.PixelFormat())),
				"pixel_aspect": s.decCodecContext.SampleAspectRatio().String(),
				"time_base":    s.decCodecContext.TimeBase().String(),
				"video_size":   strconv.Itoa(s.decCodecContext.Width()) + "x" + strconv.Itoa(s.decCodecContext.Height()),
			}
			buffersrc = astiav.FindFilterByName("buffer")
			buffersink = astiav.FindFilterByName("buffersink")
			if donut.Recipe.Video.DonutStreamFilter != nil {
				content = string(*donut.Recipe.Video.DonutStreamFilter)
			} else {
				content = "null" /* passthrough (dummy) filter for video */
			}
		}

		if buffersrc == nil {
			return errors.New("main: buffersrc is nil")
		}
		if buffersink == nil {
			return errors.New("main: buffersink is nil")
		}

		if s.buffersrcContext, err = s.filterGraph.NewFilterContext(buffersrc, "in", args); err != nil {
			return fmt.Errorf("main: creating buffersrc context failed: %w", err)
		}
		if s.buffersinkContext, err = s.filterGraph.NewFilterContext(buffersink, "out", nil); err != nil {
			return fmt.Errorf("main: creating buffersink context failed: %w", err)
		}

		outputs.SetName("in")
		outputs.SetFilterContext(s.buffersrcContext)
		outputs.SetPadIdx(0)
		outputs.SetNext(nil)

		inputs.SetName("out")
		inputs.SetFilterContext(s.buffersinkContext)
		inputs.SetPadIdx(0)
		inputs.SetNext(nil)

		if err = s.filterGraph.Parse(content, inputs, outputs); err != nil {
			return fmt.Errorf("main: parsing filter failed: %w", err)
		}

		if err = s.filterGraph.Configure(); err != nil {
			return fmt.Errorf("main: configuring filter failed: %w", err)
		}

		s.filterFrame = astiav.AllocFrame()
		closer.Add(s.filterFrame.Free)

		s.encPkt = astiav.AllocPacket()
		closer.Add(s.encPkt.Free)
	}
	return nil
}

func (c *LibAVFFmpegStreamer) prepareBitStreamFilters(p *libAVParams, closer *astikit.Closer, donut *entities.DonutParameters) error {
	for _, s := range p.streams {
		isVideo := s.decCodecContext.MediaType() == astiav.MediaTypeVideo
		isAudio := s.decCodecContext.MediaType() == astiav.MediaTypeAudio
		var currentMedia *entities.DonutMediaTask

		if isAudio {
			currentMedia = &donut.Recipe.Audio
		} else if isVideo {
			currentMedia = &donut.Recipe.Video
		} else {
			c.l.Warnf("ignoring bit stream filter for media type %s", s.decCodecContext.MediaType().String())
			continue
		}

		if currentMedia.DonutBitStreamFilter == nil {
			c.l.Infof("no bit stream filter configured for %s", s.decCodecContext.String())
			continue
		}

		bsf := astiav.FindBitStreamFilterByName(string(*currentMedia.DonutBitStreamFilter))
		if bsf == nil {
			return fmt.Errorf("can not find the filter %s", string(*currentMedia.DonutBitStreamFilter))
		}

		var err error
		s.bsfContext, err = astiav.AllocBitStreamFilterContext(bsf)
		if err != nil {
			return fmt.Errorf("error while allocating bit stream context %w", err)
		}
		closer.Add(s.bsfContext.Free)

		s.bsfContext.SetTimeBaseIn(s.inputStream.TimeBase())
		if err := s.inputStream.CodecParameters().Copy(s.bsfContext.CodecParametersIn()); err != nil {
			return fmt.Errorf("error while copying codec parameters %w", err)
		}

		if err := s.bsfContext.Initialize(); err != nil {
			return fmt.Errorf("error while initiating %w", err)
		}
		s.bsfPacket = astiav.AllocPacket()
		closer.Add(s.bsfPacket.Free)
	}
	return nil
}

func (c *LibAVFFmpegStreamer) processPacket(p *libAVParams, pkt *astiav.Packet, s *streamContext, donut *entities.DonutParameters) error {
	isVideo := s.decCodecContext.MediaType() == astiav.MediaTypeVideo
	isAudio := s.decCodecContext.MediaType() == astiav.MediaTypeAudio
	var currentMedia *entities.DonutMediaTask

	if isAudio {
		currentMedia = &donut.Recipe.Audio
	} else if isVideo {
		currentMedia = &donut.Recipe.Video
	} else {
		c.l.Warnf("ignoring to stream for media type %s", s.decCodecContext.MediaType().String())
		return nil
	}

	byPass := currentMedia.Action == entities.DonutBypass
	if isVideo && byPass {
		if donut.OnVideoFrame != nil {
			pkt.RescaleTs(s.inputStream.TimeBase(), s.decCodecContext.TimeBase())
			if err := donut.OnVideoFrame(pkt.Data(), entities.MediaFrameContext{
				PTS:      int(pkt.Pts()),
				DTS:      int(pkt.Dts()),
				Duration: c.defineVideoDuration(s, pkt),
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if isAudio && byPass {
		if donut.OnAudioFrame != nil {
			pkt.RescaleTs(s.inputStream.TimeBase(), s.decCodecContext.TimeBase())
			if err := donut.OnAudioFrame(pkt.Data(), entities.MediaFrameContext{
				PTS:      int(pkt.Pts()),
				DTS:      int(pkt.Dts()),
				Duration: c.defineAudioDuration(s, pkt),
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// if isAudio {
	// 	continue
	// }

	if err := s.decCodecContext.SendPacket(pkt); err != nil {
		return err
	}

	for {
		if err := s.decCodecContext.ReceiveFrame(s.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}
			return err
		}
		if err := c.filterAndEncode(p, s.decFrame, s, donut); err != nil {
			return err
		}
	}
	return nil
}

func (c *LibAVFFmpegStreamer) applyBitStreamFilter(p *libAVParams, pkt *astiav.Packet, s *streamContext, donut *entities.DonutParameters) error {
	if err := s.bsfContext.SendPacket(pkt); err != nil && !errors.Is(err, astiav.ErrEagain) {
		return fmt.Errorf("sending bit stream packet failed: %w", err)
	}

	for {
		if err := s.bsfContext.ReceivePacket(s.bsfPacket); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}
			return fmt.Errorf("receiving bit stream packet failed: %w", err)
		}

		c.processPacket(p, s.bsfPacket, s, donut)
		s.bsfPacket.Unref()
	}
	return nil
}

func (c *LibAVFFmpegStreamer) filterAndEncode(p *libAVParams, f *astiav.Frame, s *streamContext, donut *entities.DonutParameters) (err error) {
	if err = s.buffersrcContext.BuffersrcAddFrame(f, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
		return fmt.Errorf("adding frame failed: %w", err)
	}
	for {
		s.filterFrame.Unref()

		if err = s.buffersinkContext.BuffersinkGetFrame(s.filterFrame, astiav.NewBuffersinkFlags()); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				err = nil
				break
			}
			return fmt.Errorf("getting frame failed: %w", err)
		}
		// TODO: should we avoid setting the picture type for audio?
		s.filterFrame.SetPictureType(astiav.PictureTypeNone)
		if err = c.encodeFrame(p, s.filterFrame, s, donut); err != nil {
			err = fmt.Errorf("main: encoding and writing frame failed: %w", err)
			return
		}
	}
	return nil
}

func (c *LibAVFFmpegStreamer) encodeFrame(p *libAVParams, f *astiav.Frame, s *streamContext, donut *entities.DonutParameters) (err error) {
	s.encPkt.Unref()

	// when converting from aac to opus using filters,
	// the np samples are bigger than the frame size
	// to fix the error "more samples than frame size"
	if f != nil {
		f.SetNbSamples(s.encCodecContext.FrameSize())
	}

	if err = s.encCodecContext.SendFrame(f); err != nil {
		return fmt.Errorf("sending frame failed: %w", err)
	}

	for {
		if err = s.encCodecContext.ReceivePacket(s.encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				err = nil
				break
			}
			return fmt.Errorf("receiving packet failed: %w", err)
		}

		s.encPkt.RescaleTs(s.inputStream.TimeBase(), s.encCodecContext.TimeBase())

		// Create RTP packet
		rtpPacket := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96, // Adjust based on codec
				SequenceNumber: 0,  // Will be set by WebRTC
				Timestamp:      uint32(s.encPkt.Pts()),
				SSRC:           0, // Will be set by WebRTC
			},
			Payload: s.encPkt.Data(),
		}

		isVideo := s.decCodecContext.MediaType() == astiav.MediaTypeVideo
		if isVideo {
			if donut.OnVideoFrame != nil {
				rtpData, marshalErr := rtpPacket.Marshal()
				if marshalErr != nil {
					return fmt.Errorf("failed to marshal video RTP packet: %w", marshalErr)
				}
				if err := donut.OnVideoFrame(rtpData, entities.MediaFrameContext{
					PTS:      int(s.encPkt.Pts()),
					DTS:      int(s.encPkt.Dts()),
					Duration: c.defineVideoDuration(s, s.encPkt),
				}); err != nil {
					return err
				}
			}
		}

		isAudio := s.decCodecContext.MediaType() == astiav.MediaTypeAudio
		if isAudio {
			if donut.OnAudioFrame != nil {
				rtpPacket.PayloadType = 111 // Opus
				rtpData, marshalErr := rtpPacket.Marshal()
				if marshalErr != nil {
					return fmt.Errorf("failed to marshal audio RTP packet: %w", marshalErr)
				}
				if err := donut.OnAudioFrame(rtpData, entities.MediaFrameContext{
					PTS:      int(s.encPkt.Pts()),
					DTS:      int(s.encPkt.Dts()),
					Duration: c.defineAudioDuration(s, s.encPkt),
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (c *LibAVFFmpegStreamer) defineInputFormat(streamFormat string) (*astiav.InputFormat, error) {
	var inputFormat *astiav.InputFormat
	if streamFormat != "" {
		inputFormat = astiav.FindInputFormat(streamFormat)
		if inputFormat == nil {
			return nil, fmt.Errorf("ffmpeg/libav: could not find %s input format", streamFormat)
		}
	}
	return inputFormat, nil
}

func (c *LibAVFFmpegStreamer) defineInputOptions(p *entities.DonutParameters, closer *astikit.Closer) *astiav.Dictionary {
	var dic *astiav.Dictionary
	if len(p.Recipe.Input.Options) > 0 {
		dic = &astiav.Dictionary{}
		closer.Add(dic.Free)

		for k, v := range p.Recipe.Input.Options {
			dic.Set(k.String(), v, 0)
		}
	}
	return dic
}

func (c *LibAVFFmpegStreamer) defineAudioDuration(s *streamContext, pkt *astiav.Packet) time.Duration {
	audioDuration := time.Duration(0)
	if s.inputStream.CodecParameters().MediaType() == astiav.MediaTypeAudio {

		// Audio
		//
		// dur = 12.416666ms
		// sample = 48000
		// frameSize = 596 (it can be variable for opus)
		// 1s = dur * (sample/frameSize)
		// ref https://developer.apple.com/documentation/coreaudiotypes/audiostreambasicdescription/1423257-mframesperpacket

		// TODO: properly handle wraparound / roll over
		// or explore av frame_size https://ffmpeg.org/doxygen/trunk/structAVCodecContext.html#aec57f0d859a6df8b479cd93ca3a44a33
		// and libAV pts roll over
		if float64(pkt.Dts())-c.lastAudioFrameDTS > 0 {
			c.currentAudioFrameSize = float64(pkt.Dts()) - c.lastAudioFrameDTS
		}

		c.lastAudioFrameDTS = float64(pkt.Dts())
		sampleRate := float64(s.encCodecContext.SampleRate())
		audioDuration = time.Duration((c.currentAudioFrameSize / sampleRate) * float64(time.Second))
	}
	return audioDuration
}

func (c *LibAVFFmpegStreamer) defineVideoDuration(s *streamContext, _ *astiav.Packet) time.Duration {
	videoDuration := time.Duration(0)
	if s.inputStream.CodecParameters().MediaType() == astiav.MediaTypeVideo {
		// Video
		//
		// dur = 0,033333
		// sample = 30
		// frameSize = 1
		// 1s = dur * (sample/frameSize)

		// we're assuming fixed video frame rate
		videoDuration = time.Duration((float64(1) / float64(s.inputStream.AvgFrameRate().Num())) * float64(time.Second))
	}
	return videoDuration
}

// TODO: move this either to a mapper or make a PR for astiav
func (*LibAVFFmpegStreamer) libAVLogToString(l astiav.LogLevel) string {
	const _Ciconst_AV_LOG_DEBUG = 0x30
	const _Ciconst_AV_LOG_ERROR = 0x10
	const _Ciconst_AV_LOG_FATAL = 0x8
	const _Ciconst_AV_LOG_INFO = 0x20
	const _Ciconst_AV_LOG_PANIC = 0x0
	const _Ciconst_AV_LOG_QUIET = -0x8
	const _Ciconst_AV_LOG_VERBOSE = 0x28
	const _Ciconst_AV_LOG_WARNING = 0x18
	switch l {
	case _Ciconst_AV_LOG_WARNING:
		return "WARN"
	case _Ciconst_AV_LOG_VERBOSE:
		return "VERBOSE"
	case _Ciconst_AV_LOG_QUIET:
		return "QUIET"
	case _Ciconst_AV_LOG_PANIC:
		return "PANIC"
	case _Ciconst_AV_LOG_INFO:
		return "INFO"
	case _Ciconst_AV_LOG_FATAL:
		return "FATAL"
	case _Ciconst_AV_LOG_DEBUG:
		return "DEBUG"
	case _Ciconst_AV_LOG_ERROR:
		return "ERROR"
	default:
		return "UNKNOWN LEVEL"
	}
}
