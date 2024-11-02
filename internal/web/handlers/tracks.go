package handlers

import (
	"fmt"

	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

// TrackManager handles the creation and management of shared WebRTC tracks
type TrackManager struct {
	VideoTrack *webrtc.TrackLocalStaticRTP
	AudioTrack *webrtc.TrackLocalStaticRTP
	logger     *zap.SugaredLogger
}

// NewTrackManager creates a new TrackManager instance with shared video and audio tracks
func NewTrackManager(logger *zap.SugaredLogger) (*TrackManager, error) {
	// Create a video track for H264
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			Channels:    0,
			SDPFmtpLine: "",
		},
		"video",
		"pion-video",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create video track: %w", err)
	}

	// Create an audio track for Opus
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		"audio",
		"pion-audio",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create audio track: %w", err)
	}

	return &TrackManager{
		VideoTrack: videoTrack,
		AudioTrack: audioTrack,
		logger:     logger,
	}, nil
}

// GetVideoTrack returns the shared video track
func (tm *TrackManager) GetVideoTrack() *webrtc.TrackLocalStaticRTP {
	return tm.VideoTrack
}

// GetAudioTrack returns the shared audio track
func (tm *TrackManager) GetAudioTrack() *webrtc.TrackLocalStaticRTP {
	return tm.AudioTrack
}

// Close cleans up any resources used by the TrackManager
func (tm *TrackManager) Close() error {
	// Currently, TrackLocalStaticRTP doesn't require explicit cleanup
	// but we include this method for future-proofing and interface consistency
	tm.logger.Info("Closing track manager")
	return nil
}

// GetTracks returns both video and audio tracks
func (tm *TrackManager) GetTracks() (*webrtc.TrackLocalStaticRTP, *webrtc.TrackLocalStaticRTP) {
	return tm.VideoTrack, tm.AudioTrack
}

// IsActive returns true if both tracks are properly initialized
func (tm *TrackManager) IsActive() bool {
	return tm.VideoTrack != nil && tm.AudioTrack != nil
}
