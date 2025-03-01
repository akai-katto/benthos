package input

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/benthosdev/benthos/v4/internal/batch"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/message"
)

type asyncPreserverResend struct {
	boff     backoff.BackOff
	attempts int
	msg      message.Batch
	ackFn    AsyncAckFn
}

func newResendMsg(msg message.Batch, ackFn AsyncAckFn) asyncPreserverResend {
	boff := backoff.NewExponentialBackOff()
	boff.InitialInterval = time.Millisecond
	boff.MaxInterval = time.Second
	boff.Multiplier = 1.1
	boff.MaxElapsedTime = 0
	return asyncPreserverResend{boff: boff, attempts: 0, msg: msg, ackFn: ackFn}
}

// AsyncPreserver is a wrapper for input.Async implementations that keeps a
// buffer of sent messages until they are acknowledged. If an error occurs
// during message propagation the contents of the buffer will be resent instead
// of reading new messages until it is depleted. AsyncPreserver implements
// input.Async.
//
// Wrapping an input with this type is useful when your source of messages
// doesn't have a concept of a NoAck (like Kafka), and instead of "rejecting"
// messages we always intend to simply retry them until success.
type AsyncPreserver struct {
	resendMessages  []asyncPreserverResend
	resendInterrupt func()
	msgsMut         sync.Mutex
	pendingMessages int64

	inputClosed int32
	r           Async
}

// NewAsyncPreserver returns a new AsyncPreserver wrapper around a input.Async.
func NewAsyncPreserver(r Async) *AsyncPreserver {
	return &AsyncPreserver{
		r:               r,
		resendInterrupt: func() {},
	}
}

//------------------------------------------------------------------------------

// Connect attempts to establish a connection to the source, if
// unsuccessful returns an error. If the attempt is successful (or not
// necessary) returns nil.
func (p *AsyncPreserver) Connect(ctx context.Context) error {
	err := p.r.Connect(ctx)
	// If our source has finished but we still have messages in flight then
	// we act like we're still open. Read will be called and we can either
	// return the pending messages or wait for them.
	if errors.Is(err, component.ErrTypeClosed) && atomic.LoadInt64(&p.pendingMessages) > 0 {
		atomic.StoreInt32(&p.inputClosed, 1)
		err = nil
	}
	return err
}

func (p *AsyncPreserver) wrapAckFn(m asyncPreserverResend) (message.Batch, AsyncAckFn) {
	if m.msg.Len() == 1 {
		return p.wrapSingleAckFn(m)
	}
	return p.wrapBatchAckFn(m)
}

func (p *AsyncPreserver) wrapBatchAckFn(m asyncPreserverResend) (message.Batch, AsyncAckFn) {
	sortGroup, trackedMsg := message.NewSortGroup(m.msg)

	return trackedMsg, func(ctx context.Context, res error) error {
		if res != nil {
			resendMsg := m.msg
			if walkable, ok := res.(batch.WalkableError); ok && walkable.IndexedErrors() < m.msg.Len() {
				resendMsg = message.QuickBatch(nil)
				walkable.WalkParts(func(i int, p *message.Part, e error) bool {
					if e == nil {
						return true
					}
					if tagIndex := sortGroup.GetIndex(p); tagIndex >= 0 {
						resendMsg = append(resendMsg, m.msg.Get(tagIndex))
						return true
					}

					// If we couldn't link the errored part back to an original
					// message then we need to retry all of them.
					resendMsg = m.msg
					return false
				})
				if resendMsg.Len() == 0 {
					resendMsg = m.msg
				}
			}
			m.msg = resendMsg

			p.msgsMut.Lock()
			p.resendMessages = append(p.resendMessages, m)
			p.resendInterrupt()
			p.msgsMut.Unlock()
			return nil
		}
		atomic.AddInt64(&p.pendingMessages, -1)
		return m.ackFn(ctx, res)
	}
}

func (p *AsyncPreserver) wrapSingleAckFn(m asyncPreserverResend) (message.Batch, AsyncAckFn) {
	return m.msg, func(ctx context.Context, res error) error {
		if res != nil {
			p.msgsMut.Lock()
			p.resendMessages = append(p.resendMessages, m)
			p.resendInterrupt()
			p.msgsMut.Unlock()
			return nil
		}
		atomic.AddInt64(&p.pendingMessages, -1)
		return m.ackFn(ctx, res)
	}
}

// ReadBatch attempts to read a new message from the source.
func (p *AsyncPreserver) ReadBatch(ctx context.Context) (message.Batch, AsyncAckFn, error) {
	var cancel func()
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	// If we have messages queued to be resent we prioritise them over reading
	// new messages.
	p.msgsMut.Lock()
	if lMsgs := len(p.resendMessages); lMsgs > 0 {
		resend := p.resendMessages[0]
		if lMsgs > 1 {
			p.resendMessages = p.resendMessages[1:]
		} else {
			p.resendMessages = nil
		}
		p.msgsMut.Unlock()

		resend.attempts++
		if resend.attempts > 2 {
			// This sleep prevents a busy loop on permanently failed messages.
			if tout := resend.boff.NextBackOff(); tout > 0 {
				select {
				case <-time.After(tout):
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
		}
		sendMsg, ackFn := p.wrapAckFn(resend)
		return sendMsg.ShallowCopy(), ackFn, nil
	}
	p.resendInterrupt = cancel
	p.msgsMut.Unlock()

	var (
		msg message.Batch
		aFn AsyncAckFn
		err error
	)
	if atomic.LoadInt32(&p.inputClosed) > 0 {
		err = component.ErrTypeClosed
	} else {
		msg, aFn, err = p.r.ReadBatch(ctx)
	}
	if err != nil {
		// If our source has finished but we still have messages in flight then
		// we block, ideally until the messages are acked.
		if errors.Is(err, component.ErrTypeClosed) && atomic.LoadInt64(&p.pendingMessages) > 0 {
			// The context is cancelled either when new pending messages are
			// ready, or when the upstream component cancels. If the former
			// occurs then we still return the cancelled error and let Read get
			// called to gobble up the new pending messages.
			for {
				select {
				case <-ctx.Done():
					return nil, nil, component.ErrTimeout
				case <-time.After(time.Millisecond * 10):
					if atomic.LoadInt64(&p.pendingMessages) <= 0 {
						return nil, nil, component.ErrTypeClosed
					}
				}
			}
		}
		return nil, nil, err
	}
	atomic.AddInt64(&p.pendingMessages, 1)
	sendMsg, ackFn := p.wrapAckFn(newResendMsg(msg, aFn))
	return sendMsg.ShallowCopy(), ackFn, nil
}

// Close triggers the shut down of this component and blocks until completion or
// context cancellation.
func (p *AsyncPreserver) Close(ctx context.Context) error {
	return p.r.Close(ctx)
}
