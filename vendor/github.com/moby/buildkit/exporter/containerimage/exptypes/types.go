package exptypes

import specs "github.com/opencontainers/image-spec/specs-go/v1"

const ExporterImageConfigKey = "containerimage.config"
const ExporterInlineCache = "containerimage.inlinecache"
const ExporterPlatformsKey = "refs.platforms"

type Platforms struct {
	Platforms []Platform
}

type Platform struct {
	ID       string
	Platform specs.Platform
}
