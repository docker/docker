package images

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/distribution/reference"
	"gotest.tools/v3/assert"
)

// failingLeaseManager refuses to create a lease so that pull fails before it
// reaches the distribution layer.
type failingLeaseManager struct {
	leases.Manager
}

var errNoLease = errors.New("no lease for you")

func (failingLeaseManager) Create(context.Context, ...leases.Opt) (leases.Lease, error) {
	return leases.Lease{}, errNoLease
}

// TestPullImageLeaseFailureStopsProgressWriter checks that a pull which fails
// while taking its temporary lease does not leave the progress writer
// goroutine blocked on progressChan.
func TestPullImageLeaseFailureStopsProgressWriter(t *testing.T) {
	ref, err := reference.ParseNormalizedNamed("busybox:latest")
	assert.NilError(t, err)
	svc := &ImageService{leases: failingLeaseManager{}, contentNamespace: t.Name()}

	before := runtime.NumGoroutine()
	err = svc.pullImageWithReference(context.Background(), ref, nil, nil, nil, io.Discard)
	assert.ErrorIs(t, err, errNoLease)

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("progress writer goroutine leaked: %d goroutines before the pull, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
