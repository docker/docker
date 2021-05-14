package volumequeue

import (
	"sync"
	"time"
)

// baseRetryInterval is the base interval to retry volume operations. each
// subsequent attempt is exponential from this one
const baseRetryInterval = 100 * time.Millisecond

// maxRetryInterval is the maximum amount of time we will wait between retrying
// volume operations.
const maxRetryInterval = 10 * time.Minute

// vqTimerSource is an interface for creating timers for the volumeQueue
type vqTimerSource interface {
	// NewTimer takes an attempt number and returns a vqClockTrigger which will
	// trigger after a set period based on that attempt number.
	NewTimer(attempt uint) vqTimer
}

// vqTimer is an interface representing a timer. However, the timer
// trigger channel, C, is instead wrapped in a Done method, so that in testing,
// the timer can be substituted for a different object.
type vqTimer interface {
	Done() <-chan time.Time
	Stop() bool
}

// timerSource is an empty struct type which is used to represent the default
// vqTimerSource, which uses time.Timer.
type timerSource struct{}

// NewTimer creates a new timer.
func (timerSource) NewTimer(attempt uint) vqTimer {
	var waitFor time.Duration
	if attempt == 0 {
		waitFor = 0
	} else {
		// bit-shifting the base retry interval will raise it by 2 to the power
		// of attempt. this is an easy way to do an exponent solely with
		// integers
		waitFor = baseRetryInterval << attempt
		if waitFor > maxRetryInterval {
			waitFor = maxRetryInterval
		}
	}
	return timer{Timer: time.NewTimer(waitFor)}
}

// timer wraps a time.Timer to provide a Done method.
type timer struct {
	*time.Timer
}

// Done returns the timer's C channel, which triggers in response to the timer
// expiring.
func (t timer) Done() <-chan time.Time {
	return t.C
}

// VolumeQueue manages the exponential backoff of retrying volumes. it behaves
// somewhat like a priority queue. however, the key difference is that volumes
// which are ready to process or reprocess are read off of an unbuffered
// channel, meaning the order in which ready volumes are processed is at the
// mercy of the golang scheduler. in practice, this does not matter.
type VolumeQueue struct {
	sync.Mutex
	// next returns the next volumeQueueEntry when it is ready.
	next chan *volumeQueueEntry
	// outstanding is the set of all pending volumeQueueEntries, mapped by
	// volume ID.
	outstanding map[string]*volumeQueueEntry
	// stopChan stops the volumeQueue and cancels all entries.
	stopChan chan struct{}

	// timerSource is an object which is used to create the timer for a
	// volumeQueueEntry. it exists so that in testing, the timer can be
	// substituted for an object that we control.
	timerSource vqTimerSource
}

// volumeQueueEntry represents one entry in the volumeQueue
type volumeQueueEntry struct {
	// id is the id of the volume this entry represents. we only need the ID,
	// because the CSI manager will look up the latest revision of the volume
	// before doing any work on it.
	id string
	// attempt is the current retry attempt of the entry.
	attempt uint
	// cancel is a function which is called to abort the retry attempt.
	cancel func()
}

// NewVolumeQueue returns a new VolumeQueue with the default timerSource.
func NewVolumeQueue() *VolumeQueue {
	return &VolumeQueue{
		next:        make(chan *volumeQueueEntry),
		outstanding: make(map[string]*volumeQueueEntry),
		stopChan:    make(chan struct{}),
		timerSource: timerSource{},
	}
}

// Enqueue adds an entry to the VolumeQueue with the specified retry attempt.
// if an entry for the specified id already exists, enqueue will remove it and
// create a new entry.
func (vq *VolumeQueue) Enqueue(id string, attempt uint) {
	// we must lock the volumeQueue when we add entries, because we will be
	// accessing the outstanding map
	vq.Lock()
	defer vq.Unlock()

	if entry, ok := vq.outstanding[id]; ok {
		entry.cancel()
		delete(vq.outstanding, id)
	}

	cancelChan := make(chan struct{})
	v := &volumeQueueEntry{
		id:      id,
		attempt: attempt,
		cancel: func() {
			close(cancelChan)
		},
	}

	t := vq.timerSource.NewTimer(attempt)

	// this goroutine is the meat of the volumeQueue. when the timer triggers,
	// the volume queue entry is written out to the next channel.
	//
	// the nature of the select statement, and of goroutines and of
	// ansynchronous operations means that this is not actually strictly
	// ordered. if several entries are ready, then the one that actually gets
	// dequeued is at the mercy of the golang scheduler.
	//
	// however, the flip side of this is that canceling an entry truly cancels
	// it. because we're blocking on a write attempt, if we cancel, we don't
	// do that write attempt, and there's no need to try to remove from the
	// queue a ready-but-now-canceled entry before it is processed.
	go func() {
		select {
		case <-t.Done():
			// once the timer fires, we will try to write this entry to the
			// next channel. however, because next is unbuffered, if we ended
			// up in a situation where no read occurred, we would be
			// deadlocked. to avoid this, we select on both a vq.next write and
			// on a read from cancelChan, which allows us to abort our write
			// attempt.
			select {
			case vq.next <- v:
			case <-cancelChan:
			}
		case <-cancelChan:
			// the documentation for timer recommends draining the channel like
			// this.
			if !t.Stop() {
				<-t.Done()
			}
		}
	}()

	vq.outstanding[id] = v
}

// Wait returns the ID and attempt number of the next Volume ready to process.
// If no volume is ready, wait blocks until one is ready. if the volumeQueue
// is stopped, wait returns "", 0
func (vq *VolumeQueue) Wait() (string, uint) {
	select {
	case v := <-vq.next:
		vq.Lock()
		defer vq.Unlock()
		// we need to be certain that this entry is the same entry that we
		// read, because otherwise there may be a race.
		//
		// it would be possible for the read from next to succeed, but before
		// the lock is acquired, a new attempt is enqueued. enqueuing the new
		// attempt deletes the old entry before replacing it with the new entry
		// and releasing the lock. then, this routine may acquire the lock, and
		// delete a new entry.
		//
		// in practice, it is unclear if this race could happen or would matter
		// if it did, but always better safe than sorry.
		e, ok := vq.outstanding[v.id]
		if ok && e == v {
			delete(vq.outstanding, v.id)
		}

		return v.id, v.attempt
	case <-vq.stopChan:
		// if the volumeQueue is stopped, then there may be no more writes, so
		// we should return an empty result from wait
		return "", 0
	}
}

// Outstanding returns the number of items outstanding in this queue
func (vq *VolumeQueue) Outstanding() int {
	return len(vq.outstanding)
}

// Stop stops the volumeQueue and cancels all outstanding entries. stop may
// only be called once.
func (vq *VolumeQueue) Stop() {
	vq.Lock()
	defer vq.Unlock()
	close(vq.stopChan)
	for _, entry := range vq.outstanding {
		entry.cancel()
	}
	return
}
