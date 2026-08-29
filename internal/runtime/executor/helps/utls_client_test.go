package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Parallel()

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "chatgpt.com" {
			t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

func TestNewUtlsHTTPClientDoesNotPoolMissingAuthIdentity(t *testing.T) {
	clientA := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	clientB := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	transportA, okA := clientA.Transport.(*utlsTransportBundle)
	transportB, okB := clientB.Transport.(*utlsTransportBundle)
	if !okA || !okB {
		t.Fatalf("nil-auth transports = %T and %T, want isolated *utlsTransportBundle values", clientA.Transport, clientB.Transport)
	}
	if transportA == transportB {
		t.Fatal("nil-auth clients unexpectedly share a transport")
	}
}

type fakePooledTransport struct {
	idleCloses atomic.Int64
	roundTrips atomic.Int64
}

func (t *fakePooledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.roundTrips.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    req,
	}, nil
}

func (t *fakePooledTransport) CloseIdleConnections() {
	t.idleCloses.Add(1)
}

func TestUtlsTransportPoolReusesTransportConcurrently(t *testing.T) {
	const requestCount = 128
	var created atomic.Int64
	fake := &fakePooledTransport{}
	pool := newUtlsTransportPool(8, 0, func(utlsTransportKey) closeIdleRoundTripper {
		created.Add(1)
		return fake
	})
	roundTripper := &pooledUtlsRoundTripper{
		pool: pool,
		key:  newUtlsTransportKey(&cliproxyauth.Auth{ID: "auth-a", Provider: "codex"}, "direct"),
	}

	start := make(chan struct{})
	errors := make(chan error, requestCount)
	var wg sync.WaitGroup
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, errRequest := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
			if errRequest != nil {
				errors <- errRequest
				return
			}
			resp, errRoundTrip := roundTripper.RoundTrip(req)
			if errRoundTrip != nil {
				errors <- errRoundTrip
				return
			}
			if _, errRead := io.ReadAll(resp.Body); errRead != nil {
				errors <- errRead
				return
			}
			if errClose := resp.Body.Close(); errClose != nil {
				errors <- errClose
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent round trip failed: %v", err)
	}

	if got := created.Load(); got != 1 {
		t.Fatalf("created transports = %d, want 1", got)
	}
	if got := fake.roundTrips.Load(); got != requestCount {
		t.Fatalf("round trips = %d, want %d", got, requestCount)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if got := len(pool.entries); got != 1 {
		t.Fatalf("pool entries = %d, want 1", got)
	}
	for _, entry := range pool.entries {
		if entry.active != 0 {
			t.Fatalf("active requests = %d, want 0", entry.active)
		}
	}
}

func TestUtlsTransportPoolAllowsActiveIdentityOverflowThenTrimsToIdleCap(t *testing.T) {
	const identityCount = utlsTransportCacheEntries + 64
	transports := make(map[utlsTransportKey]*fakePooledTransport, identityCount)
	pool := newUtlsTransportPool(utlsTransportCacheEntries, 0, func(key utlsTransportKey) closeIdleRoundTripper {
		transport := &fakePooledTransport{}
		transports[key] = transport
		return transport
	})
	type result struct {
		response *http.Response
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, identityCount)
	for i := 0; i < identityCount; i++ {
		key := utlsTransportKey{
			provider:     "codex",
			authIdentity: fmt.Sprintf("id:%d", i),
			egress:       "direct",
		}
		go func() {
			<-start
			req, errRequest := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
			if errRequest != nil {
				results <- result{err: errRequest}
				return
			}
			response, errRoundTrip := (&pooledUtlsRoundTripper{pool: pool, key: key}).RoundTrip(req)
			results <- result{response: response, err: errRoundTrip}
		}()
	}
	close(start)
	responses := make([]*http.Response, 0, identityCount)
	deadline := time.After(2 * time.Second)
	for len(responses) < identityCount {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("parallel identity round trip: %v", got.err)
			}
			responses = append(responses, got.response)
		case <-deadline:
			t.Fatalf("only %d/%d active identities completed; possible global queue", len(responses), identityCount)
		}
	}

	pool.mu.Lock()
	if got := len(pool.entries); got != identityCount {
		pool.mu.Unlock()
		t.Fatalf("active pool entries = %d, want %d", got, identityCount)
	}
	for key, entry := range pool.entries {
		if entry.active != 1 {
			pool.mu.Unlock()
			t.Fatalf("active requests for %v = %d, want 1", key, entry.active)
		}
	}
	pool.mu.Unlock()
	for _, transport := range transports {
		if got := transport.idleCloses.Load(); got != 0 {
			t.Fatalf("active transport was evicted %d times", got)
		}
	}

	for _, response := range responses {
		if errClose := response.Body.Close(); errClose != nil {
			t.Fatalf("close response body: %v", errClose)
		}
	}
	pool.mu.Lock()
	if got := len(pool.entries); got != utlsTransportCacheEntries {
		pool.mu.Unlock()
		t.Fatalf("idle pool entries = %d, want cap %d", got, utlsTransportCacheEntries)
	}
	for key, entry := range pool.entries {
		if entry.active != 0 {
			pool.mu.Unlock()
			t.Fatalf("active requests after close for %v = %d, want 0", key, entry.active)
		}
	}
	pool.mu.Unlock()
	var idleCloses int64
	for _, transport := range transports {
		idleCloses += transport.idleCloses.Load()
	}
	if want := int64(identityCount - utlsTransportCacheEntries); idleCloses != want {
		t.Fatalf("idle transport evictions = %d, want %d", idleCloses, want)
	}
}

func TestUtlsTransportPoolDoesNotReuseAcrossAuthProviderOrProxy(t *testing.T) {
	var created atomic.Int64
	pool := newUtlsTransportPool(8, 0, func(utlsTransportKey) closeIdleRoundTripper {
		created.Add(1)
		return &fakePooledTransport{}
	})

	authA := &cliproxyauth.Auth{ID: "auth-a", Provider: "codex"}
	authB := &cliproxyauth.Auth{ID: "auth-b", Provider: "codex"}
	authClaude := &cliproxyauth.Auth{ID: "auth-a", Provider: "claude"}
	keys := []utlsTransportKey{
		newUtlsTransportKey(authA, "socks5://proxy-a.example:1080"),
		newUtlsTransportKey(authA, "socks5://proxy-a.example:1080"),
		newUtlsTransportKey(authB, "socks5://proxy-a.example:1080"),
		newUtlsTransportKey(authClaude, "socks5://proxy-a.example:1080"),
		newUtlsTransportKey(authA, "socks5://proxy-b.example:1080"),
	}
	for _, key := range keys {
		resp := roundTripWithPool(t, pool, key)
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("close response body: %v", errClose)
		}
	}

	if got := created.Load(); got != 4 {
		t.Fatalf("created transports = %d, want 4 isolated identities", got)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if got := len(pool.entries); got != 4 {
		t.Fatalf("pool entries = %d, want 4", got)
	}
}

func TestUtlsTransportPoolEvictionDoesNotCloseActiveTransport(t *testing.T) {
	transports := make(map[utlsTransportKey]*fakePooledTransport)
	pool := newUtlsTransportPool(1, 0, func(key utlsTransportKey) closeIdleRoundTripper {
		transport := &fakePooledTransport{}
		transports[key] = transport
		return transport
	})
	keyA := utlsTransportKey{provider: "codex", authIdentity: "id:a", egress: "direct"}
	keyB := utlsTransportKey{provider: "codex", authIdentity: "id:b", egress: "direct"}
	keyC := utlsTransportKey{provider: "codex", authIdentity: "id:c", egress: "direct"}

	respA := roundTripWithPool(t, pool, keyA)
	respB := roundTripWithPool(t, pool, keyB)
	if errClose := respB.Body.Close(); errClose != nil {
		t.Fatalf("close response B: %v", errClose)
	}
	if got := transports[keyA].idleCloses.Load(); got != 0 {
		t.Fatalf("active transport A was closed %d times", got)
	}
	if got := transports[keyB].idleCloses.Load(); got != 1 {
		t.Fatalf("overflow transport B close calls = %d, want 1", got)
	}

	if errClose := respA.Body.Close(); errClose != nil {
		t.Fatalf("close response A: %v", errClose)
	}
	respC := roundTripWithPool(t, pool, keyC)
	if got := transports[keyA].idleCloses.Load(); got != 1 {
		t.Fatalf("idle transport A close calls = %d, want 1", got)
	}
	if errClose := respC.Body.Close(); errClose != nil {
		t.Fatalf("close response C: %v", errClose)
	}
}

func TestUtlsTransportPoolEvictsExpiredIdleTransport(t *testing.T) {
	now := time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC)
	fake := &fakePooledTransport{}
	pool := newUtlsTransportPool(4, time.Minute, func(utlsTransportKey) closeIdleRoundTripper {
		return fake
	})
	pool.now = func() time.Time { return now }
	key := utlsTransportKey{provider: "codex", authIdentity: "id:a", egress: "direct"}

	resp := roundTripWithPool(t, pool, key)
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close response: %v", errClose)
	}
	now = now.Add(2 * time.Minute)
	pool.evictExpired()

	if got := fake.idleCloses.Load(); got != 1 {
		t.Fatalf("expired transport close calls = %d, want 1", got)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if got := len(pool.entries); got != 0 {
		t.Fatalf("pool entries after expiry = %d, want 0", got)
	}
}

func TestUtlsTransportPoolReleasesAbandonedBodyOnContextCancel(t *testing.T) {
	fake := &fakePooledTransport{}
	pool := newUtlsTransportPool(4, 0, func(utlsTransportKey) closeIdleRoundTripper {
		return fake
	})
	key := utlsTransportKey{provider: "codex", authIdentity: "id:a", egress: "direct"}
	ctx, cancel := context.WithCancel(context.Background())
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}
	resp, errRoundTrip := (&pooledUtlsRoundTripper{pool: pool, key: key}).RoundTrip(req)
	if errRoundTrip != nil {
		t.Fatalf("round trip: %v", errRoundTrip)
	}
	if got := poolActiveRequests(pool, key); got != 1 {
		t.Fatalf("active requests before cancel = %d, want 1", got)
	}

	cancel()
	waitForPoolActiveRequests(t, pool, key, 0)
	pool.mu.Lock()
	lruAfterCancel := pool.lru
	pool.mu.Unlock()
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close abandoned response body: %v", errClose)
	}
	pool.mu.Lock()
	lruAfterClose := pool.lru
	pool.mu.Unlock()
	if lruAfterClose != lruAfterCancel {
		t.Fatalf("body released more than once: lru changed from %d to %d", lruAfterCancel, lruAfterClose)
	}
}

func TestNewUtlsTransportBundleRejectsInvalidExplicitProxy(t *testing.T) {
	transport := newUtlsTransportBundle("this-is-not-a-proxy-url")
	if _, ok := transport.(*failingUtlsTransport); !ok {
		t.Fatalf("invalid proxy transport = %T, want *failingUtlsTransport", transport)
	}
	req, errRequest := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}
	if _, errRoundTrip := transport.RoundTrip(req); errRoundTrip == nil {
		t.Fatal("invalid explicit proxy unexpectedly fell back to direct egress")
	}
}

type blockingNonContextDialer struct {
	started chan struct{}
	release chan struct{}
	conn    net.Conn
	once    sync.Once
}

func (d *blockingNonContextDialer) Dial(string, string) (net.Conn, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return d.conn, nil
}

func TestDialUtlsContextCancelsNonContextDialerAndClosesLateConnection(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	defer func() { _ = peerConn.Close() }()
	dialer := &blockingNonContextDialer{
		started: make(chan struct{}),
		release: make(chan struct{}),
		conn:    clientConn,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		conn, errDial := dialUtlsContext(ctx, dialer, "tcp", "proxy.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		result <- errDial
	}()
	<-dialer.started
	cancel()
	select {
	case errDial := <-result:
		if !errors.Is(errDial, context.Canceled) {
			t.Fatalf("dial error = %v, want context.Canceled", errDial)
		}
	case <-time.After(time.Second):
		t.Fatal("dial did not return after context cancellation")
	}

	close(dialer.release)
	peerRead := make(chan error, 1)
	go func() {
		var buffer [1]byte
		_, errRead := peerConn.Read(buffer[:])
		peerRead <- errRead
	}()
	select {
	case errRead := <-peerRead:
		if errRead == nil {
			t.Fatal("late dial connection remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("late dial connection was not closed")
	}
}

func TestNewUtlsHTTP2TransportKeepsBurstCapacityManaged(t *testing.T) {
	transport, errTransport := newUtlsHTTP2Transport("direct")
	if errTransport != nil {
		t.Fatalf("create direct transport: %v", errTransport)
	}
	if transport.StrictMaxConcurrentStreams {
		t.Fatal("StrictMaxConcurrentStreams must remain disabled to avoid a hidden per-auth queue")
	}
	if transport.IdleConnTimeout != utlsConnectionIdleTimeout {
		t.Fatalf("IdleConnTimeout = %v, want %v", transport.IdleConnTimeout, utlsConnectionIdleTimeout)
	}
}

func poolActiveRequests(pool *utlsTransportPool, key utlsTransportKey) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	entry := pool.entries[key]
	if entry == nil {
		return 0
	}
	return entry.active
}

func waitForPoolActiveRequests(t *testing.T, pool *utlsTransportPool, key utlsTransportKey, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := poolActiveRequests(pool, key); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active requests = %d, want %d", poolActiveRequests(pool, key), want)
}

func roundTripWithPool(t *testing.T, pool *utlsTransportPool, key utlsTransportKey) *http.Response {
	t.Helper()
	req, errRequest := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}
	resp, errRoundTrip := (&pooledUtlsRoundTripper{pool: pool, key: key}).RoundTrip(req)
	if errRoundTrip != nil {
		t.Fatalf("round trip: %v", errRoundTrip)
	}
	return resp
}
