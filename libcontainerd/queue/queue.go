package queue // import "github.com/docker/docker/libcontainerd/queue"

import "sync"

// Queue is the structure used for holding functions in a queue.
type Queue struct {
	sync.Mutex
	fns map[string]chan struct{}
}

// Append adds an item to a queue.
func (q *Queue) Append(id string, f func()) {
	q.Lock()
	defer q.Unlock()

	if q.fns == nil {
		q.fns = make(map[string]chan struct{})
	}

	done := make(chan struct{})

	fn, ok := q.fns[id]
	q.fns[id] = done
	go func() {
		if ok {
			<-fn
		}
		f()
		close(done)

		q.Lock()
		if q.fns[id] == done {
			delete(q.fns, id)
		}
		q.Unlock()
	}()
}
