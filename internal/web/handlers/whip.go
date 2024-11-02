package handlers

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

type WHIPHandler struct {
	l          *zap.SugaredLogger
	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP
}

// NewWHIPHandler creates a new WHIP handler with the given dependencies
func NewWHIPHandler(
	log *zap.SugaredLogger,
	tm *TrackManager, // Inject TrackManager instead of individual tracks
) *WHIPHandler {
	return &WHIPHandler{
		l:          log,
		videoTrack: tm.GetVideoTrack(),
		audioTrack: tm.GetAudioTrack(),
	}
}

func (h *WHIPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	h.l.Infof("Received WHIP Offer SDP:\n%s\n", string(offer))

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

	// Setup the codecs
	if err = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			Channels:    0,
			SDPFmtpLine: "",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("failed to register video codec: %w", err)
	}

	if err = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to register audio codec: %w", err)
	}

	// Create and configure interceptor registry
	i := &interceptor.Registry{}
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		return fmt.Errorf("failed to create PLI interceptor: %w", err)
	}
	i.Add(intervalPliFactory)

	if err = webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return fmt.Errorf("failed to register default interceptors: %w", err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfiguration)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Add ICE candidate logging
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		h.l.Infof("Server ICE candidate (WHIP): Protocol: %s, Address: %s, Port: %d",
			candidate.Protocol,
			candidate.Address,
			candidate.Port)
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		h.l.Infof("ICE Connection State has changed (WHIP): %s", connectionState.String())
	})

	// Add transceivers
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("failed to add video transceiver: %w", err)
	}

	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to add audio transceiver: %w", err)
	}

	// Handle incoming tracks
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					h.l.Errorf("Failed to read RTP packet: %v", err)
					return
				}

				var writeErr error
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					writeErr = h.videoTrack.WriteRTP(pkt)
				} else if track.Kind() == webrtc.RTPCodecTypeAudio {
					writeErr = h.audioTrack.WriteRTP(pkt)
				}

				if writeErr != nil {
					h.l.Errorf("Failed to write RTP packet: %v", writeErr)
					return
				}
			}
		}()
	})

	return h.writeAnswer(w, peerConnection, offer)
}

func (h *WHIPHandler) writeAnswer(w http.ResponseWriter, peerConnection *webrtc.PeerConnection, offer []byte) error {
	// Validate SDP offer
	sdpOffer := string(offer)
	if sdpOffer == "" || !strings.Contains(sdpOffer, "ice-ufrag") {
		return fmt.Errorf("invalid SDP offer: missing ICE credentials")
	}

	// Set the remote description
	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
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

	// Set the local description
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("failed to set local description: %w", err)
	}

	// Block until ICE Gathering is complete
	<-gatherComplete

	// Set WHIP response headers
	w.Header().Add("Location", "/whip")
	w.WriteHeader(http.StatusCreated)

	// Write the answer to the response
	_, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP)
	if err != nil {
		return fmt.Errorf("failed to write response: %w", err)
	}

	return nil
}
