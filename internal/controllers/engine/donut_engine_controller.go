package engine

import (
	"fmt"
	"strings"

	"github.com/flavioribeiro/donut/internal/controllers/probers"
	"github.com/flavioribeiro/donut/internal/controllers/streamers"
	"github.com/flavioribeiro/donut/internal/entities"
	"github.com/flavioribeiro/donut/internal/mapper"
	"go.uber.org/fx"
)

type DonutEngine interface {
	Appetizer() (entities.DonutAppetizer, error)
	ServerIngredients() (*entities.StreamInfo, error)
	ClientIngredients() (*entities.StreamInfo, error)
	RecipeFor(server, client *entities.StreamInfo) (*entities.DonutRecipe, error)
	Serve(p *entities.DonutParameters)
}

type DonutEngineParams struct {
	fx.In
	Streamers []streamers.DonutStreamer `group:"streamers"`
	Probers   []probers.DonutProber     `group:"probers"`
	Mapper    *mapper.Mapper
}

type DonutEngineController struct {
	p DonutEngineParams
}

func NewDonutEngineController(p DonutEngineParams) *DonutEngineController {
	return &DonutEngineController{p}
}

func (c *DonutEngineController) EngineFor(req *entities.RequestParams) (DonutEngine, error) {
	prober := c.selectProberFor(req)
	if prober == nil {
		return nil, fmt.Errorf("request %v: not fulfilled. error %w", req, entities.ErrMissingProber)
	}

	streamer := c.selectStreamerFor(req)
	if streamer == nil {
		return nil, fmt.Errorf("request %v: not fulfilled. error %w", req, entities.ErrMissingStreamer)
	}

	return &donutEngine{
		prober:   prober,
		streamer: streamer,
		mapper:   c.p.Mapper,
		req:      req,
	}, nil
}

// TODO: try to use generics
func (c *DonutEngineController) selectProberFor(req *entities.RequestParams) probers.DonutProber {
	for _, p := range c.p.Probers {
		if p.Match(req) {
			return p
		}
	}
	return nil
}

// TODO: try to use generics
func (c *DonutEngineController) selectStreamerFor(req *entities.RequestParams) streamers.DonutStreamer {
	for _, p := range c.p.Streamers {
		if p.Match(req) {
			return p
		}
	}
	return nil
}

type donutEngine struct {
	prober   probers.DonutProber
	streamer streamers.DonutStreamer
	mapper   *mapper.Mapper
	req      *entities.RequestParams
}

func (d *donutEngine) ServerIngredients() (*entities.StreamInfo, error) {
	appetizer, err := d.Appetizer()
	if err != nil {
		return nil, err
	}
	return d.prober.StreamInfo(appetizer)
}

func (d *donutEngine) ClientIngredients() (*entities.StreamInfo, error) {
	return d.mapper.FromWebRTCSessionDescriptionToStreamInfo(d.req.Offer)
}

func (d *donutEngine) Serve(p *entities.DonutParameters) {
	d.streamer.Stream(p)
}

func (d *donutEngine) RecipeFor(server, client *entities.StreamInfo) (*entities.DonutRecipe, error) {
	appetizer, err := d.Appetizer()
	if err != nil {
		return nil, err
	}

	r := &entities.DonutRecipe{
		Input: appetizer,
		Video: entities.DonutMediaTask{
			Action:               entities.DonutBypass,
			Codec:                entities.H264,
			DonutBitStreamFilter: &entities.DonutH264AnnexB,
		},
		Audio: entities.DonutMediaTask{
			Action:            entities.DonutTranscode,
			Codec:             entities.Opus,
			DonutStreamFilter: entities.AudioResamplerFilter(48000),
			CodecContextOptions: []entities.LibAVOptionsCodecContext{
				entities.SetSampleRate(48000),
				entities.SetBitRate(128000),
				entities.SetSampleFormat("s16"),
			},
		},
	}

	return r, nil
}

func (d *donutEngine) Appetizer() (entities.DonutAppetizer, error) {
	isRTMP := strings.Contains(strings.ToLower(d.req.StreamURL), "rtmp")
	isSRT := strings.Contains(strings.ToLower(d.req.StreamURL), "srt")

	if isRTMP {
		return entities.DonutAppetizer{
			URL: fmt.Sprintf("%s/%s", d.req.StreamURL, d.req.StreamID),
			Options: map[entities.DonutInputOptionKey]string{
				entities.DonutRTMPLive: "live",
			},
			Format: "flv",
		}, nil
	}

	if isSRT {
		return entities.DonutAppetizer{
			URL:    d.req.StreamURL,
			Format: "mpegts", // TODO: check how to get format for srt
			Options: map[entities.DonutInputOptionKey]string{
				entities.DonutSRTStreamID:  d.req.StreamID,
				entities.DonutSRTTranstype: "live",
				entities.DonutSRTsmoother:  "live",
			},
		}, nil
	}

	return entities.DonutAppetizer{}, entities.ErrUnsupportedStreamURL
}
