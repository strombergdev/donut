package handlers

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/flavioribeiro/donut/internal/controllers"
	"github.com/flavioribeiro/donut/internal/controllers/engine"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
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
	body, err := ioutil.ReadAll(r.Body)
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

	// Get server-side media info
	serverStreamInfo, err := donutEngine.ServerIngredients()
	if err != nil {
		h.l.Errorw("Failed to get server stream info", "error", err)
		return fmt.Errorf("failed to get server stream info: %w", err)
	}
	h.l.Infof("ServerIngredients: %#v", serverStreamInfo)

	// Get client-side media support
	clientStreamInfo, err := donutEngine.ClientIngredients()
	if err != nil {
		h.l.Errorw("Failed to get client stream info", "error", err)
		return fmt.Errorf("failed to get client stream info: %w", err)
	}
	h.l.Infof("ClientIngredients: %#v", clientStreamInfo)

	// Create DonutRecipe
	donutRecipe, err := donutEngine.RecipeFor(serverStreamInfo, clientStreamInfo)
	if err != nil {
		h.l.Errorw("Failed to create DonutRecipe", "error", err)
		return fmt.Errorf("failed to create DonutRecipe: %w", err)
	}
	h.l.Infof("DonutRecipe: %#v", donutRecipe)

	// Context to manage streaming lifecycle
	ctx, cancel := context.WithCancel(context.Background())

	// Set up WebRTC connection
	webRTCResponse, err := h.webRTCController.Setup(cancel, donutRecipe, params)
	if err != nil {
		cancel()
		h.l.Errorw("Failed to set up WebRTC", "error", err)
		return fmt.Errorf("failed to set up WebRTC: %w", err)
	}
	h.l.Infof("WebRTCResponse: %#v", webRTCResponse)

	// Add connection timeout headers
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Keep-Alive", "timeout=300")

	// Create a done channel to track connection state
	done := make(chan struct{})

	// Handle client disconnect
	go func() {
		<-r.Context().Done()
		h.l.Info("Client disconnected")
		close(done)
		cancel()
	}()

	// Start streaming media
	go donutEngine.Serve(&entities.DonutParameters{
		Cancel: cancel,
		Ctx:    ctx,
		Recipe: *donutRecipe,
		OnClose: func() {
			cancel()
			webRTCResponse.Connection.Close()
			close(done)
		},
		OnError: func(err error) {
			if !errors.Is(err, context.Canceled) {
				h.l.Errorw("Error while streaming", "error", err)
			}
		},
		OnStream: func(st *entities.Stream) error {
			return h.webRTCController.SendMetadata(webRTCResponse.Data, st)
		},
		OnVideoFrame: func(data []byte, c entities.MediaFrameContext) error {
			return h.webRTCController.SendMediaSample(webRTCResponse.Video, data, c)
		},
		OnAudioFrame: func(data []byte, c entities.MediaFrameContext) error {
			return h.webRTCController.SendMediaSample(webRTCResponse.Audio, data, c)
		},
	})

	// Send SDP answer back to the client
	answerSDP := webRTCResponse.LocalSDP.SDP
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(answerSDP)))
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(answerSDP)); err != nil {
		cancel()
		return fmt.Errorf("failed to write SDP answer: %w", err)
	}

	// Keep the connection alive
	<-done
	return nil
}
