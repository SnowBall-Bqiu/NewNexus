package events

import "sync"

type Broker struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]chan []byte
}

func NewBroker() *Broker { return &Broker{subscribers: make(map[int]chan []byte)} }

func (b *Broker) Subscribe() (int, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	channel := make(chan []byte, 16)
	b.subscribers[b.nextID] = channel
	return b.nextID, channel
}

func (b *Broker) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if channel, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(channel)
	}
}

func (b *Broker) Publish(payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, channel := range b.subscribers {
		select {
		case channel <- append([]byte(nil), payload...):
		default:
		}
	}
}
