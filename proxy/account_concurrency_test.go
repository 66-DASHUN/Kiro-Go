package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/config"
)

func TestAdminAccountConcurrencyDefaultAndPatch(t *testing.T) {
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithoutBackgroundForTest(t)
	h.pool.Reload()
	check := func(want float64) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.apiGetAccounts(rec, httptest.NewRequest("GET", "/", nil))
		var accounts []map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 1 || accounts[0]["maxConcurrency"] != want {
			t.Fatalf("account list concurrency=%v, want %v", accounts, want)
		}
		rec = httptest.NewRecorder()
		h.apiGetAccountFull(rec, httptest.NewRequest("GET", "/", nil), "one")
		var full map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &full)
		if full["maxConcurrency"] != want {
			t.Fatalf("full account concurrency=%v, want %v", full["maxConcurrency"], want)
		}
	}
	check(5)
	rec := httptest.NewRecorder()
	h.apiUpdateAccount(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"maxConcurrency":2}`)), "one")
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	check(2)
	h.apiUpdateAccount(httptest.NewRecorder(), httptest.NewRequest("PUT", "/", strings.NewReader(`{"nickname":"next"}`)), "one")
	check(2)
}

func TestAdminAccountConcurrencyRejectsInvalidPatch(t *testing.T) {
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithoutBackgroundForTest(t)
	for _, value := range []string{"0", "-1", "1.5", `"5"`, "null", "true", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.apiUpdateAccount(rec, httptest.NewRequest("PUT", "/", strings.NewReader(`{"maxConcurrency":`+value+`}`)), "one")
			if rec.Code != 400 {
				t.Fatalf("invalid limit %s accepted: status %d", value, rec.Code)
			}
		})
	}
}

func TestAccountConcurrencySharedAcrossProtocolsAndStreamLifetime(t *testing.T) {
	entered := make(chan struct{}, 10)
	finish := make(chan struct{}, 10)
	firstFrame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})
	trailer := meteringFrame(t)
	h := newStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write(firstFrame)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		select {
		case <-finish:
			w.Write(trailer)
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); close(finish); wg.Wait() })
	done := make(chan int, 6)
	start := func(index int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}[index%3]
			body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":100,"stream":%t,"messages":[{"role":"user","content":"hello"}]}`, index%2 == 1)
			if index%3 == 2 {
				body = fmt.Sprintf(`{"model":"claude-sonnet-4.5","stream":%t,"input":"hello"}`, index%2 == 1)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx)
			switch index % 3 {
			case 0:
				h.handleClaudeMessages(rec, req)
			case 1:
				h.handleOpenAIChat(rec, req)
			case 2:
				h.handleOpenAIResponses(rec, req)
			}
			done <- rec.Code
		}()
	}
	for i := 0; i < 5; i++ {
		start(i)
		awaitConcurrencyEntry(t, entered)
	}
	start(5)
	select {
	case <-entered:
		t.Fatal("sixth request reached upstream while five streams remained active")
	case <-time.After(200 * time.Millisecond):
	}
	finish <- struct{}{}
	awaitConcurrencyEntry(t, entered)
	for i := 0; i < 5; i++ {
		finish <- struct{}{}
	}
	for i := 0; i < 6; i++ {
		select {
		case code := <-done:
			if code != 200 {
				t.Errorf("request status %d", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("requests did not finish")
		}
	}
}

func awaitConcurrencyEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach upstream")
	}
}

type concurrencyAcquireResult struct {
	release func()
	err     error
}

func queueConcurrencyCall(l *accountConcurrencyLimiter, ctx context.Context, account *config.Account) <-chan concurrencyAcquireResult {
	done := make(chan concurrencyAcquireResult, 1)
	go func() { release, err := l.acquire(ctx, account); done <- concurrencyAcquireResult{release, err} }()
	return done
}

func awaitConcurrencyWaiters(t *testing.T, l *accountConcurrencyLimiter, account *config.Account, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		state := l.accounts[accountConcurrencyKey(account)]
		got := 0
		if state != nil {
			got = len(state.waiters)
		}
		l.mu.Unlock()
		if got == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter count did not become %d", count)
}

func awaitConcurrencyGrant(t *testing.T, done <-chan concurrencyAcquireResult) func() {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.release
	case <-time.After(3 * time.Second):
		t.Fatal("slot was not granted")
	}
	return nil
}

func TestAccountConcurrencyIndependentAccountsAndAPIKeys(t *testing.T) {
	l := newAccountConcurrencyLimiter(time.Second)
	a := &config.Account{ID: "limit-a", ApiKeyID: "first-key", MaxConcurrency: 1}
	b := &config.Account{ID: "limit-b", MaxConcurrency: 1}
	release, err := l.acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	copyA := *a
	copyA.ApiKeyID = "second-key"
	queued := queueConcurrencyCall(l, context.Background(), &copyA)
	awaitConcurrencyWaiters(t, l, a, 1)
	otherRelease, err := l.acquire(context.Background(), b)
	if err != nil {
		t.Fatal("independent account blocked:", err)
	}
	otherRelease()
	release()
	granted := awaitConcurrencyGrant(t, queued)
	granted()
	granted() // Duplicate release cannot create an extra slot.
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.accounts) != 0 {
		t.Fatalf("idle states leaked: %d", len(l.accounts))
	}
}

func TestAccountConcurrencyCanceledWaiterAndFIFO(t *testing.T) {
	l := newAccountConcurrencyLimiter(time.Second)
	a := &config.Account{ID: "limit-fifo", MaxConcurrency: 1}
	release, err := l.acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := queueConcurrencyCall(l, ctx, a)
	awaitConcurrencyWaiters(t, l, a, 1)
	first := queueConcurrencyCall(l, context.Background(), a)
	awaitConcurrencyWaiters(t, l, a, 2)
	second := queueConcurrencyCall(l, context.Background(), a)
	awaitConcurrencyWaiters(t, l, a, 3)
	cancel()
	result := <-canceled
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled waiter error=%v", result.err)
	}
	release()
	firstRelease := awaitConcurrencyGrant(t, first)
	select {
	case result := <-second:
		if result.release != nil {
			result.release()
		}
		t.Fatal("later waiter jumped the queue")
	default:
	}
	firstRelease()
	awaitConcurrencyGrant(t, second)()
}

func TestAccountConcurrencyHotLimitRaiseAndLower(t *testing.T) {
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "hot", MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithoutBackgroundForTest(t)
	a := testAccount(t) // Keep this old snapshot through every update.
	release, err := upstreamAccountConcurrency.acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	second := queueConcurrencyCall(upstreamAccountConcurrency, context.Background(), a)
	awaitConcurrencyWaiters(t, upstreamAccountConcurrency, a, 1)
	patch := func(limit int) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.apiUpdateAccount(rec, httptest.NewRequest("PUT", "/", strings.NewReader(fmt.Sprintf(`{"maxConcurrency":%d}`, limit))), a.ID)
		if rec.Code != 200 {
			t.Fatalf("limit edit: %d %s", rec.Code, rec.Body.String())
		}
	}
	patch(2)
	secondRelease := awaitConcurrencyGrant(t, second)
	defer secondRelease()
	patch(1)
	third := queueConcurrencyCall(upstreamAccountConcurrency, context.Background(), a)
	awaitConcurrencyWaiters(t, upstreamAccountConcurrency, a, 1)
	release()
	awaitConcurrencyWaiters(t, upstreamAccountConcurrency, a, 1) // One active still fills the lower limit.
	select {
	case result := <-third:
		if result.release != nil {
			result.release()
		}
		t.Fatal("lower limit admitted another call")
	default:
	}
	secondRelease()
	awaitConcurrencyGrant(t, third)()
}

func TestAccountConcurrencyTimeoutDoesNotCoolAccount(t *testing.T) {
	if err := config.Init(proxyTempDir(t) + "/kiro.db"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "timeout", Enabled: true, AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix(), MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithoutBackgroundForTest(t)
	h.pool.Reload()
	a := testAccount(t)
	l := newAccountConcurrencyLimiter(20 * time.Millisecond)
	release, err := l.acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = l.acquire(context.Background(), a)
	if !isUpstreamClientError(err) || upstreamFailureStatus(err) != 429 {
		t.Fatalf("local timeout is not a client 429: %v", err)
	}
	for i := 0; i < 4; i++ {
		h.handleAccountFailure(a, err)
	}
	if h.pool.GetNext() == nil {
		t.Fatal("local saturation cooled the account")
	}
	release()
	nextRelease, err := l.acquire(context.Background(), a)
	if err != nil {
		t.Fatal("timeout leaked a slot:", err)
	}
	nextRelease()
}

func TestAccountConcurrencyUpstreamErrorReleasesSlot(t *testing.T) {
	var calls int
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"})
	trailer := meteringFrame(t)
	newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "bad request", 400)
			return
		}
		writeFrames(w, frame, trailer)
	})
	limit := 1
	if _, err := config.UpdateAccountPatch("only", config.AccountPatch{MaxConcurrency: &limit}); err != nil {
		t.Fatal(err)
	}
	a := testAccount(t)
	if err := CallKiroAPIContext(context.Background(), a, testKiroPayload(), nil); err == nil {
		t.Fatal("upstream failure was ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := CallKiroAPIContext(ctx, a, testKiroPayload(), nil); err != nil {
		t.Fatal("failed request leaked its slot:", err)
	}
}
