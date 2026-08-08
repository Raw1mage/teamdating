package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type sseClient struct {
	resp   *http.Response
	reader *bufio.Reader
	cancel context.CancelFunc
}

func (c *client) openStream(t *testing.T, pid string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.srv.URL+"/api/proposals/"+pid+"/stream", nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	for _, cookie := range c.jar {
		req.AddCookie(cookie)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	stream := &sseClient{resp: resp, reader: bufio.NewReader(resp.Body), cancel: cancel}
	t.Cleanup(stream.close)
	return stream
}

func (s *sseClient) close() { s.cancel(); _ = s.resp.Body.Close() }

func (s *sseClient) next(t *testing.T) (string, json.RawMessage) {
	t.Helper()
	event, data, err := s.readEvent()
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	return event, data
}

func (s *sseClient) nextWithin(t *testing.T, wait time.Duration) (string, json.RawMessage) {
	t.Helper()
	type result struct {
		event string
		data  json.RawMessage
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		event, data, err := s.readEvent()
		resultCh <- result{event: event, data: data, err: err}
	}()
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("read SSE: %v", result.err)
		}
		return result.event, result.data
	case <-time.After(wait):
		t.Fatalf("SSE event did not arrive within %s; Flush may be missing", wait)
		return "", nil
	}
}

func (s *sseClient) readEvent() (string, json.RawMessage, error) {
	var event string
	var data json.RawMessage
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return event, data, nil
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data[:0], value...)
		}
	}
}

func TestSSEHeadersSnapshotAndMutation(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	p := a.createProposal("Trip")
	stream := a.openStream(t, p.PID)
	if got := stream.resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := stream.resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := stream.resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	event, data := stream.next(t)
	if event != "snapshot" || !strings.Contains(string(data), p.PID) {
		t.Fatalf("initial event = %q %s, want proposal snapshot", event, data)
	}
	a.addOption(p.PID, "2026-09-01", "2026-09-03")
	event, data = stream.nextWithin(t, 500*time.Millisecond)
	if event != "option_create" || !strings.Contains(string(data), p.PID) {
		t.Fatalf("mutation event = %q %s, want option_create", event, data)
	}
}

func TestPresenceRemovalFollowsStreamCancellation(t *testing.T) {
	e := newEnv(t)
	a, b := e.newClient("Alice"), e.newClient("Bob")
	p := a.createProposal("Trip")
	observer := a.openStream(t, p.PID)
	observer.next(t)
	participant := b.openStream(t, p.PID)
	participant.next(t)
	event, data := observer.next(t)
	if event != "presence" || !strings.Contains(string(data), b.UID) || !strings.Contains(string(data), "true") {
		t.Fatalf("join presence = %q %s", event, data)
	}
	participant.close()
	event, data = observer.next(t)
	if event != "presence" || !strings.Contains(string(data), b.UID) || !strings.Contains(string(data), "false") {
		t.Fatalf("leave presence = %q %s", event, data)
	}
}

func TestCursorFanoutIsTransient(t *testing.T) {
	e := newEnv(t)
	a, b := e.newClient("Alice"), e.newClient("Bob")
	p := a.createProposal("Trip")
	streamA := a.openStream(t, p.PID)
	streamA.next(t)
	streamB := b.openStream(t, p.PID)
	streamB.next(t)
	streamA.next(t)
	if code := a.do(http.MethodPost, "/api/proposals/"+p.PID+"/cursor", map[string]float64{"x": .25, "y": .75}, nil); code != http.StatusNoContent {
		t.Fatalf("cursor status = %d, want 204", code)
	}
	event, data := streamB.next(t)
	if event != "cursor" || !strings.Contains(string(data), `"x":0.25`) || !strings.Contains(string(data), `"y":0.75`) {
		t.Fatalf("receiver cursor = %q %s", event, data)
	}
	log, err := os.ReadFile(e.dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	if strings.Contains(string(log), "cursor") {
		t.Fatalf("cursor was persisted in events.jsonl: %s", log)
	}
	if !strings.Contains(string(log), "proposal_create") {
		t.Fatal("control group failed: no durable proposal event in event log")
	}
}

// TestStreamPayloadsAreUnwrapped pins the WIRE SHAPE of every streamed event.
//
// ★ This is the guard the fleet did not have when the cursor layer silently
// never rendered for the whole of M1. The bug was that mutation events were
// written as the whole hub.Message envelope -- {"type":..,"data":{..}} -- while
// the initial snapshot was written as a bare payload. One stream, two shapes.
//
// Nothing errored. The browser read d.uid / d.x off the envelope, got
// undefined, and took the "cursor left" branch, so "no cursor to draw" and
// "cannot draw the cursor" produced the same empty overlay.
//
// Note why the pre-existing assertions could not catch it: they used
// strings.Contains(data, `"x":0.25`), and that substring is present in the
// WRAPPED form too. Substring presence answers "does this appear anywhere",
// never "is the structure right" -- so the check had no discriminating power
// against exactly this defect. Decoding into a struct is what gives it teeth.
func TestStreamPayloadsAreUnwrapped(t *testing.T) {
	e := newEnv(t)
	a, b := e.newClient("Alice"), e.newClient("Bob")
	p := a.createProposal("Trip")

	// assertBare fails if the payload still carries the transport envelope,
	// i.e. if it decodes to an object holding BOTH "type" and "data".
	assertBare := func(event string, data json.RawMessage) {
		t.Helper()
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil && envelope.Type != "" && len(envelope.Data) > 0 {
			t.Fatalf("%s event still wrapped in a transport envelope: %s", event, data)
		}
	}

	streamA := a.openStream(t, p.PID)
	event, data := streamA.next(t)
	assertBare(event, data)

	streamB := b.openStream(t, p.PID)
	streamB.next(t)

	// presence: the receiver must be able to read uid/online off the top level.
	event, data = streamA.next(t)
	if event != "presence" {
		t.Fatalf("event = %q, want presence", event)
	}
	assertBare(event, data)
	var presence struct {
		UID    string `json:"uid"`
		Online bool   `json:"online"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	if presence.UID != b.UID || !presence.Online {
		t.Fatalf("presence = %+v, want uid=%s online=true", presence, b.UID)
	}

	// cursor: same, and this is the one that shipped broken.
	if code := a.do(http.MethodPost, "/api/proposals/"+p.PID+"/cursor", map[string]float64{"x": .25, "y": .75}, nil); code != http.StatusNoContent {
		t.Fatalf("cursor status = %d, want 204", code)
	}
	event, data = streamB.next(t)
	if event != "cursor" {
		t.Fatalf("event = %q, want cursor", event)
	}
	assertBare(event, data)
	var cursor struct {
		UID string   `json:"uid"`
		X   *float64 `json:"x"`
		Y   *float64 `json:"y"`
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	// Pointers, not bare float64: a missing key and a legitimate 0 must not
	// share one value, or an absent coordinate reads as the top-left corner.
	if cursor.X == nil || cursor.Y == nil {
		t.Fatalf("cursor coordinates absent from top level: %s", data)
	}
	if cursor.UID != a.UID || *cursor.X != .25 || *cursor.Y != .75 {
		t.Fatalf("cursor = %s, want uid=%s x=0.25 y=0.75", data, a.UID)
	}

	// mutation events travel the same path and must be bare too.
	a.addOption(p.PID, "2026-09-01", "2026-09-03")
	event, data = streamA.nextWithin(t, 500*time.Millisecond)
	if event != "option_create" {
		t.Fatalf("event = %q, want option_create", event)
	}
	assertBare(event, data)
}

// TestSSESnapshotCarriesViewerStance is the guard for the 2026-08-05 reload bug:
// the SSE snapshot MUST include the viewer's own myWeight, exactly like GET
// /proposals/:pid does. Before the fix, handleStream built the snapshot from the
// viewer-blind ProposalSnapshot(pid), so on reload the client GET'd the proposal
// WITH myWeight, rendered it, then the snapshot re-rendered over it WITHOUT
// myWeight — every option showed un-voted until any interaction re-GET'd it.
//
// ★ The discriminating input is a voter reloading their OWN stream. "snapshot
//   arrived" and "snapshot carries my stance" shared one output (the event
//   fired, just blank), which is why no earlier test caught it.
func TestSSESnapshotCarriesViewerStance(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	p := a.createProposal("Trip")
	pv := a.addOption(p.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID
	if code := a.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid, "weight": 1.0}, nil); code != http.StatusOK {
		t.Fatalf("vote: status %d", code)
	}

	// Alice reopens her own stream — this is the reload path.
	stream := a.openStream(t, p.PID)
	event, data := stream.next(t)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap snapshotEvent
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	raw, _ := json.Marshal(snap.Proposal)
	var pvSnap proposalView
	if err := json.Unmarshal(raw, &pvSnap); err != nil {
		t.Fatalf("decode snapshot proposal: %v", err)
	}
	if len(pvSnap.Options) != 1 {
		t.Fatalf("snapshot has %d options, want 1", len(pvSnap.Options))
	}
	mw := pvSnap.Options[0].MyWeight
	if mw == nil {
		t.Fatal("snapshot option has nil myWeight for a viewer who voted — " +
			"reload will show the option as un-voted until the user interacts")
	}
	if *mw != 1.0 {
		t.Fatalf("snapshot myWeight = %v, want 1.0", *mw)
	}
}

// TestSSESnapshotOmitsOthersStance is the control: a viewer who has NOT voted
// must get nil myWeight, so the assertion above is testing the viewer binding,
// not simply "myWeight is always set".
func TestSSESnapshotOmitsOthersStance(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	b := e.newClient("Bob")
	p := a.createProposal("Trip")
	pv := a.addOption(p.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID
	if code := a.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid, "weight": 1.0}, nil); code != http.StatusOK {
		t.Fatalf("vote: status %d", code)
	}

	// Bob (who never voted) opens the stream — his snapshot must not carry
	// Alice's stance as if it were his own.
	stream := b.openStream(t, p.PID)
	event, data := stream.next(t)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap snapshotEvent
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	raw, _ := json.Marshal(snap.Proposal)
	var pvSnap proposalView
	if err := json.Unmarshal(raw, &pvSnap); err != nil {
		t.Fatalf("decode snapshot proposal: %v", err)
	}
	if pvSnap.Options[0].MyWeight != nil {
		t.Fatalf("snapshot myWeight = %v for a viewer who never voted, want nil",
			*pvSnap.Options[0].MyWeight)
	}
}
