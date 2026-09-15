package ring

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultSessionTTL is how long a session survives without calls. An investigation
// has gaps — reading a dashboard, waiting for a colleague — and an hour covers
// them without letting yesterday's budget carry into today.
const DefaultSessionTTL = time.Hour

// TracePrefix names the service in the trace id, so a report that carries
// `Trace: ringsrv/7f3a91c2` says where to look.
const TracePrefix = "ringsrv/"

// Session is what one caller accumulates while they work.
type Session struct {
	TraceID string
	Writes  int

	lastSeen time.Time
}

// Sessions hands out per-caller state by key.
type Sessions struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	byID map[string]*Session
}

// NewSessions builds the manager. A zero ttl means DefaultSessionTTL; now is injectable
// for tests.
func NewSessions(ttl time.Duration, now func() time.Time) *Sessions {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Sessions{ttl: ttl, now: now, byID: map[string]*Session{}}
}

// Get returns the caller's session, creating it when there is none or when the
// previous one went cold. Expired entries are dropped on the way: the map is
// small and the sweep is cheaper than a goroutine that has to be stopped.
func (m *Sessions) Get(key string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.sweep(now)

	s, ok := m.byID[key]
	if !ok {
		s = &Session{TraceID: newTraceID()}
		m.byID[key] = s
	}
	s.lastSeen = now
	return s
}

// AddWrite counts one write attempt and reports the new total. Called once
// the method and the path passed the allowlist: a call refused there spends
// nothing, while a call that reaches the upstream spends one whether or not
// the upstream accepted it — the budget is about how often the model may try
// to change something, not about how often it succeeds.
func (m *Sessions) AddWrite(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.byID[key]
	if !ok {
		s = &Session{TraceID: newTraceID(), lastSeen: m.now()}
		m.byID[key] = s
	}
	s.Writes++
	return s.Writes
}

// Len reports how many sessions are held. For tests and for a metric.
func (m *Sessions) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

func (m *Sessions) sweep(now time.Time) {
	for k, s := range m.byID {
		if now.Sub(s.lastSeen) > m.ttl {
			delete(m.byID, k)
		}
	}
}

func newTraceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a trace id is not a
		// secret: a degraded id is better than a refused call.
		return TracePrefix + "unknown"
	}
	return TracePrefix + hex.EncodeToString(b[:])
}
