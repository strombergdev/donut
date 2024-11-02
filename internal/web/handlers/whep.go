package handlers

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/flavioribeiro/donut/internal/controllers"
	"github.com/flavioribeiro/donut/internal/controllers/engine"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	"github.com/pion/webrtc/v4"
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
	c                *entities.Config
	l                *zap.SugaredLogger
	webRTCController *controllers.WebRTCController
	mapper           *mapper.Mapper
	donut            *engine.DonutEngineController
	videoTrack       *webrtc.TrackLocalStaticRTP
	audioTrack       *webrtc.TrackLocalStaticRTP
}

func NewWHEPHandler(
	c *entities.Config,
	log *zap.SugaredLogger,
	webRTCController *controllers.WebRTCController,
	mapper *mapper.Mapper,
	donut *engine.DonutEngineController,
	tm *TrackManager,
) *WHEPHandler {
	return &WHEPHandler{
		c:                c,
		l:                log,
		webRTCController: webRTCController,
		mapper:           mapper,
		donut:            donut,
		videoTrack:       tm.GetVideoTrack(),
		audioTrack:       tm.GetAudioTrack(),
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

	// Add Video Track that is being written to from WHIP Session
	rtpSender, err := peerConnection.AddTrack(h.videoTrack)
	if err != nil {
		return fmt.Errorf("failed to add video track: %w", err)
	}

	// Add Audio Track that is being written to from WHIP Session
	audioRtpSender, err := peerConnection.AddTrack(h.audioTrack)
	if err != nil {
		return fmt.Errorf("failed to add audio track: %w", err)
	}

	// Read incoming RTCP packets for both tracks
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

	return h.writeAnswer(w, peerConnection, offer)
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
