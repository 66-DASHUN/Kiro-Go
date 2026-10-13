package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"kiro-proxy/config"
)

var upstreamAccountConcurrency = newAccountConcurrencyLimiter(60 * time.Second)

type accountConcurrencyLimiter struct {
	mu          sync.Mutex
	accounts    map[string]*accountConcurrencyState
	waitTimeout time.Duration
}

type accountConcurrencyState struct {
	account config.Account
	active  int
	waiters []*accountConcurrencyWaiter
}

type accountConcurrencyWaiter struct {
	ctx     context.Context
	ready   chan struct{}
	granted bool // protected by the registry mutex
}

func newAccountConcurrencyLimiter(waitTimeout time.Duration) *accountConcurrencyLimiter {
	return &accountConcurrencyLimiter{accounts: make(map[string]*accountConcurrencyState), waitTimeout: waitTimeout}
}

func accountConcurrencyKey(account *config.Account) string {
	if account == nil {
		return "anonymous"
	}
	if account.ID != "" {
		return "id:" + account.ID
	}
	// Persisted accounts have stable IDs. Keep legacy in-memory credentials private.
	credential := account.RefreshTokenFingerprint
	if credential == "" {
		credential = config.RefreshTokenFingerprint(account.RefreshToken)
	}
	if credential == "" {
		credential = account.KiroApiKey
	}
	if credential == "" {
		credential = account.UserId
	}
	if credential == "" {
		credential = account.AccessToken
	}
	return fmt.Sprintf("credential:%x", sha256.Sum256([]byte(credential)))
}

// acquire covers the whole upstream operation, including reading a streaming body.
func (l *accountConcurrencyLimiter) acquire(ctx context.Context, account *config.Account) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := accountConcurrencyKey(account)
	waiter := &accountConcurrencyWaiter{ctx: ctx, ready: make(chan struct{})}
	l.mu.Lock()
	state := l.accounts[key]
	if state == nil {
		state = &accountConcurrencyState{}
		if account != nil {
			state.account = *account
		}
		l.accounts[key] = state
	}
	state.waiters = append(state.waiters, waiter)
	l.drainLocked(key, state)
	l.mu.Unlock()

	timer := time.NewTimer(l.waitTimeout)
	defer timer.Stop()
	var waitErr error
	select {
	case <-waiter.ready:
		waitErr = ctx.Err()
	case <-ctx.Done():
		waitErr = ctx.Err()
	case <-timer.C:
		waitErr = &upstreamClientError{status: http.StatusTooManyRequests, message: "Account concurrency limit reached; queue wait timed out. Please retry later."}
	}
	if waitErr != nil {
		l.mu.Lock()
		if waiter.granted {
			state.active--
		} else {
			for i, pending := range state.waiters {
				if pending == waiter {
					state.waiters = append(state.waiters[:i], state.waiters[i+1:]...)
					break
				}
			}
		}
		l.drainLocked(key, state)
		l.mu.Unlock()
		return nil, waitErr
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			state.active--
			l.drainLocked(key, state)
		})
	}, nil
}

func (l *accountConcurrencyLimiter) drainLocked(key string, state *accountConcurrencyState) {
	limit := config.GetAccountConcurrencyLimit(&state.account)
	for state.active < limit && len(state.waiters) > 0 {
		waiter := state.waiters[0]
		state.waiters[0] = nil
		state.waiters = state.waiters[1:]
		if waiter.ctx.Err() != nil {
			continue
		}
		state.active++
		waiter.granted = true
		close(waiter.ready)
	}
	if state.active == 0 && len(state.waiters) == 0 && l.accounts[key] == state {
		delete(l.accounts, key)
	}
}

// notify is called after persistence; increases wake queued requests immediately.
func (l *accountConcurrencyLimiter) notify(account *config.Account) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := accountConcurrencyKey(account)
	if state := l.accounts[key]; state != nil {
		if account != nil {
			state.account = *account
		}
		l.drainLocked(key, state)
	}
}
