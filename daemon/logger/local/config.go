package local

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/docker/go-units"
)

// CreateConfig is used to configure new instances of driver
type CreateConfig struct {
	DisableCompression bool
	MaxFileSize        int64
	MaxFileCount       int
}

func newConfig(opts map[string]string) (*CreateConfig, error) {
	cfg := &CreateConfig{
		MaxFileSize:        defaultMaxFileSize,
		MaxFileCount:       defaultMaxFileCount,
		DisableCompression: !defaultCompressLogs,
	}
	if v, ok := opts["max-size"]; ok {
		maxSize, err := units.FromHumanSize(v)
		if err != nil {
			return nil, fmt.Errorf("invalid value for max-size: %s: %w", v, err)
		}
		cfg.MaxFileSize = maxSize
	}

	if v, ok := opts["max-file"]; ok {
		maxFile, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid value for max-file: %s: %w", v, err)
		}
		cfg.MaxFileCount = maxFile
	}

	if v, ok := opts["compress"]; ok {
		compressLogs, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid boolean value for compress: %w", err)
		}
		cfg.DisableCompression = !compressLogs
	}

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validateConfig(cfg *CreateConfig) error {
	if cfg.MaxFileSize < 0 {
		return errors.New("max-size must not be negative")
	}
	if cfg.MaxFileCount < 0 {
		return errors.New("max-file must not be negative")
	}
	if !cfg.DisableCompression && cfg.MaxFileCount <= 1 {
		// compression is applied to rotated files, so doesn't apply if rotation is not used.
		return errors.New("compression cannot be enabled when max file count is 1")
	}
	return nil
}
