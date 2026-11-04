package pipe

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
)

type channel[V any] struct {
	OnSendCompletion func()
	value            atomic.Pointer[V]
	lastValue        *V
}

func (c *channel[V]) Send(v V) {
	c.value.Store(&v)
	if c.OnSendCompletion != nil {
		c.OnSendCompletion()
	}
}

func (c *channel[V]) Receive() (V, bool) {
	v := c.value.Load()
	if v == nil || v == c.lastValue {
		return *new(V), false
	}
	c.lastValue = v
	return *v, true
}

type Pipe[Payload, Value any] struct {
	Sender              Sender[Payload, Value]
	Receiver            Receiver[Payload, Value]
	OnReceiveCompletion func()
	OnSendCompletion    func()
}

type Request[Payload any] struct {
	Payload  Payload
	Canceled bool
}

type Sender[Payload, Value any] interface {
	Request() Request[Payload]
	Update(v Value)
	Finalize(v Value, err error)
	Status() Status[Value]
}

type Receiver[Payload, Value any] interface {
	Receive() bool
	Cancel()
	Status() Status[Value]
	Request() Payload
}

type Status[Value any] struct {
	Canceled  bool
	Completed bool
	Err       error
	Value     Value
}

func NewWithFunction[Payload, Value any](f func(context.Context) (Value, error)) (*Pipe[Payload, Value], func()) {
	p := New[Payload, Value](Request[Payload]{})

	ctx, cancel := context.WithCancelCause(context.TODO())

	p.OnReceiveCompletion = func() {
		if req := p.Sender.Request(); req.Canceled {
			cancel(errors.WithStack(context.Canceled))
		}
	}

	return p, func() {
		res, err := f(ctx)
		if err != nil {
			p.Sender.Finalize(*new(Value), err)
			return
		}
		p.Sender.Finalize(res, nil)
	}
}

func New[Payload, Value any](req Request[Payload]) *Pipe[Payload, Value] {
	cancelCh := &channel[Request[Payload]]{}
	roundTripCh := &channel[Status[Value]]{}
	pw := &sender[Payload, Value]{
		req:         req,
		sendChannel: roundTripCh,
	}
	pr := &receiver[Payload, Value]{
		req:         req,
		recvChannel: roundTripCh,
		sendChannel: cancelCh,
	}

	p := &Pipe[Payload, Value]{
		Sender:   pw,
		Receiver: pr,
	}

	cancelCh.OnSendCompletion = func() {
		v, ok := cancelCh.Receive()
		if ok {
			pw.setRequest(v)
		}
		if p.OnReceiveCompletion != nil {
			p.OnReceiveCompletion()
		}
	}

	roundTripCh.OnSendCompletion = func() {
		if p.OnSendCompletion != nil {
			p.OnSendCompletion()
		}
	}

	return p
}

type sender[Payload, Value any] struct {
	status      Status[Value]
	req         Request[Payload]
	sendChannel *channel[Status[Value]]
	mu          sync.Mutex
}

func (pw *sender[Payload, Value]) Status() Status[Value] {
	return pw.status
}

func (pw *sender[Payload, Value]) Request() Request[Payload] {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.req
}

func (pw *sender[Payload, Value]) setRequest(req Request[Payload]) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.req = req
}

func (pw *sender[Payload, Value]) Update(v Value) {
	pw.status.Value = v
	pw.sendChannel.Send(pw.status)
}

func (pw *sender[Payload, Value]) Finalize(v Value, err error) {
	pw.status.Value = v
	pw.status.Err = err
	pw.status.Completed = true
	if errors.Is(err, context.Canceled) && pw.Request().Canceled {
		pw.status.Canceled = true
	}
	pw.sendChannel.Send(pw.status)
}

type receiver[Payload, Value any] struct {
	status      Status[Value]
	req         Request[Payload]
	recvChannel *channel[Status[Value]]
	sendChannel *channel[Request[Payload]]
}

func (pr *receiver[Payload, Value]) Request() Payload {
	return pr.req.Payload
}

func (pr *receiver[Payload, Value]) Receive() bool {
	v, ok := pr.recvChannel.Receive()
	if !ok {
		return false
	}
	pr.status = v
	return true
}

func (pr *receiver[Payload, Value]) Cancel() {
	req := pr.req
	if req.Canceled {
		return
	}
	req.Canceled = true
	pr.sendChannel.Send(req)
}

func (pr *receiver[Payload, Value]) Status() Status[Value] {
	return pr.status
}
