package serf

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// EventType are all the types of events that may occur and be sent
// along the Serf channel.
type EventType int

const (
	EventMemberJoin EventType = iota
	EventMemberLeave
	EventMemberFailed
	EventMemberUpdate
	EventMemberReap
	EventUser
	EventQuery
)

func (t EventType) String() string {
	switch t {
	case EventMemberJoin:
		return "member-join"
	case EventMemberLeave:
		return "member-leave"
	case EventMemberFailed:
		return "member-failed"
	case EventMemberUpdate:
		return "member-update"
	case EventMemberReap:
		return "member-reap"
	case EventUser:
		return "user"
	case EventQuery:
		return "query"
	default:
		panic(fmt.Sprintf("unknown event type: %d", t))
	}
}

// Event is a generic interface for exposing Serf events
// Clients will usually need to use a type switches to get
// to a more useful type
type Event interface {
	EventType() EventType
	String() string
}

// MemberEvent is the struct used for member related events
// Because Serf coalesces events, an event may contain multiple members.
type MemberEvent struct {
	Type    EventType
	Members []Member
}

func (m MemberEvent) EventType() EventType {
	return m.Type
}

func (m MemberEvent) String() string {
	switch m.Type {
	case EventMemberJoin:
		return "member-join"
	case EventMemberLeave:
		return "member-leave"
	case EventMemberFailed:
		return "member-failed"
	case EventMemberUpdate:
		return "member-update"
	case EventMemberReap:
		return "member-reap"
	default:
		panic(fmt.Sprintf("unknown event type: %d", m.Type))
	}
}

// UserEvent is the struct used for events that are triggered
// by the user and are not related to members
type UserEvent struct {
	LTime    LamportTime
	Name     string
	Payload  []byte
	Coalesce bool
}

func (u UserEvent) EventType() EventType {
	return EventUser
}

func (u UserEvent) String() string {
	return fmt.Sprintf("user-event: %s", u.Name)
}

// Query is the struct used by EventQuery type events
type Query struct {
	LTime   LamportTime
	Name    string
	Payload []byte

	serf        *Serf
	id          uint32    // ID is not exported, since it may change
	addr        []byte    // Address to respond to
	port        uint16    // Port to respond to
	deadline    time.Time // Must respond by this deadline
	relayFactor uint8     // Number of duplicate responses to relay back to sender
	respLock    sync.Mutex
}

func (q *Query) EventType() EventType {
	return EventQuery
}

func (q *Query) String() string {
	return fmt.Sprintf("query: %s", q.Name)
}

// Deadline returns the time by which a response must be sent
func (q *Query) Deadline() time.Time {
	return q.deadline
}

func (q *Query) createResponse(buf []byte) messageQueryResponse {
	// Create response
	return messageQueryResponse{
		LTime:   q.LTime,
		ID:      q.id,
		From:    q.serf.config.NodeName,
		Payload: buf,
	}
}

// Check response size
func (q *Query) checkResponseSize(resp []byte) error {
	if len(resp) > q.serf.config.QueryResponseSizeLimit {
		return fmt.Errorf("response exceeds limit of %d bytes", q.serf.config.QueryResponseSizeLimit)
	}
	return nil
}

func (q *Query) respondWithMessageAndResponse(raw []byte, resp messageQueryResponse) error {
	// Check the size limit
	if err := q.checkResponseSize(raw); err != nil {
		return err
	}

	q.respLock.Lock()
	defer q.respLock.Unlock()

	// Check if we've already responded
	if q.deadline.IsZero() {
		return fmt.Errorf("response already sent")
	}

	// Ensure we aren't past our response deadline
	if time.Now().After(q.deadline) {
		return fmt.Errorf("response is past the deadline")
	}

	// Send the response directly to the originator
	addr := net.UDPAddr{IP: q.addr, Port: int(q.port)}
	if err := q.serf.memberlist.SendTo(&addr, raw); err != nil {
		return err
	}

	// Relay the response through up to relayFactor other nodes
	if err := q.serf.relayResponse(q.relayFactor, addr, &resp); err != nil {
		return err
	}

	// Clear the deadline, responses sent
	q.deadline = time.Time{}

	return nil
}

// Respond is used to send a response to the user query
func (q *Query) Respond(buf []byte) error {
	// Create response
	resp := q.createResponse(buf)

	// Encode response
	raw, err := encodeMessage(messageQueryResponseType, resp)
	if err != nil {
		return fmt.Errorf("failed to format response: %v", err)
	}

	if err := q.respondWithMessageAndResponse(raw, resp); err != nil {
		return fmt.Errorf("failed to respond to key query: %v", err)
	}

	return nil
}
