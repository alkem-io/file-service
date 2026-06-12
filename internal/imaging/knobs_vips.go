//go:build vips

package imaging

import "github.com/davidbyttow/govips/v2/vips"

func configureVips(cfg StreamingConfig) {
	if cfg.DiscThreshold > 0 {
		vips.SetStreamDiscThreshold(cfg.DiscThreshold)
	}
	if cfg.ScratchDir != "" {
		vips.SetStreamScratchDir(cfg.ScratchDir)
	}
	if cfg.PipeReadLimit > 0 {
		vips.SetPipeReadLimit(cfg.PipeReadLimit)
	}
}
