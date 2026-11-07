//go:build linux || freebsd

package libnetwork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// createNetnsDir creates the directory the network namespaces of sandboxes
// are kept in, if one is configured.
func (c *Controller) createNetnsDir() error {
	if c.cfg.NetnsDir == "" {
		return nil
	}
	if err := os.MkdirAll(c.cfg.NetnsDir, 0o755); err != nil {
		return fmt.Errorf("creating netns directory: %w", err)
	}
	return nil
}

// requireNetnsDir returns an error if no directory is configured for the
// network namespaces of sandboxes.
func (c *Controller) requireNetnsDir() error {
	if c.cfg.NetnsDir == "" {
		return errors.New("no netns directory is configured (see config.OptionNetnsDir)")
	}
	return nil
}

// sandboxKey returns the key of the sandbox with the given ID: the path of
// its network namespace.
func (c *Controller) sandboxKey(sandboxID string) string {
	return filepath.Join(c.cfg.NetnsDir, sandboxID[:min(len(sandboxID), 12)])
}
