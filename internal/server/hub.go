package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// hub wakes long polls. Every server LISTENs on one PostgreSQL channel; a NOTIFY with a host's FQDN
// wakes that host's waiting polls on every server, so a change saved on one node reaches an agent
// polling another.
type hub struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{waiters: map[string]map[chan struct{}]struct{}{}} }

// wait blocks until the host is woken, the timeout passes or ctx ends.
func (h *hub) wait(ctx context.Context, fqdn string, d time.Duration) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters[fqdn] == nil {
		h.waiters[fqdn] = map[chan struct{}]struct{}{}
	}
	h.waiters[fqdn][ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waiters[fqdn], ch)
		h.mu.Unlock()
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-ch:
	}
}

func (h *hub) wake(fqdn string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.waiters[fqdn] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// listen holds a connection that LISTENs and wakes local waiters. If the connection drops it
// reconnects; polls still end at their timeout meanwhile.
func (h *hub) listen(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		// The connection is taken out of the pool for good: a LISTENing connection must not be reused.
		c := conn.Hijack()
		if _, err := c.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
			_ = c.Close(context.Background())
			time.Sleep(time.Second)
			continue
		}
		for {
			n, err := c.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("notification listener lost its connection; reconnecting", "err", err)
				}
				break
			}
			h.wake(n.Payload)
		}
		_ = c.Close(context.Background())
	}
}
