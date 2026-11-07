//go:build !linux && !freebsd

package libnetwork

func (c *Controller) createNetnsDir() error {
	return nil
}

func (c *Controller) requireNetnsDir() error {
	return nil
}

// sandboxKey returns the key of the sandbox with the given ID.
func (c *Controller) sandboxKey(sandboxID string) string {
	return sandboxID
}
