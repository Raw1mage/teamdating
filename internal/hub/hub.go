// Package hub distributes transient proposal updates to SSE connections.
package hub

import (
	"encoding/json"
	"sync"
)

const subscriberBuffer = 16

type Message struct {
	Seq  uint64          `json:"seq,omitempty"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type Presence struct {
	UID    string `json:"uid"`
	Online bool   `json:"online"`
}

type Cursor struct {
	UID string  `json:"uid"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
}

type Subscription struct {
	PID string
	UID string
	C   <-chan Message
	id  uint64
}

type subscriber struct {
	uid string
	ch  chan Message
}

type proposalSubscribers struct {
	subs     map[uint64]subscriber
	presence map[string]int
}

type Hub struct {
	mu     sync.Mutex
	nextID uint64
	byPID  map[string]*proposalSubscribers
}

func New() *Hub {
	return &Hub{byPID: make(map[string]*proposalSubscribers)}
}

func (h *Hub) Subscribe(pid, uid string) (*Subscription, []Presence) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.byPID[pid]
	if p == nil {
		p = &proposalSubscribers{subs: make(map[uint64]subscriber), presence: make(map[string]int)}
		h.byPID[pid] = p
	}
	present := make([]Presence, 0, len(p.presence))
	for existingUID := range p.presence {
		present = append(present, Presence{UID: existingUID, Online: true})
	}
	h.nextID++
	ch := make(chan Message, subscriberBuffer)
	p.subs[h.nextID] = subscriber{uid: uid, ch: ch}
	p.presence[uid]++
	if p.presence[uid] == 1 {
		h.broadcastLocked(p, Message{Type: "presence", Data: marshal(Presence{UID: uid, Online: true})}, h.nextID)
	}
	return &Subscription{PID: pid, UID: uid, C: ch, id: h.nextID}, present
}

func (h *Hub) Unsubscribe(s *Subscription) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.byPID[s.PID]
	if p == nil {
		return
	}
	sub, ok := p.subs[s.id]
	if !ok {
		return
	}
	delete(p.subs, s.id)
	close(sub.ch)
	p.presence[sub.uid]--
	if p.presence[sub.uid] == 0 {
		delete(p.presence, sub.uid)
		h.broadcastLocked(p, Message{Type: "presence", Data: marshal(Presence{UID: sub.uid, Online: false})}, 0)
	}
	if len(p.subs) == 0 {
		delete(h.byPID, s.PID)
	}
}

func (h *Hub) Observe(seq uint64, typ string, data json.RawMessage) {
	var target struct {
		PID string `json:"PID"`
	}
	if json.Unmarshal(data, &target) != nil || target.PID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.byPID[target.PID]; p != nil {
		h.broadcastLocked(p, Message{Seq: seq, Type: typ, Data: append(json.RawMessage(nil), data...)}, 0)
	}
}

func (h *Hub) Cursor(pid, uid string, x, y float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.byPID[pid]; p != nil {
		for id, sub := range p.subs {
			if sub.uid == uid {
				continue
			}
			select {
			case sub.ch <- Message{Type: "cursor", Data: marshal(Cursor{UID: uid, X: x, Y: y})}:
			default:
				delete(p.subs, id)
				close(sub.ch)
				p.presence[sub.uid]--
				if p.presence[sub.uid] == 0 {
					delete(p.presence, sub.uid)
				}
			}
		}
	}
}

func (h *Hub) broadcastLocked(p *proposalSubscribers, msg Message, exclude uint64) {
	for id, sub := range p.subs {
		if id == exclude {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
			delete(p.subs, id)
			close(sub.ch)
			p.presence[sub.uid]--
			if p.presence[sub.uid] == 0 {
				delete(p.presence, sub.uid)
			}
		}
	}
}

func marshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
