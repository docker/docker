//go:build (linux && exclude_disk_quota) || (linux && !cgo) || !linux

package quota

func NewControl(basePath string) (*Control, error) {
	return nil, ErrQuotaNotSupported
}

// SetQuota - assign a unique project id to directory and set the quota limits
// for that project id
func (q *Control) SetQuota(targetPath string, quota Quota) error {
	return ErrQuotaNotSupported
}

// GetQuota - get the quota limits of a directory that was configured with SetQuota
func (q *Control) GetQuota(targetPath string, quota *Quota) error {
	return ErrQuotaNotSupported
}

// RemoveQuota - clear the quota limit for the given path
func (q *Control) RemoveQuota(targetPath string) error {
	return ErrQuotaNotSupported
}
