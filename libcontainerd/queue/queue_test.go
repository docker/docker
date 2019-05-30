package queue // import "github.com/docker/docker/libcontainerd/queue"

import (
	"testing"
	"time"

	"gotest.tools/assert"
)

func TestSerialization(t *testing.T) {
	var (
		q             Queue
		serialization = 1
	)

	q.Append("aaa", func() {
		//simulate a long time task
		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, serialization, 1)
		serialization = 2
	})
	q.Append("aaa", func() {
		assert.Equal(t, serialization, 2)
		serialization = 3
	})
	q.Append("aaa", func() {
		assert.Equal(t, serialization, 3)
		serialization = 4
	})
	time.Sleep(20 * time.Millisecond)
}
