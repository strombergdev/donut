package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/flavioribeiro/donut/internal/controllers"
	"github.com/flavioribeiro/donut/internal/controllers/engine"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

type WHEPHandler struct {
	c                *entities.Config
	l                *zap.SugaredLogger
	webRTCController *controllers.WebRTCController
	mapper           *mapper.Mapper
	donut            *engine.DonutEngineController
}

func NewWHEPHandler(
	c *entities.Config,
	log *zap.SugaredLogger,
	webRTCController *controllers.WebRTCController,
	mapper *mapper.Mapper,
	donut *engine.DonutEngineController,
) *WHEPHandler {
	return &WHEPHandler{
		c:                c,
		l:                log,
		webRTCController: webRTCController,
		mapper:           mapper,
		donut:            donut,
	}
}

func (h *WHEPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	h.l.Infow("WHEP request received",
		"method", r.Method,
		"headers", r.Header,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr)

	// WHEP expects a POST request with 'application/sdp' Content-Type
	if r.Method != http.MethodPost {
		h.l.Errorw("Method not allowed", "method", r.Method)
		return fmt.Errorf("method not allowed: %s", r.Method)
	}

	if r.Header.Get("Content-Type") != "application/sdp" {
		return fmt.Errorf("unsupported media type")
	}

	// Read SDP offer from request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.l.Errorw("Failed to read request body", "error", err)
		return fmt.Errorf("failed to read request body: %w", err)
	}
	defer r.Body.Close()

	// Parse the SDP offer
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(body),
	}

	// Create request parameters
	params := entities.RequestParams{
		StreamURL: h.c.DefaultStreamURL,
		StreamID:  h.c.DefaultStreamID,
		Offer:     offer,
	}

	// Validate parameters
	if err := params.Valid(); err != nil {
		h.l.Errorw("Invalid request parameters", "error", err)
		return fmt.Errorf("invalid request parameters: %w", err)
	}
	h.l.Infof("WHEP RequestParams: %s", params.String())

	// Create DonutEngine
	donutEngine, err := h.donut.EngineFor(&params)
	if err != nil {
		h.l.Errorw("Failed to create DonutEngine", "error", err)
		return fmt.Errorf("failed to create DonutEngine: %w", err)
	}
	h.l.Infof("DonutEngine: %#v", donutEngine)

	// Context to manage streaming lifecycle
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up WebRTC connection
	peerConnection, err := h.setupPeerConnection(&params, &offer)
	if err != nil {
		h.l.Errorw("Failed to set up PeerConnection", "error", err)
		return fmt.Errorf("failed to set up PeerConnection: %w", err)
	}
	defer peerConnection.Close()

	// Send SDP answer back to the client
	answerSDP := peerConnection.LocalDescription().SDP
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(answerSDP)))
	w.Header().Add("Location", r.URL.Path)
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(answerSDP)); err != nil {
		h.l.Errorw("Failed to write SDP answer", "error", err)
		return fmt.Errorf("failed to write SDP answer: %w", err)
	}

	// Start streaming media
	done := make(chan struct{})
	go func() {
		<-r.Context().Done()
		h.l.Info("Client disconnected")
		close(done)
		cancel()
	}()

	go func() {
		err := donutEngine.Serve(&entities.DonutParameters{
			Cancel: cancel,
			Ctx:    ctx,
			Recipe: entities.DonutRecipe{}, // Use appropriate recipe
			OnClose: func() {
				cancel()
				peerConnection.Close()
				close(done)
			},
			OnError: func(err error) {
				if !errors.Is(err, context.Canceled) {
					h.l.Errorw("Error while streaming", "error", err)
				}
			},
		})
		if err != nil {
			h.l.Errorw("Failed to serve DonutEngine", "error", err)
		}
	}()

	// Keep the connection alive
	<-done
	return nil
}

func (h *WHEPHandler) setupPeerConnection(params *entities.RequestParams, offer *webrtc.SessionDescription) (*webrtc.PeerConnection, error) {
	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

	// Register codecs based on DonutEngine capabilities
	if err := h.webRTCController.RegisterCodecs(m); err != nil {
		return nil, err
	}

	// Create an InterceptorRegistry
	i := &interceptor.Registry{}

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	// Create the API object with the MediaEngine and Interceptors
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// Create a new PeerConnection
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}

	// Set the remote SessionDescription
	if err := peerConnection.SetRemoteDescription(*offer); err != nil {
		return nil, err
	}

	// Add transceivers based on the remote description
	for _, media := range offer.MediaDescriptions {
		kind := webrtc.NewRTPCodecType(media.MediaName.Media)
		if _, err := peerConnection.AddTransceiverFromKind(kind); err != nil {
			return nil, err
		}
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	// Set the local SessionDescription
	if err := peerConnection.SetLocalDescription(answer); err != nil {
		return nil, err
	}

	// Wait for ICE gathering to complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	<-gatherComplete

	return peerConnection, nil
}
