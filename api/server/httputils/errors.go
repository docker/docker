package httputils // import "github.com/docker/docker/api/server/httputils"
import (
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/errdefs/adapter"
)

// GetHTTPErrorStatusCode retrieves status code from error message.
func GetHTTPErrorStatusCode(err error) int {
	return errdefs.GetHTTPErrorStatusCode(err, adapter.All()...)
}
