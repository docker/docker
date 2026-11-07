package vfs

import (
	"context"
	"errors"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/quota"
)

type driverQuota struct {
	quotaCtl *quota.Control
	quotaOpt quota.Quota
}

func setupDriverQuota(driver *Driver) {
	if quotaCtl, err := quota.NewControl(driver.home); err == nil {
		driver.quotaCtl = quotaCtl
	} else if !errors.Is(err, quota.ErrQuotaNotSupported) {
		log.G(context.TODO()).Warnf("Unable to setup quota: %v\n", err)
	}
}

func (d *Driver) setQuotaOpt(size uint64) error {
	d.quotaOpt.Size = size
	return nil
}

func (d *Driver) getQuotaOpt() uint64 {
	return d.quotaOpt.Size
}

func (d *Driver) setupQuota(dir string, size uint64) error {
	return d.quotaCtl.SetQuota(dir, quota.Quota{Size: size})
}

func (d *Driver) removeQuota(dir string) {
	if d.quotaCtl == nil {
		return
	}
	if err := d.quotaCtl.RemoveQuota(dir); err != nil {
		log.G(context.TODO()).Warnf("Failed to remove quota for %v: %v", dir, err)
	}
}

func (d *Driver) quotaSupported() bool {
	return d.quotaCtl != nil
}
