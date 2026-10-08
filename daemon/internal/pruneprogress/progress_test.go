package pruneprogress

import (
	"testing"

	"github.com/moby/moby/api/types/prune"
	"gotest.tools/v3/assert"
)

func TestNotify(t *testing.T) {
	Notify(nil, "ignored", prune.Deleted)
	var received []prune.Object
	Notify(func(object prune.Object) { received = append(received, object) }, "removed", prune.Deleted)
	assert.DeepEqual(t, received, []prune.Object{{ID: "removed", Action: prune.Deleted}})
}
