package serf

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"regexp"
	"sync"
	"time"
)

// QueryParam is provided to Query() to configure the parameters of the
// query. If not provided, sane defaults will be used.
type QueryParam struct {
	// If provided, we restrict the nodes that should respond to those
	// with names in this list
	FilterNodes []string

	// FilterTags maps a tag name to a regular expression that is applied
	// to restrict the nodes that should respond
	FilterTags map[string]string

	// If true, we are requesting an delivery acknowledgement from
	// every node that meets the filter requirement. This means nodes
	// the receive the message but do not pass the filters, will not
	// send an ack.
	RequestAck bool

	// RelayFactor controls the number of duplicate responses to relay
	// back to the sender through other nodes for redundancy.
	RelayFactor uint8

	// The timeout limits how long the query is left open. If not provided,
	// then a default timeout is used based on the configuration of Serf
	Timeout time.Duration
}

// DefaultQueryTimeout returns the default timeout value for a query
// Computed as GossipInterval * QueryTimeoutMult * log(N+1)
func (s *Serf) DefaultQueryTimeout() time.Duration {
	n := s.memberlist.NumMembers()
	timeout := s.config.MemberlistConfig.GossipInterval
	timeout *= time.Duration(s.config.QueryTimeoutMult)
	timeout *= time.Duration(math.Ceil(math.Log10(float64(n + 1))))
	return timeout
}

// DefaultQueryParam is used to return the default query parameters
func (s *Serf) DefaultQueryParams() *QueryParam {
	return &QueryParam{
		FilterNodes: nil,
		FilterTags:  nil,
		RequestAck:  false,
		Timeout:     s.DefaultQueryTimeout(),
	}
}

// encodeFilters is used to convert the filters into the wire format
func (q *QueryParam) encodeFilters() ([][]byte, error) {
	var filters [][]byte

	// Add the node filter
	if len(q.FilterNodes) > 0 {
		if buf, err := encodeFilter(filterNodeType, q.FilterNodes); err != nil {
			return nil, err
		} else {
			filters = append(filters, buf)
		}
	}

	// Add the tag filters
	for tag, expr := range q.FilterTags {
		filt := filterTag{tag, expr}
		if buf, err := encodeFilter(filterTagType, &filt); err != nil {
			return nil, err
		} else {
			filters = append(filters, buf)
		}
	}

	return filters, nil
}

// QueryResponse is returned for each new Query. It is used to collect
// Ack's as well as responses and to provide those back to a client.
type QueryResponse struct {
	// ackCh is used to send the name of a node for which we've received an ack
	ackCh chan string

	// deadline is the query end time (start + query timeout)
	deadline time.Time

	// Query ID
	id uint32

	// Stores the LTime of the query
	lTime LamportTime

	// respCh is used to send a response from a node
	respCh chan NodeResponse

	// acks/responses are used to track the nodes that have sent an ack/response
	acks      map[string]struct{}
	responses map[string]struct{}

	closed    bool
	closeLock sync.Mutex
}

// newQueryResponse is used to construct a new query response
func newQueryResponse(n int, q *messageQuery) *QueryResponse {
	resp := &QueryResponse{
		deadline:  time.Now().Add(q.Timeout),
		id:        q.ID,
		lTime:     q.LTime,
		respCh:    make(chan NodeResponse, n),
		responses: make(map[string]struct{}),
	}
	if q.Ack() {
		resp.ackCh = make(chan string, n)
		resp.acks = make(map[string]struct{})
	}
	return resp
}

// Close is used to close the query, which will close the underlying
// channels and prevent further deliveries
func (r *QueryResponse) Close() {
	r.closeLock.Lock()
	defer r.closeLock.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.ackCh != nil {
		close(r.ackCh)
	}
	if r.respCh != nil {
		close(r.respCh)
	}
}

// Deadline returns the ending deadline of the query
func (r *QueryResponse) Deadline() time.Time {
	return r.deadline
}

// Finished returns if the query is finished running
func (r *QueryResponse) Finished() bool {
	r.closeLock.Lock()
	defer r.closeLock.Unlock()
	return r.closed || time.Now().After(r.deadline)
}

// AckCh returns a channel that can be used to listen for acks
// Channel will be closed when the query is finished. This is nil,
// if the query did not specify RequestAck.
func (r *QueryResponse) AckCh() <-chan string {
	return r.ackCh
}

// ResponseCh returns a channel that can be used to listen for responses.
// Channel will be closed when the query is finished.
func (r *QueryResponse) ResponseCh() <-chan NodeResponse {
	return r.respCh
}

// sendResponse sends a response on the response channel ensuring the channel is not closed.
func (r *QueryResponse) sendResponse(nr NodeResponse) error {
	r.closeLock.Lock()
	defer r.closeLock.Unlock()
	if r.closed {
		return nil
	}
	select {
	case r.respCh <- nr:
		r.responses[nr.From] = struct{}{}
	default:
		return errors.New("serf: Failed to deliver query response, dropping")
	}
	return nil
}

// NodeResponse is used to represent a single response from a node
type NodeResponse struct {
	From    string
	Payload []byte
}

// shouldProcessQuery checks if a query should be proceeded given
// a set of filers.
func (s *Serf) shouldProcessQuery(filters [][]byte) bool {
	for _, filter := range filters {
		switch filterType(filter[0]) {
		case filterNodeType:
			// Decode the filter
			var nodes filterNode
			if err := decodeMessage(filter[1:], &nodes); err != nil {
				s.logger.Printf("[WARN] serf: failed to decode filterNodeType: %v", err)
				return false
			}

			// Check if we are being targeted
			found := false
			for _, n := range nodes {
				if n == s.config.NodeName {
					found = true
					break
				}
			}
			if !found {
				return false
			}

		case filterTagType:
			// Decode the filter
			var filt filterTag
			if err := decodeMessage(filter[1:], &filt); err != nil {
				s.logger.Printf("[WARN] serf: failed to decode filterTagType: %v", err)
				return false
			}

			// Check if we match this regex
			tags := s.config.Tags
			matched, err := regexp.MatchString(filt.Expr, tags[filt.Tag])
			if err != nil {
				s.logger.Printf("[WARN] serf: failed to compile filter regex (%s): %v", filt.Expr, err)
				return false
			}
			if !matched {
				return false
			}

		default:
			s.logger.Printf("[WARN] serf: query has unrecognized filter type: %d", filter[0])
			return false
		}
	}
	return true
}

// relayResponse will relay a copy of the given response to up to relayFactor
// other members.
func (s *Serf) relayResponse(relayFactor uint8, addr net.UDPAddr, resp *messageQueryResponse) error {
	if relayFactor == 0 {
		return nil
	}

	// Needs to be worth it; we need to have at least relayFactor *other*
	// nodes. If you have a tiny cluster then the relayFactor shouldn't
	// be needed.
	members := s.Members()
	if len(members) < int(relayFactor)+1 {
		return nil
	}

	// Prep the relay message, which is a wrapped version of the original.
	raw, err := encodeRelayMessage(messageQueryResponseType, addr, &resp)
	if err != nil {
		return fmt.Errorf("failed to format relayed response: %v", err)
	}
	if len(raw) > s.config.QueryResponseSizeLimit {
		return fmt.Errorf("relayed response exceeds limit of %d bytes", s.config.QueryResponseSizeLimit)
	}

	// Relay to a random set of peers.
	localName := s.LocalMember().Name
	relayMembers := kRandomMembers(int(relayFactor), members, func(m Member) bool {
		return m.Status != StatusAlive || m.ProtocolMax < 5 || m.Name == localName
	})
	for _, m := range relayMembers {
		relayAddr := net.UDPAddr{IP: m.Addr, Port: int(m.Port)}
		if err := s.memberlist.SendTo(&relayAddr, raw); err != nil {
			return fmt.Errorf("failed to send relay response: %v", err)
		}
	}
	return nil
}

// kRandomMembers selects up to k members from a given list, optionally
// filtering by the given filterFunc
func kRandomMembers(k int, members []Member, filterFunc func(Member) bool) []Member {
	n := len(members)
	kMembers := make([]Member, 0, k)
OUTER:
	// Probe up to 3*n times, with large n this is not necessary
	// since k << n, but with small n we want search to be
	// exhaustive
	for i := 0; i < 3*n && len(kMembers) < k; i++ {
		// Get random member
		idx := rand.Intn(n)
		member := members[idx]

		// Give the filter a shot at it.
		if filterFunc != nil && filterFunc(member) {
			continue OUTER
		}

		// Check if we have this member already
		for j := 0; j < len(kMembers); j++ {
			if member.Name == kMembers[j].Name {
				continue OUTER
			}
		}

		// Append the member
		kMembers = append(kMembers, member)
	}

	return kMembers
}
