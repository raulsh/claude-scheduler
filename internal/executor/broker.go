package executor

import (
	"encoding/json"
	"sync"
)

// ringSize bounds the per-execution replay buffer. A browser attaching
// mid-run gets this much recent context immediately; anything older is
// fetched from the database instead.
const ringSize = 500

// subscriberBuffer is the per-subscriber queue depth. A subscriber that
// cannot keep up is dropped rather than being allowed to stall the executor,
// because the transcript is durable in the database either way.
const subscriberBuffer = 256

// Message is one transcript event as delivered to subscribers.
type Message struct {
	Seq      int64           `json:"seq"`
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
	Status   string          `json:"status,omitempty"`
}

// Broker fans out live execution events to HTTP subscribers.
type Broker struct {
	mu     sync.Mutex
	topics map[int64]*topic
}

// NewBroker creates an empty broker.
func NewBroker() *Broker {
	return &Broker{topics: make(map[int64]*topic)}
}

type topic struct {
	mu      sync.Mutex
	ring    []Message
	subs    map[int64]chan Message
	nextID  int64
	closed  bool
	dropped int64
}

func (b *Broker) topicFor(executionID int64) *topic {
	b.mu.Lock()
	defer b.mu.Unlock()

	t, ok := b.topics[executionID]
	if !ok {
		t = &topic{subs: make(map[int64]chan Message)}
		b.topics[executionID] = t
	}
	return t
}

// Publish delivers a message to every current subscriber and records it in
// the replay ring.
func (b *Broker) Publish(executionID int64, msg Message) {
	t := b.topicFor(executionID)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}

	t.ring = append(t.ring, msg)
	if len(t.ring) > ringSize {
		t.ring = t.ring[len(t.ring)-ringSize:]
	}

	for id, ch := range t.subs {
		select {
		case ch <- msg:
		default:
			// The subscriber is not draining. Drop it: the database holds the
			// authoritative transcript, and the client will refetch on
			// reconnect.
			t.dropped++
			close(ch)
			delete(t.subs, id)
		}
	}
}

// Subscription is a live feed of one execution's events.
type Subscription struct {
	// Backlog holds buffered messages after the requested sequence number,
	// delivered before anything arriving on C.
	Backlog []Message
	C       <-chan Message

	cancel func()
}

// Close releases the subscription.
func (s *Subscription) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Subscribe attaches to an execution's stream, replaying buffered messages
// with a sequence number greater than afterSeq.
//
// The second return value reports whether the execution has already
// finished, in which case C is closed and only Backlog matters.
func (b *Broker) Subscribe(executionID, afterSeq int64) (*Subscription, bool) {
	t := b.topicFor(executionID)

	t.mu.Lock()
	defer t.mu.Unlock()

	var backlog []Message
	for _, m := range t.ring {
		if m.Seq > afterSeq {
			backlog = append(backlog, m)
		}
	}

	if t.closed {
		closed := make(chan Message)
		close(closed)
		return &Subscription{Backlog: backlog, C: closed}, true
	}

	id := t.nextID
	t.nextID++
	ch := make(chan Message, subscriberBuffer)
	t.subs[id] = ch

	return &Subscription{
		Backlog: backlog,
		C:       ch,
		cancel: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if sub, ok := t.subs[id]; ok {
				delete(t.subs, id)
				close(sub)
			}
		},
	}, false
}

// Close ends a topic, closing every subscriber channel so open SSE responses
// terminate cleanly.
func (b *Broker) Close(executionID int64) {
	t := b.topicFor(executionID)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for id, ch := range t.subs {
		close(ch)
		delete(t.subs, id)
	}
}

// Forget drops all state for an execution, including its replay ring.
// Called once no subscriber is expected, to bound memory.
func (b *Broker) Forget(executionID int64) {
	b.Close(executionID)

	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.topics, executionID)
}

// SubscriberCount reports how many live subscribers an execution has.
func (b *Broker) SubscriberCount(executionID int64) int {
	t := b.topicFor(executionID)
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subs)
}
