package stream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

const (
	// subStateOpen is the default state of a subscription. An open subscription
	// may return new events.
	subStateOpen = 0

	// subStateForceClosed indicates the subscription was forced closed by
	// the EventPublisher, possibly as a result of a change to an ACL token, and
	// will not return new events.
	// The subscriber must issue a new Subscribe request.
	subStateForceClosed = 1

	// subStateUnsub indicates the subscription was closed by the caller, and
	// will not return new events.
	subStateUnsub = 2
)

// ErrSubForceClosed is a error signalling the subscription has been
// closed. The client should Unsubscribe, then re-Subscribe.
var ErrSubForceClosed = errors.New("subscription closed by server, client must reset state and resubscribe")

// Subscription provides events on a Topic. Events may be filtered by Key.
// Events are returned by Next(), and may start with a Snapshot of events.
type Subscription struct {
	state uint32

	// req is the requests that we are responding to
	req SubscribeRequest

	// currentItem stores the current snapshot or topic buffer item we are on. It
	// is mutated by calls to Next.
	currentItem *bufferItem

	// closed is a channel which is closed when the subscription is closed. It
	// is used to exit the blocking select.
	closed chan struct{}

	// unsub is a function set by EventPublisher that is called to free resources
	// when the subscription is no longer needed.
	// It must be safe to call the function from multiple goroutines and the function
	// must be idempotent.
	unsub func()
}

// SubscribeRequest identifies the types of events the subscriber would like to
// receiver. Topic and Token are required.
type SubscribeRequest struct {
	Topic Topic
	Key   string
	Token string
	Index uint64
}

// newSubscription return a new subscription. The caller is responsible for
// calling Unsubscribe when it is done with the subscription, to free resources.
func newSubscription(req SubscribeRequest, item *bufferItem, unsub func()) *Subscription {
	return &Subscription{
		closed:      make(chan struct{}),
		req:         req,
		currentItem: item,
		unsub:       unsub,
	}
}

// Next returns the next Event to deliver. It must only be called from a
// single goroutine concurrently as it mutates the Subscription.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		if err := s.requireStateOpen(); err != nil {
			return Event{}, err
		}

		next, err := s.currentItem.Next(ctx, s.closed)
		if err := s.requireStateOpen(); err != nil {
			return Event{}, err
		}
		if err != nil {
			return Event{}, err
		}
		s.currentItem = next
		if len(next.Events) == 0 {
			continue
		}
		event, ok := filterByKey(s.req, next.Events)
		if !ok {
			continue
		}
		return event, nil
	}
}

func (s *Subscription) requireStateOpen() error {
	switch atomic.LoadUint32(&s.state) {
	case subStateForceClosed:
		return ErrSubForceClosed
	case subStateUnsub:
		return fmt.Errorf("subscription was closed by unsubscribe")
	default:
		return nil
	}
}

func newEventFromBatch(req SubscribeRequest, events []Event) Event {
	first := events[0]
	if len(events) == 1 {
		return first
	}
	return Event{
		Topic:   req.Topic,
		Key:     req.Key,
		Index:   first.Index,
		Payload: events,
	}
}

func filterByKey(req SubscribeRequest, events []Event) (Event, bool) {
	event := newEventFromBatch(req, events)
	if req.Key == "" {
		return event, true
	}

	fn := func(e Event) bool {
		return req.Key == e.Key
	}
	return event.Filter(fn)
}

// Close the subscription. Subscribers will receive an error when they call Next,
// and will need to perform a new Subscribe request.
// It is safe to call from any goroutine.
func (s *Subscription) forceClose() {
	if atomic.CompareAndSwapUint32(&s.state, subStateOpen, subStateForceClosed) {
		close(s.closed)
	}
}

// Unsubscribe the subscription, freeing resources.
func (s *Subscription) Unsubscribe() {
	if atomic.CompareAndSwapUint32(&s.state, subStateOpen, subStateUnsub) {
		close(s.closed)
	}
	s.unsub()
}
