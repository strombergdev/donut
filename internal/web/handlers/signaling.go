package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/flavioribeiro/donut/internal/controllers"
	"github.com/flavioribeiro/donut/internal/controllers/engine"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

type SignalingHandler struct {
	c                *entities.Config
	l                *zap.SugaredLogger
	webRTCController *controllers.WebRTCController
	mapper           *mapper.Mapper
	donut            *engine.DonutEngineController
}

func NewSignalingHandler(
	c *entities.Config,
	log *zap.SugaredLogger,
	webRTCController *controllers.WebRTCController,
	mapper *mapper.Mapper,
	donut *engine.DonutEngineController,
) *SignalingHandler {
	return &SignalingHandler{
		c:                c,
		l:                log,
		webRTCController: webRTCController,
		mapper:           mapper,
		donut:            donut,
	}
}

func (h *SignalingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	params, err := h.createAndValidateParams(w, r)
	if err != nil {
		return err
	}

	// It decides which prober and streamer should be used based on the parameters (server-side protocol).
	donutEngine, err := h.donut.EngineFor(&params)
	if err != nil {
		return err
	}

	// real stream info from server
	serverStreamInfo, err := donutEngine.Prober().StreamInfo(&params)
	if err != nil {
		return err
	}

	// client stream info support from the client (browser)
	// TODO: evaluate to move this code either inside webrtc or to a prober
	clientStreamInfo, err := h.mapper.FromWebRTCSessionDescriptionToStreamInfo(params.Offer)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	peer, err := h.webRTCController.CreatePeerConnection(cancel)
	if err != nil {
		return err
	}

	// TODO: introduce a mode to deal with transcoding recipes
	// selects proper media that client and server has adverted.
	// donutEngine preferable vp8, ogg???
	// From: [] To: [] or Transcode:[], Bypass: []
	// libav_streamer.go, libav_streamer_format.go, libav_streamer_codec.go...
	// reads from Server (input) and generates h264 raw, and ogg and send it with timing attributes
	compatibleStreams, ok := donutEngine.CompatibleStreamsFor(serverStreamInfo, clientStreamInfo)
	if !ok {
		h.l.Info("we must transcode")
	}

	if len(compatibleStreams) == 0 {
		return entities.ErrMissingCompatibleStreams
	}

	var videoTrack *webrtc.TrackLocalStaticSample
	// var audioTrack *webrtc.TrackLocalStaticSample

	for _, st := range compatibleStreams {
		// TODO: make the mapping less dependent on type
		if st.Type == entities.VideoType {
			videoTrack, err = h.webRTCController.CreateTrack(
				peer,
				st,
				string(st.Type), // "video" or "audio"
				params.SRTStreamID,
			)
			if err != nil {
				return err
			}

		}
		// if st.Type == entities.AudioType {
	}

	metadataSender, err := h.webRTCController.CreateDataChannel(peer, entities.MetadataChannelID)
	if err != nil {
		return err
	}

	if err = h.webRTCController.SetRemoteDescription(peer, params.Offer); err != nil {
		return err
	}

	localDescription, err := h.webRTCController.GatheringWebRTC(peer)
	if err != nil {
		return err
	}

	go donutEngine.Streamer().Stream(&entities.DonutParameters{
		Cancel: cancel,
		Ctx:    ctx,

		// TODO: add an UI element for the sub-type (format) when input is srt://
		// We're assuming that SRT is carrying mpegts.
		StreamFormat: "mpegts",
		StreamID:     params.SRTStreamID,
		StreamURL:    fmt.Sprintf("srt://%s:%d", params.SRTHost, params.SRTPort),

		OnClose: func() {
			cancel()
			peer.Close()
		},
		OnError: func(err error) {
			h.l.Errorw("error while streaming", "error", err)
		},
		OnStream: func(st *entities.Stream) {
			h.sendStreamInfoToMetadata(st, metadataSender)
		},
		OnVideoFrame: func(data []byte, c entities.MediaFrameContext) error {
			return h.webRTCController.SendVideoSample(videoTrack, data, c)
		},
		OnAudioFrame: func(data []byte, c entities.MediaFrameContext) error {
			// TODO: implement
			return nil
		},
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	err = json.NewEncoder(w).Encode(*localDescription)
	if err != nil {
		return err
	}

	return nil
}

func (h *SignalingHandler) sendStreamInfoToMetadata(st *entities.Stream, md *webrtc.DataChannel) {
	msg := h.mapper.FromStreamToEntityMessage(*st)
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		h.l.Errorw("error marshalling stream message", "error", err)
		return
	}
	md.SendText(string(msgBytes))
}

func (h *SignalingHandler) createAndValidateParams(w http.ResponseWriter, r *http.Request) (entities.RequestParams, error) {
	if r.Method != http.MethodPost {
		return entities.RequestParams{}, entities.ErrHTTPPostOnly
	}

	params := entities.RequestParams{}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		return entities.RequestParams{}, err
	}
	if err := params.Valid(); err != nil {
		return entities.RequestParams{}, err
	}

	return params, nil
}
