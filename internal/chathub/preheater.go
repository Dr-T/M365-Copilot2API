package chathub

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type preheatedConn struct {
	conn    *websocket.Conn
	created time.Time
	wsURL   string
}

type Preheater struct {
	mu     sync.Mutex
	pool   map[string]*preheatedConn // key = oid|tid
	dialer *websocket.Dialer
	header http.Header
	stop   chan struct{}

	// callback to build a full WS URL for a given account; set by the Client
	BuildURL func(acc Account, sessionID, conversationID, requestID string) (string, error)
}

func NewPreheater() *Preheater {
	p := &Preheater{
		pool:   make(map[string]*preheatedConn),
		dialer: &websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
		},
		header: http.Header{},
		stop:   make(chan struct{}),
	}
	go p.gcLoop()
	return p
}

func (p *Preheater) key(oid, tid string) string { return oid + "|" + tid }

func (p *Preheater) Take(oid, tid string) *websocket.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.pool[p.key(oid, tid)]
	if !ok {
		return nil
	}
	delete(p.pool, p.key(oid, tid))
	// Validate the connection is still alive with a short read deadline
	_ = pc.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err := pc.conn.ReadMessage()
	if err == nil {
		// Got an unexpected message; connection is alive but has stale data, discard
		pc.conn.Close()
		return nil
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) ||
		websocket.IsCloseError(err, websocket.CloseGoingAway) ||
		websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
		pc.conn.Close()
		return nil
	}
	// Timeout or unexpected error is expected for idle connections (read deadline)
	_ = pc.conn.SetReadDeadline(time.Time{}) // reset deadline
	return pc.conn
}

func (p *Preheater) Warm(ctx context.Context, acc Account, wsURL string) {
	if wsURL == "" {
		return
	}
	key := p.key(acc.OID, acc.TID)

	// Don't preheat if we already have a warm connection
	p.mu.Lock()
	if existing, ok := p.pool[key]; ok {
		if time.Since(existing.created) < 30*time.Second {
			p.mu.Unlock()
			return
		}
		// Stale connection, remove it
		existing.conn.Close()
		delete(p.pool, key)
	}
	p.mu.Unlock()

	conn, resp, err := p.dialer.DialContext(ctx, wsURL, p.header.Clone())
	if err != nil {
		if resp != nil {
			log.Printf("[preheater] dial failed oid=%s status=%d err=%v", acc.OID, resp.StatusCode, err)
		} else {
			log.Printf("[preheater] dial failed oid=%s err=%v", acc.OID, err)
		}
		return
	}

	// Perform SignalR handshake
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+"\x1e")); err != nil {
		log.Printf("[preheater] handshake send failed: %v", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, err = conn.ReadMessage()
	if err != nil {
		log.Printf("[preheater] handshake recv failed: %v", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // reset

	p.mu.Lock()
	// Replace any stale connection
	if old, ok := p.pool[key]; ok {
		old.conn.Close()
	}
	p.pool[key] = &preheatedConn{conn: conn, created: time.Now(), wsURL: wsURL}
	p.mu.Unlock()

	log.Printf("[preheater] warmed connection oid=%s tid=%s age=0ms", acc.OID, acc.TID)
}

func (p *Preheater) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	warm := make([]map[string]any, 0, len(p.pool))
	for k, pc := range p.pool {
		warm = append(warm, map[string]any{"key": k, "age_ms": time.Since(pc.created).Milliseconds()})
	}
	return map[string]any{"mode": "preheater", "warm_connections": len(p.pool), "details": warm}
}

func (p *Preheater) GC() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, pc := range p.pool {
		if now.Sub(pc.created) > 2*time.Minute {
			pc.conn.Close()
			delete(p.pool, k)
		}
	}
}

func (p *Preheater) Close() {
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pc := range p.pool {
		pc.conn.Close()
		delete(p.pool, k)
	}
}

func (p *Preheater) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.GC()
		}
	}
}
