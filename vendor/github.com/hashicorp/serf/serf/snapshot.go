package serf

import (
	"bufio"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/armon/go-metrics"
)

/*
Serf supports using a "snapshot" file that contains various
transactional data that is used to help Serf recover quickly
and gracefully from a failure. We append member events, as well
as the latest clock values to the file during normal operation,
and periodically checkpoint and roll over the file. During a restore,
we can replay the various member events to recall a list of known
nodes to re-join, as well as restore our clock values to avoid replaying
old events.
*/

const (
	// flushInterval is how often we force a flush of the snapshot file
	flushInterval = 500 * time.Millisecond

	// clockUpdateInterval is how often we fetch the current lamport time of the cluster and write to the snapshot file
	clockUpdateInterval = 500 * time.Millisecond

	// tmpExt is the extention we use for the temporary file during compaction
	tmpExt = ".compact"

	// snapshotErrorRecoveryInterval is how often we attempt to recover from
	// errors writing to the snapshot file.
	snapshotErrorRecoveryInterval = 30 * time.Second

	// eventChSize is the size of the event buffers between Serf and the
	// consuming application. If this is exhausted we will block Serf and Memberlist.
	eventChSize = 2048

	// shutdownFlushTimeout is the time limit to write pending events to the snapshot during a shutdown
	shutdownFlushTimeout = 250 * time.Millisecond

	// snapshotBytesPerNode is an estimated bytes per node to snapshot
	snapshotBytesPerNode = 128

	// snapshotCompactionThreshold is the threshold we apply to
	// the snapshot size estimate (nodes * bytes per node) before compacting.
	snapshotCompactionThreshold = 2
)

// Snapshotter is responsible for ingesting events and persisting
// them to disk, and providing a recovery mechanism at start time.
type Snapshotter struct {
	aliveNodes              map[string]string
	clock                   *LamportClock
	fh                      *os.File
	buffered                *bufio.Writer
	inCh                    <-chan Event
	streamCh                chan Event
	lastFlush               time.Time
	lastClock               LamportTime
	lastEventClock          LamportTime
	lastQueryClock          LamportTime
	leaveCh                 chan struct{}
	leaving                 bool
	logger                  *log.Logger
	minCompactSize          int64
	path                    string
	offset                  int64
	outCh                   chan<- Event
	rejoinAfterLeave        bool
	shutdownCh              <-chan struct{}
	waitCh                  chan struct{}
	lastAttemptedCompaction time.Time
}

// PreviousNode is used to represent the previously known alive nodes
type PreviousNode struct {
	Name string
	Addr string
}

func (p PreviousNode) String() string {
	return fmt.Sprintf("%s: %s", p.Name, p.Addr)
}

// NewSnapshotter creates a new Snapshotter that records events up to a
// max byte size before rotating the file. It can also be used to
// recover old state. Snapshotter works by reading an event channel it returns,
// passing through to an output channel, and persisting relevant events to disk.
// Setting rejoinAfterLeave makes leave not clear the state, and can be used
// if you intend to rejoin the same cluster after a leave.
func NewSnapshotter(path string,
	minCompactSize int,
	rejoinAfterLeave bool,
	logger *log.Logger,
	clock *LamportClock,
	outCh chan<- Event,
	shutdownCh <-chan struct{}) (chan<- Event, *Snapshotter, error) {
	inCh := make(chan Event, eventChSize)
	streamCh := make(chan Event, eventChSize)

	// Try to open the file
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open snapshot: %v", err)
	}

	// Determine the offset
	info, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, nil, fmt.Errorf("failed to stat snapshot: %v", err)
	}
	offset := info.Size()

	// Create the snapshotter
	snap := &Snapshotter{
		aliveNodes:       make(map[string]string),
		clock:            clock,
		fh:               fh,
		buffered:         bufio.NewWriter(fh),
		inCh:             inCh,
		streamCh:         streamCh,
		lastClock:        0,
		lastEventClock:   0,
		lastQueryClock:   0,
		leaveCh:          make(chan struct{}),
		logger:           logger,
		minCompactSize:   int64(minCompactSize),
		path:             path,
		offset:           offset,
		outCh:            outCh,
		rejoinAfterLeave: rejoinAfterLeave,
		shutdownCh:       shutdownCh,
		waitCh:           make(chan struct{}),
	}

	// Recover the last known state
	if err := snap.replay(); err != nil {
		fh.Close()
		return nil, nil, err
	}

	// Start handling new commands
	go snap.teeStream()
	go snap.stream()
	return inCh, snap, nil
}

// LastClock returns the last known clock time
func (s *Snapshotter) LastClock() LamportTime {
	return s.lastClock
}

// LastEventClock returns the last known event clock time
func (s *Snapshotter) LastEventClock() LamportTime {
	return s.lastEventClock
}

// LastQueryClock returns the last known query clock time
func (s *Snapshotter) LastQueryClock() LamportTime {
	return s.lastQueryClock
}

// AliveNodes returns the last known alive nodes
func (s *Snapshotter) AliveNodes() []*PreviousNode {
	// Copy the previously known
	previous := make([]*PreviousNode, 0, len(s.aliveNodes))
	for name, addr := range s.aliveNodes {
		previous = append(previous, &PreviousNode{name, addr})
	}

	// Randomize the order, prevents hot shards
	for i := range previous {
		j := rand.Intn(i + 1)
		previous[i], previous[j] = previous[j], previous[i]
	}
	return previous
}

// Wait is used to wait until the snapshotter finishes shut down
func (s *Snapshotter) Wait() {
	<-s.waitCh
}

// Leave is used to remove known nodes to prevent a restart from
// causing a join. Otherwise nodes will re-join after leaving!
func (s *Snapshotter) Leave() {
	select {
	case s.leaveCh <- struct{}{}:
	case <-s.shutdownCh:
	}
}

// teeStream is a long running routine that is used to copy events
// to the output channel and the internal event handler.
func (s *Snapshotter) teeStream() {
	flushEvent := func(e Event) {
		// Forward to the internal stream, do not block
		select {
		case s.streamCh <- e:
		default:
		}

		// Forward the event immediately, do not block
		if s.outCh != nil {
			select {
			case s.outCh <- e:
			default:
			}
		}
	}

OUTER:
	for {
		select {
		case e := <-s.inCh:
			flushEvent(e)
		case <-s.shutdownCh:
			break OUTER
		}
	}

	// Drain any remaining events before exiting
	for {
		select {
		case e := <-s.inCh:
			flushEvent(e)
		default:
			return
		}
	}
}

// stream is a long running routine that is used to handle events
func (s *Snapshotter) stream() {
	clockTicker := time.NewTicker(clockUpdateInterval)
	defer clockTicker.Stop()

	// flushEvent is used to handle writing out an event
	flushEvent := func(e Event) {
		// Stop recording events after a leave is issued
		if s.leaving {
			return
		}
		switch typed := e.(type) {
		case MemberEvent:
			s.processMemberEvent(typed)
		case UserEvent:
			s.processUserEvent(typed)
		case *Query:
			s.processQuery(typed)
		default:
			s.logger.Printf("[ERR] serf: Unknown event to snapshot: %#v", e)
		}
	}

	for {
		select {
		case <-s.leaveCh:
			s.leaving = true

			// If we plan to re-join, keep our state
			if !s.rejoinAfterLeave {
				s.aliveNodes = make(map[string]string)
			}
			s.tryAppend("leave\n")
			if err := s.buffered.Flush(); err != nil {
				s.logger.Printf("[ERR] serf: failed to flush leave to snapshot: %v", err)
			}
			if err := s.fh.Sync(); err != nil {
				s.logger.Printf("[ERR] serf: failed to sync leave to snapshot: %v", err)
			}

		case e := <-s.streamCh:
			flushEvent(e)

		case <-clockTicker.C:
			s.updateClock()

		case <-s.shutdownCh:
			// Setup a timeout
			flushTimeout := time.After(shutdownFlushTimeout)

			// Snapshot the clock
			s.updateClock()

			// Clear out the buffers
		FLUSH:
			for {
				select {
				case e := <-s.streamCh:
					flushEvent(e)
				case <-flushTimeout:
					break FLUSH
				default:
					break FLUSH
				}
			}

			if err := s.buffered.Flush(); err != nil {
				s.logger.Printf("[ERR] serf: failed to flush snapshot: %v", err)
			}
			if err := s.fh.Sync(); err != nil {
				s.logger.Printf("[ERR] serf: failed to sync snapshot: %v", err)
			}
			s.fh.Close()
			close(s.waitCh)
			return
		}
	}
}

// processMemberEvent is used to handle a single member event
func (s *Snapshotter) processMemberEvent(e MemberEvent) {
	switch e.Type {
	case EventMemberJoin:
		for _, mem := range e.Members {
			addr := net.TCPAddr{IP: mem.Addr, Port: int(mem.Port)}
			s.aliveNodes[mem.Name] = addr.String()
			s.tryAppend(fmt.Sprintf("alive: %s %s\n", mem.Name, addr.String()))
		}

	case EventMemberLeave:
		fallthrough
	case EventMemberFailed:
		for _, mem := range e.Members {
			delete(s.aliveNodes, mem.Name)
			s.tryAppend(fmt.Sprintf("not-alive: %s\n", mem.Name))
		}
	}
	s.updateClock()
}

// updateClock is called periodically to check if we should udpate our
// clock value. This is done after member events but should also be done
// periodically due to race conditions with join and leave intents
func (s *Snapshotter) updateClock() {
	lastSeen := s.clock.Time() - 1
	if lastSeen > s.lastClock {
		s.lastClock = lastSeen
		s.tryAppend(fmt.Sprintf("clock: %d\n", s.lastClock))
	}
}

// processUserEvent is used to handle a single user event
func (s *Snapshotter) processUserEvent(e UserEvent) {
	// Ignore old clocks
	if e.LTime <= s.lastEventClock {
		return
	}
	s.lastEventClock = e.LTime
	s.tryAppend(fmt.Sprintf("event-clock: %d\n", e.LTime))
}

// processQuery is used to handle a single query event
func (s *Snapshotter) processQuery(q *Query) {
	// Ignore old clocks
	if q.LTime <= s.lastQueryClock {
		return
	}
	s.lastQueryClock = q.LTime
	s.tryAppend(fmt.Sprintf("query-clock: %d\n", q.LTime))
}

// tryAppend will invoke append line but will not return an error
func (s *Snapshotter) tryAppend(l string) {
	if err := s.appendLine(l); err != nil {
		s.logger.Printf("[ERR] serf: Failed to update snapshot: %v", err)
		now := time.Now()
		if now.Sub(s.lastAttemptedCompaction) > snapshotErrorRecoveryInterval {
			s.lastAttemptedCompaction = now
			s.logger.Printf("[INFO] serf: Attempting compaction to recover from error...")
			err = s.compact()
			if err != nil {
				s.logger.Printf("[ERR] serf: Compaction failed, will reattempt after %v: %v", snapshotErrorRecoveryInterval, err)
			} else {
				s.logger.Printf("[INFO] serf: Finished compaction, successfully recovered from error state")
			}
		}
	}
}

// appendLine is used to append a line to the existing log
func (s *Snapshotter) appendLine(l string) error {
	defer metrics.MeasureSince([]string{"serf", "snapshot", "appendLine"}, time.Now())

	n, err := s.buffered.WriteString(l)
	if err != nil {
		return err
	}

	// Check if we should flush
	now := time.Now()
	if now.Sub(s.lastFlush) > flushInterval {
		s.lastFlush = now
		if err := s.buffered.Flush(); err != nil {
			return err
		}
	}

	// Check if a compaction is necessary
	s.offset += int64(n)
	if s.offset > s.snapshotMaxSize() {
		return s.compact()
	}
	return nil
}

// snapshotMaxSize computes the maximum size and is used to force periodic compaction.
func (s *Snapshotter) snapshotMaxSize() int64 {
	nodes := int64(len(s.aliveNodes))
	estSize := nodes * snapshotBytesPerNode
	threshold := estSize * snapshotCompactionThreshold

	// Apply a minimum threshold to avoid frequent compaction
	if threshold < s.minCompactSize {
		threshold = s.minCompactSize
	}
	return threshold
}

// Compact is used to compact the snapshot once it is too large
func (s *Snapshotter) compact() error {
	defer metrics.MeasureSince([]string{"serf", "snapshot", "compact"}, time.Now())

	// Try to open the file to new fiel
	newPath := s.path + tmpExt
	fh, err := os.OpenFile(newPath, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0755)
	if err != nil {
		return fmt.Errorf("failed to open new snapshot: %v", err)
	}

	// Create a buffered writer
	buf := bufio.NewWriter(fh)

	// Write out the live nodes
	var offset int64
	for name, addr := range s.aliveNodes {
		line := fmt.Sprintf("alive: %s %s\n", name, addr)
		n, err := buf.WriteString(line)
		if err != nil {
			fh.Close()
			return err
		}
		offset += int64(n)
	}

	// Write out the clocks
	line := fmt.Sprintf("clock: %d\n", s.lastClock)
	n, err := buf.WriteString(line)
	if err != nil {
		fh.Close()
		return err
	}
	offset += int64(n)

	line = fmt.Sprintf("event-clock: %d\n", s.lastEventClock)
	n, err = buf.WriteString(line)
	if err != nil {
		fh.Close()
		return err
	}
	offset += int64(n)

	line = fmt.Sprintf("query-clock: %d\n", s.lastQueryClock)
	n, err = buf.WriteString(line)
	if err != nil {
		fh.Close()
		return err
	}
	offset += int64(n)

	// Flush the new snapshot
	err = buf.Flush()

	if err != nil {
		return fmt.Errorf("failed to flush new snapshot: %v", err)
	}

	err = fh.Sync()

	if err != nil {
		fh.Close()
		return fmt.Errorf("failed to fsync new snapshot: %v", err)
	}

	fh.Close()

	// We now need to swap the old snapshot file with the new snapshot.
	// Turns out, Windows won't let us rename the files if we have
	// open handles to them or if the destination already exists. This
	// means we are forced to close the existing handles, delete the
	// old file, move the new one in place, and then re-open the file
	// handles.

	// Flush the existing snapshot, ignoring errors since we will
	// delete it momentarily.
	s.buffered.Flush()
	s.buffered = nil

	// Close the file handle to the old snapshot
	s.fh.Close()
	s.fh = nil

	// Delete the old file
	if err := os.Remove(s.path); err != nil {
		return fmt.Errorf("failed to remove old snapshot: %v", err)
	}

	// Move the new file into place
	if err := os.Rename(newPath, s.path); err != nil {
		return fmt.Errorf("failed to install new snapshot: %v", err)
	}

	// Open the new snapshot
	fh, err = os.OpenFile(s.path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0755)
	if err != nil {
		return fmt.Errorf("failed to open snapshot: %v", err)
	}
	buf = bufio.NewWriter(fh)

	// Rotate our handles
	s.fh = fh
	s.buffered = buf
	s.offset = offset
	s.lastFlush = time.Now()
	return nil
}

// replay is used to seek to reset our internal state by replaying
// the snapshot file. It is used at initialization time to read old
// state
func (s *Snapshotter) replay() error {
	// Seek to the beginning
	if _, err := s.fh.Seek(0, os.SEEK_SET); err != nil {
		return err
	}

	// Read each line
	reader := bufio.NewReader(s.fh)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		// Skip the newline
		line = line[:len(line)-1]

		// Switch on the prefix
		if strings.HasPrefix(line, "alive: ") {
			info := strings.TrimPrefix(line, "alive: ")
			addrIdx := strings.LastIndex(info, " ")
			if addrIdx == -1 {
				s.logger.Printf("[WARN] serf: Failed to parse address: %v", line)
				continue
			}
			addr := info[addrIdx+1:]
			name := info[:addrIdx]
			s.aliveNodes[name] = addr

		} else if strings.HasPrefix(line, "not-alive: ") {
			name := strings.TrimPrefix(line, "not-alive: ")
			delete(s.aliveNodes, name)

		} else if strings.HasPrefix(line, "clock: ") {
			timeStr := strings.TrimPrefix(line, "clock: ")
			timeInt, err := strconv.ParseUint(timeStr, 10, 64)
			if err != nil {
				s.logger.Printf("[WARN] serf: Failed to convert clock time: %v", err)
				continue
			}
			s.lastClock = LamportTime(timeInt)

		} else if strings.HasPrefix(line, "event-clock: ") {
			timeStr := strings.TrimPrefix(line, "event-clock: ")
			timeInt, err := strconv.ParseUint(timeStr, 10, 64)
			if err != nil {
				s.logger.Printf("[WARN] serf: Failed to convert event clock time: %v", err)
				continue
			}
			s.lastEventClock = LamportTime(timeInt)

		} else if strings.HasPrefix(line, "query-clock: ") {
			timeStr := strings.TrimPrefix(line, "query-clock: ")
			timeInt, err := strconv.ParseUint(timeStr, 10, 64)
			if err != nil {
				s.logger.Printf("[WARN] serf: Failed to convert query clock time: %v", err)
				continue
			}
			s.lastQueryClock = LamportTime(timeInt)

		} else if strings.HasPrefix(line, "coordinate: ") {
			continue // Ignores any coordinate persistence from old snapshots, serf should re-converge
		} else if line == "leave" {
			// Ignore a leave if we plan on re-joining
			if s.rejoinAfterLeave {
				s.logger.Printf("[INFO] serf: Ignoring previous leave in snapshot")
				continue
			}
			s.aliveNodes = make(map[string]string)
			s.lastClock = 0
			s.lastEventClock = 0
			s.lastQueryClock = 0

		} else if strings.HasPrefix(line, "#") {
			// Skip comment lines

		} else {
			s.logger.Printf("[WARN] serf: Unrecognized snapshot line: %v", line)
		}
	}

	// Seek to the end
	if _, err := s.fh.Seek(0, os.SEEK_END); err != nil {
		return err
	}
	return nil
}
