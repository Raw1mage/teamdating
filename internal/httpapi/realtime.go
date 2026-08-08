package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"teamdating/internal/auth"
	"teamdating/internal/hub"
)

type snapshotEvent struct {
	Seq      uint64         `json:"seq"`
	Proposal any            `json:"proposal"`
	Presence []hub.Presence `json:"presence"`
}

type cursorReq struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (a *api) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "SSE_UNSUPPORTED", "回應不支援串流")
		return
	}
	pid := r.PathValue("pid")
	if _, ok := a.store.Proposal(pid); !ok {
		writeErr(w, http.StatusNotFound, auth.CodeProposalNotFound, "找不到此提案")
		return
	}
	id := identify(a.store, r)
	sub, presence := a.hub.Subscribe(pid, id.UID)
	defer a.hub.Unsubscribe(sub)
	// ★ snapshot MUST carry the viewer's own stance (myWeight), exactly like the
	//   GET /proposals/:pid path does — use viewProposal(pid, id), not the
	//   viewer-blind ProposalSnapshot. The bug this fixes (2026-08-05): on
	//   reload the client first GETs the proposal WITH myWeight and renders it,
	//   then the SSE snapshot arrived WITHOUT myWeight and re-rendered over it,
	//   so every option showed as un-voted until any interaction (which re-GETs
	//   via applyDelta and restores it). "snapshot present" and "snapshot
	//   carries my stance" shared one output: the event fired, just blank.
	_, seq, ok := a.store.ProposalSnapshot(pid)
	if !ok {
		writeErr(w, http.StatusNotFound, auth.CodeProposalNotFound, "找不到此提案")
		return
	}
	proposal := a.viewProposal(pid, id)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if !writeSSE(w, "snapshot", snapshotEvent{Seq: seq, Proposal: proposal, Presence: presence}) {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-sub.C:
			if !ok {
				return
			}
			if msg.Seq <= seq && msg.Seq != 0 {
				continue
			}
			// ★ 送裸 payload，不送 Message 信封。事件種類已由 SSE 的
			// `event:` 欄位承載，再包一層 {type,data} 會讓同一條串流出現
			// 兩種格式（snapshot 是裸的、其餘是包的），而多包的那一層
			// 不會產生任何錯誤 —— 前端只是讀到 undefined，於是游標永遠
			// 不畫、presence 反被清空。缺席與失敗共用同一個輸出。
			if !writeSSE(w, msg.Type, msg.Data) {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *api) handleCursor(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if !id.Registered {
		writeErr(w, http.StatusForbidden, auth.CodeIdentityUnregistered, "請先登記姓名才能分享游標")
		return
	}
	if _, ok := a.store.Proposal(pid); !ok {
		writeErr(w, http.StatusNotFound, auth.CodeProposalNotFound, "找不到此提案")
		return
	}
	var req cursorReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.X < 0 || req.X > 1 || req.Y < 0 || req.Y > 1 {
		writeErr(w, http.StatusBadRequest, "CURSOR_INVALID", "游標座標必須介於 0 到 1")
		return
	}
	a.hub.Cursor(pid, id.UID, req.X, req.Y)
	w.WriteHeader(http.StatusNoContent)
}

func writeSSE(w http.ResponseWriter, event string, data any) bool {
	b, err := json.Marshal(data)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err == nil
}
