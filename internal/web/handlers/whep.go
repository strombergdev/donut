package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/flavioribeiro/donut/internal/controllers/engine"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	webrtc3 "github.com/pion/webrtc/v3"
	webrtc "github.com/pion/webrtc/v4" // or
	"github.com/pion/webrtc/v4/pkg/media"
	"go.uber.org/zap"
)

var peerConnectionConfiguration = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	},
}

type WHEPHandler struct {
	c          *entities.Config
	l          *zap.SugaredLogger
	mapper     *mapper.Mapper
	donut      *engine.DonutEngineController
	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP
}

func NewWHEPHandler(
	c *entities.Config,
	log *zap.SugaredLogger,
	mapper *mapper.Mapper,
	donut *engine.DonutEngineController,
	tm *TrackManager,
) *WHEPHandler {
	return &WHEPHandler{
		c:          c,
		l:          log,
		mapper:     mapper,
		donut:      donut,
		videoTrack: tm.GetVideoTrack(),
		audioTrack: tm.GetAudioTrack(),
	}
}

func (h *WHEPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	// Read and log the offer details
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	h.l.Infof("Received WHEP Offer SDP:\n%s\n", string(offer))

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfiguration)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Add ICE candidate logging
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		h.l.Infof("Server ICE candidate (WHEP): Protocol: %s, Address: %s, Port: %d",
			candidate.Protocol,
			candidate.Address,
			candidate.Port)
	})

	// Log when ICE connection state changes
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		h.l.Infof("ICE Connection State has changed (WHEP): %s", connectionState.String())
	})

	params, err := h.createAndValidateParams(r)

	donutEngine, err := h.donut.EngineFor(&params)
	if err != nil {
		return err
	}
	h.l.Infof("DonutEngine %#v", donutEngine)

	// server side media info
	serverStreamInfo, err := donutEngine.ServerIngredients()
	if err != nil {
		return err
	}
	h.l.Infof("ServerIngredients %#v", serverStreamInfo)

	// client side media support
	clientStreamInfo, err := donutEngine.ClientIngredients()
	if err != nil {
		return err
	}
	h.l.Infof("ClientIngredients %#v", clientStreamInfo)

	donutRecipe, err := donutEngine.RecipeFor(serverStreamInfo, clientStreamInfo)
	if err != nil {
		return err
	}
	h.l.Infof("DonutRecipe %#v", donutRecipe)

	// We can't defer calling cancel here because it'll live alongside the stream.
	ctx, cancel := context.WithCancel(context.Background())

	// Create video and audio tracks for this connection
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: "video/h264"},
		"video",
		"pion-rtsp",
	)
	if err != nil {
		return fmt.Errorf("failed to create video track: %w", err)
	}
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"},
		"audio",
		"pion-rtsp",
	)
	if err != nil {
		return fmt.Errorf("failed to create audio track: %w", err)
	}

	// Add tracks to peer connection
	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("failed to add video track: %w", err)
	}
	audioRtpSender, err := peerConnection.AddTrack(audioTrack)
	if err != nil {
		return fmt.Errorf("failed to add audio track: %w", err)
	}

	go donutEngine.Serve(&entities.DonutParameters{
		Cancel: cancel,
		Ctx:    ctx,
		Recipe: *donutRecipe,
		OnClose: func() {
			cancel()
			peerConnection.Close()
		},
		OnError: func(err error) {
			h.l.Errorw("error while streaming", "error", err)
		},
		OnVideoFrame: func(data []byte, c entities.MediaFrameContext) error {
			if err := videoTrack.WriteSample(media.Sample{
				Data:     data,
				Duration: c.Duration,
			}); err != nil {
				return fmt.Errorf("failed to write video: %w", err)
			}
			return nil
		},
		OnAudioFrame: func(data []byte, c entities.MediaFrameContext) error {
			if err := audioTrack.WriteSample(media.Sample{
				Data:     data,
				Duration: c.Duration,
			}); err != nil {
				return fmt.Errorf("failed to write audio: %w", err)
			}
			return nil
		},
	})

	// Handle RTCP packets
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				h.l.Errorf("Failed to read video RTCP: %v", rtcpErr)
				return
			}
		}
	}()

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := audioRtpSender.Read(rtcpBuf); rtcpErr != nil {
				h.l.Errorf("Failed to read audio RTCP: %v", rtcpErr)
				return
			}
		}
	}()

	// Add this to the ServeHTTP function after creating the peer connection
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		h.l.Infof("Got track: %s (%s)", track.ID(), track.Kind())
	})

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		h.l.Infof("Connection state changed: %s", state.String())
	})

	h.writeAnswer(w, peerConnection, offer)
	return nil
}

func (h *WHEPHandler) writeAnswer(w http.ResponseWriter, peerConnection *webrtc.PeerConnection, offer []byte) error {
	// Validate SDP offer
	sdpOffer := string(offer)
	if sdpOffer == "" || !strings.Contains(sdpOffer, "ice-ufrag") {
		return fmt.Errorf("invalid SDP offer: missing ICE credentials")
	}

	// Set the handler for ICE connection state
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		h.l.Infof("ICE Connection State has changed: %s", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			if err := peerConnection.Close(); err != nil {
				h.l.Errorf("Failed to close peer connection: %v", err)
			}
		}
	})

	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offer),
	}); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
	}

	if err = peerConnection.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("failed to set local description: %w", err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	<-gatherComplete

	// WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Add("Location", "/whep")
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	_, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP)
	if err != nil {
		return fmt.Errorf("failed to write response: %w", err)
	}

	return nil
}

func (h *WHEPHandler) createAndValidateParams(r *http.Request) (entities.RequestParams, error) {
	if r.Method != http.MethodPost {
		return entities.RequestParams{}, entities.ErrHTTPPostOnly
	}

	// For WHEP, we'll use the configured default stream URL and ID
	params := entities.RequestParams{
		StreamURL: h.c.DefaultStreamURL,
		StreamID:  h.c.DefaultStreamID,
	}

	// Read the SDP offer from the request body
	offerBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return entities.RequestParams{}, fmt.Errorf("failed to read offer: %w", err)
	}

	// Parse the SDP offer using v4
	params.Offer = webrtc3.SessionDescription{
		Type: webrtc3.SDPTypeOffer,
		SDP:  string(offerBytes),
	}

	if err := params.Valid(); err != nil {
		return entities.RequestParams{}, err
	}

	return params, nil
}
