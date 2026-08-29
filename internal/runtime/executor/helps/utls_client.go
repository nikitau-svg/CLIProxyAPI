package helps

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const (
	utlsTransportCacheEntries = 256
	utlsTransportCacheTTL     = 10 * time.Minute
	utlsConnectionIdleTimeout = 90 * time.Second
)

// closeIdleRoundTripper is implemented by transports that can retire their
// idle connections without interrupting in-flight requests.
type closeIdleRoundTripper interface {
	http.RoundTripper
	CloseIdleConnections()
}

// utlsTransportKey prevents HTTP/2 connections and HPACK state from crossing
// auth, provider, or egress boundaries. The destination host is partitioned by
// the HTTP/2 transport itself.
type utlsTransportKey struct {
	provider     string
	authIdentity string
	egress       string
}

type utlsTransportEntry struct {
	transport closeIdleRoundTripper
	active    int
	lastIdle  time.Time
	lru       uint64
}

type utlsTransportFactory func(utlsTransportKey) closeIdleRoundTripper

// utlsTransportPool caches a bounded number of idle transports. Active
// identities may temporarily exceed maxIdleEntries rather than forcing one
// project to wait for an unrelated auth identity. The excess is trimmed as
// soon as a transport becomes idle.
type utlsTransportPool struct {
	mu             sync.Mutex
	entries        map[utlsTransportKey]*utlsTransportEntry
	maxIdleEntries int
	idleTTL        time.Duration
	factory        utlsTransportFactory
	now            func() time.Time
	lru            uint64
	cleanupTimer   *time.Timer
	cleanupAt      time.Time
	cleanupGen     uint64
}

func newUtlsTransportPool(maxIdleEntries int, idleTTL time.Duration, factory utlsTransportFactory) *utlsTransportPool {
	if maxIdleEntries < 1 {
		maxIdleEntries = 1
	}
	return &utlsTransportPool{
		entries:        make(map[utlsTransportKey]*utlsTransportEntry),
		maxIdleEntries: maxIdleEntries,
		idleTTL:        idleTTL,
		factory:        factory,
		now:            time.Now,
	}
}

var sharedUtlsTransportPool = newUtlsTransportPool(
	utlsTransportCacheEntries,
	utlsTransportCacheTTL,
	func(key utlsTransportKey) closeIdleRoundTripper {
		return newUtlsTransportBundle(key.egress)
	},
)

func (p *utlsTransportPool) acquire(key utlsTransportKey) *utlsTransportEntry {
	now := p.now()
	p.mu.Lock()
	toClose := p.removeExpiredLocked(now)
	entry := p.entries[key]
	if entry == nil {
		toClose = append(toClose, p.makeIdleRoomLocked()...)
		entry = &utlsTransportEntry{transport: p.factory(key)}
		p.entries[key] = entry
	}
	p.lru++
	entry.lru = p.lru
	entry.active++
	p.scheduleCleanupLocked(now)
	p.mu.Unlock()
	closeIdleTransports(toClose)
	return entry
}

func (p *utlsTransportPool) release(key utlsTransportKey, entry *utlsTransportEntry) {
	now := p.now()
	p.mu.Lock()
	if current := p.entries[key]; current == entry {
		if entry.active > 0 {
			entry.active--
		}
		if entry.active == 0 {
			p.lru++
			entry.lru = p.lru
			entry.lastIdle = now
		}
	}
	toClose := p.trimOverflowLocked()
	p.scheduleCleanupLocked(now)
	p.mu.Unlock()
	closeIdleTransports(toClose)
}

func (p *utlsTransportPool) makeIdleRoomLocked() []closeIdleRoundTripper {
	var toClose []closeIdleRoundTripper
	for len(p.entries) >= p.maxIdleEntries {
		key, entry, ok := p.oldestIdleLocked()
		if !ok {
			break
		}
		delete(p.entries, key)
		toClose = append(toClose, entry.transport)
	}
	return toClose
}

func (p *utlsTransportPool) trimOverflowLocked() []closeIdleRoundTripper {
	var toClose []closeIdleRoundTripper
	for len(p.entries) > p.maxIdleEntries {
		key, entry, ok := p.oldestIdleLocked()
		if !ok {
			break
		}
		delete(p.entries, key)
		toClose = append(toClose, entry.transport)
	}
	return toClose
}

func (p *utlsTransportPool) oldestIdleLocked() (utlsTransportKey, *utlsTransportEntry, bool) {
	var oldestKey utlsTransportKey
	var oldest *utlsTransportEntry
	for key, entry := range p.entries {
		if entry.active != 0 {
			continue
		}
		if oldest == nil || entry.lru < oldest.lru {
			oldestKey = key
			oldest = entry
		}
	}
	return oldestKey, oldest, oldest != nil
}

func (p *utlsTransportPool) removeExpiredLocked(now time.Time) []closeIdleRoundTripper {
	if p.idleTTL <= 0 {
		return nil
	}
	var toClose []closeIdleRoundTripper
	for key, entry := range p.entries {
		if entry.active != 0 || entry.lastIdle.IsZero() || now.Sub(entry.lastIdle) < p.idleTTL {
			continue
		}
		delete(p.entries, key)
		toClose = append(toClose, entry.transport)
	}
	return toClose
}

func (p *utlsTransportPool) scheduleCleanupLocked(now time.Time) {
	if p.idleTTL <= 0 {
		return
	}
	var deadline time.Time
	for _, entry := range p.entries {
		if entry.active != 0 || entry.lastIdle.IsZero() {
			continue
		}
		candidate := entry.lastIdle.Add(p.idleTTL)
		if deadline.IsZero() || candidate.Before(deadline) {
			deadline = candidate
		}
	}
	// Keep an already scheduled earlier cleanup. If its original entry became
	// active or its deadline moved, that inexpensive callback will recompute
	// the next deadline. This avoids allocating two timers per request.
	if p.cleanupTimer != nil && (deadline.IsZero() || !deadline.Before(p.cleanupAt)) {
		return
	}
	if p.cleanupTimer != nil {
		p.cleanupTimer.Stop()
		p.cleanupTimer = nil
		p.cleanupAt = time.Time{}
	}
	if deadline.IsZero() {
		return
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}
	p.cleanupGen++
	generation := p.cleanupGen
	p.cleanupAt = deadline
	p.cleanupTimer = time.AfterFunc(delay, func() {
		p.cleanupExpired(generation)
	})
}

func (p *utlsTransportPool) cleanupExpired(generation uint64) {
	now := p.now()
	p.mu.Lock()
	if generation != p.cleanupGen {
		p.mu.Unlock()
		return
	}
	p.cleanupTimer = nil
	p.cleanupAt = time.Time{}
	p.cleanupGen++
	toClose := p.removeExpiredLocked(now)
	p.scheduleCleanupLocked(now)
	p.mu.Unlock()
	closeIdleTransports(toClose)
}

// evictExpired is kept separate from the timer callback so idle eviction can
// be verified deterministically without relying on scheduler timing.
func (p *utlsTransportPool) evictExpired() {
	now := p.now()
	p.mu.Lock()
	p.cleanupGen++
	if p.cleanupTimer != nil {
		p.cleanupTimer.Stop()
		p.cleanupTimer = nil
		p.cleanupAt = time.Time{}
	}
	toClose := p.removeExpiredLocked(now)
	p.scheduleCleanupLocked(now)
	p.mu.Unlock()
	closeIdleTransports(toClose)
}

func closeIdleTransports(transports []closeIdleRoundTripper) {
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

type pooledUtlsRoundTripper struct {
	pool *utlsTransportPool
	key  utlsTransportKey
}

func (t *pooledUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	entry := t.pool.acquire(t.key)
	resp, errRoundTrip := entry.transport.RoundTrip(req)
	if errRoundTrip != nil {
		t.pool.release(t.key, entry)
		return resp, errRoundTrip
	}
	if resp == nil || resp.Body == nil {
		t.pool.release(t.key, entry)
		return resp, nil
	}
	body := &pooledUtlsResponseBody{
		ReadCloser: resp.Body,
		finished:   make(chan struct{}),
		release: func() {
			t.pool.release(t.key, entry)
		},
	}
	resp.Body = body
	if ctxDone := req.Context().Done(); ctxDone != nil {
		// Request cancellation makes the HTTP/2 transport abort its stream. We
		// only release pool accounting here; eviction uses CloseIdleConnections,
		// which never interrupts a stream still considered active by HTTP/2.
		go func() {
			select {
			case <-ctxDone:
				body.finish()
			case <-body.finished:
			}
		}()
	}
	return resp, nil
}

type pooledUtlsResponseBody struct {
	io.ReadCloser
	once     sync.Once
	finished chan struct{}
	release  func()
}

func (b *pooledUtlsResponseBody) Read(p []byte) (int, error) {
	n, errRead := b.ReadCloser.Read(p)
	if errRead != nil {
		b.finish()
	}
	return n, errRead
}

func (b *pooledUtlsResponseBody) Close() error {
	errClose := b.ReadCloser.Close()
	b.finish()
	return errClose
}

func (b *pooledUtlsResponseBody) finish() {
	b.once.Do(func() {
		close(b.finished)
		b.release()
	})
}

// utlsTransportBundle keeps both protected-host HTTP/2 and fallback
// connections within the same auth/egress isolation boundary.
type utlsTransportBundle struct {
	utls     *http2.Transport
	fallback http.RoundTripper
}

type failingUtlsTransport struct {
	err error
}

func (t *failingUtlsTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func (t *failingUtlsTransport) CloseIdleConnections() {}

func newUtlsTransportBundle(proxyURL string) closeIdleRoundTripper {
	h2Transport, errH2Transport := newUtlsHTTP2Transport(proxyURL)
	if errH2Transport != nil {
		return &failingUtlsTransport{err: fmt.Errorf("utls proxy configuration rejected: %w", errH2Transport)}
	}
	fallback := defaultIsolatedTransport()
	if proxyURL != "" {
		transport, mode, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
		if errTransport != nil {
			return &failingUtlsTransport{err: fmt.Errorf("fallback proxy configuration rejected: %w", errTransport)}
		}
		if mode != proxyutil.ModeInherit && transport == nil {
			return &failingUtlsTransport{err: errors.New("fallback explicit proxy did not provide a transport")}
		}
		if transport != nil {
			fallback = transport
		}
	}
	return &utlsTransportBundle{
		utls:     h2Transport,
		fallback: fallback,
	}
}

func defaultIsolatedTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

func newUtlsHTTP2Transport(proxyURL string) (*http2.Transport, error) {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			return nil, errBuild
		}
		if mode != proxyutil.ModeInherit && proxyDialer == nil {
			return nil, errors.New("explicit proxy did not provide a dialer")
		}
		if mode != proxyutil.ModeInherit {
			dialer = proxyDialer
		}
	}

	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, tlsConfig *cryptotls.Config) (net.Conn, error) {
			conn, errDial := dialUtlsContext(ctx, dialer, network, addr)
			if errDial != nil {
				return nil, errDial
			}

			var serverName string
			if tlsConfig != nil {
				serverName = strings.TrimSpace(tlsConfig.ServerName)
			}
			if serverName == "" {
				serverName = addr
				if host, _, errSplit := net.SplitHostPort(addr); errSplit == nil {
					serverName = host
				}
			}
			tlsConn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloChrome_Auto)
			if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
				if errClose := conn.Close(); errClose != nil {
					return nil, fmt.Errorf("utls handshake failed: %w; close failed: %v", errHandshake, errClose)
				}
				return nil, fmt.Errorf("utls handshake failed: %w", errHandshake)
			}
			if negotiated := tlsConn.ConnectionState().NegotiatedProtocol; negotiated != http2.NextProtoTLS {
				if errClose := tlsConn.Close(); errClose != nil {
					return nil, fmt.Errorf("utls negotiated ALPN %q instead of h2; close failed: %v", negotiated, errClose)
				}
				return nil, fmt.Errorf("utls negotiated ALPN %q instead of h2", negotiated)
			}
			return tlsConn, nil
		},
		// Keep StrictMaxConcurrentStreams disabled: bursts may open multiple
		// managed connections instead of introducing a hidden per-auth queue.
		StrictMaxConcurrentStreams: false,
		IdleConnTimeout:            utlsConnectionIdleTimeout,
	}, nil
}

func dialUtlsContext(ctx context.Context, dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, network, addr)
	}
	if ctx == nil || ctx.Done() == nil {
		return dialer.Dial(network, addr)
	}
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult)
	go func() {
		conn, errDial := dialer.Dial(network, addr)
		select {
		case result <- dialResult{conn: conn, err: errDial}:
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case dialed := <-result:
		if errContext := ctx.Err(); errContext != nil {
			if dialed.conn != nil {
				_ = dialed.conn.Close()
			}
			return nil, errContext
		}
		return dialed.conn, dialed.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *utlsTransportBundle) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := utlsProtectedHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return t.utls.RoundTrip(req)
		}
	}
	return t.fallback.RoundTrip(req)
}

func (t *utlsTransportBundle) CloseIdleConnections() {
	t.utls.CloseIdleConnections()
	if transport, ok := t.fallback.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// utlsProtectedHosts contains the hosts that should use utls Chrome TLS fingerprint
// to bypass Cloudflare's TLS fingerprinting.
var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

func newUtlsTransportKey(auth *cliproxyauth.Auth, proxyURL string) utlsTransportKey {
	key := utlsTransportKey{egress: strings.TrimSpace(proxyURL)}
	if auth == nil {
		key.authIdentity = "anonymous"
		return key
	}
	key.provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	switch {
	case strings.TrimSpace(auth.ID) != "":
		key.authIdentity = "id:" + strings.TrimSpace(auth.ID)
	case strings.TrimSpace(auth.Index) != "":
		key.authIdentity = "index:" + strings.TrimSpace(auth.Index)
	case strings.TrimSpace(auth.FileName) != "":
		key.authIdentity = "file:" + strings.TrimSpace(auth.FileName)
	default:
		// Runtime pointer identity is a last-resort boundary for transient test
		// or plugin auth records that have not received a stable ID yet.
		key.authIdentity = fmt.Sprintf("runtime:%p", auth)
	}
	return key
}

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for provider requests that need a Chrome-like TLS fingerprint.
// The shared transport cache is partitioned by auth, provider, and egress so
// bearer credentials and HTTP/2 compression state cannot cross identities.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var transport http.RoundTripper
	if proxyURL == "" && ctx != nil {
		if contextTransport, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && contextTransport != nil {
			// A context transport is request-scoped and may be a test double or a
			// caller-owned egress. It must never enter the shared pool.
			transport = &contextUtlsRoundTripper{transport: contextTransport}
		}
	}
	if transport == nil {
		if auth == nil {
			// Without a stable auth identity, fail closed on connection sharing.
			// The per-client HTTP/2 transport still retires its own idle conns.
			transport = newUtlsTransportBundle(proxyURL)
		} else {
			transport = &pooledUtlsRoundTripper{
				pool: sharedUtlsTransportPool,
				key:  newUtlsTransportKey(auth, proxyURL),
			}
		}
	}

	client := &http.Client{Transport: transport}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

// contextUtlsRoundTripper preserves the existing request-scoped override for
// both protected and fallback hosts without caching it globally.
type contextUtlsRoundTripper struct {
	transport http.RoundTripper
}

func (t *contextUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(req)
}
