package hub

import (
	"encoding/json"
	"testing"
	"time"
)

func receive(t *testing.T, c <-chan Message) Message {
	t.Helper()
	select {
	case message, ok := <-c:
		if !ok {
			t.Fatal("subscriber disconnected unexpectedly")
		}
		return message
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hub message")
		return Message{}
	}
}

func TestCursorFansOutOnlyToOtherUsers(t *testing.T) {
	h := New()
	sender, _ := h.Subscribe("P1", "U1")
	receiver, _ := h.Subscribe("P1", "U2")
	defer h.Unsubscribe(sender)
	defer h.Unsubscribe(receiver)
	_ = receive(t, sender.C) // U2 presence
	h.Cursor("P1", "U1", .25, .75)
	message := receive(t, receiver.C)
	if message.Type != "cursor" {
		t.Fatalf("receiver got %q, want cursor", message.Type)
	}
	var cursor Cursor
	if err := json.Unmarshal(message.Data, &cursor); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cursor.UID != "U1" || cursor.X != .25 || cursor.Y != .75 {
		t.Errorf("cursor = %+v, want U1/.25/.75", cursor)
	}
	select {
	case message := <-sender.C:
		t.Errorf("sender received its own cursor: %+v", message)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	h := New()
	slow, _ := h.Subscribe("P1", "slow")
	defer h.Unsubscribe(slow)
	start := time.Now()
	for index := 0; index < subscriberBuffer+1; index++ {
		h.Observe(uint64(index+1), "option_create", json.RawMessage(`{"PID":"P1"}`))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("publisher blocked for %s on a full subscriber", elapsed)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-slow.C:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("slow subscriber was not disconnected after draining its buffer")
		}
	}
}

func TestUnsubscribeBroadcastsOfflinePresence(t *testing.T) {
	h := New()
	one, _ := h.Subscribe("P1", "U1")
	two, _ := h.Subscribe("P1", "U2")
	defer h.Unsubscribe(two)
	_ = receive(t, one.C) // U2 online
	h.Unsubscribe(two)
	message := receive(t, one.C)
	if message.Type != "presence" {
		t.Fatalf("message type = %q, want presence", message.Type)
	}
	var presence Presence
	if err := json.Unmarshal(message.Data, &presence); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	if presence.UID != "U2" || presence.Online {
		t.Errorf("offline presence = %+v, want U2 offline", presence)
	}
}
