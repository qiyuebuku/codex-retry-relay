package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var buildVersion = "dev"

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type config struct {
	upstream     *url.URL
	upstreamIP   string
	listen       string
	maxRetries   int
	backoff      time.Duration
	requestLimit time.Duration
	maxRetryWait time.Duration
	// bufferUntilSuccess keeps an SSE response private from the client until
	// response.completed is received, allowing safe retries after late failures.
	bufferUntilSuccess bool
	// passthrough accepts any request path and forwards it unchanged, for
	// upstreams that do not serve the OpenAI-compatible API under a /v1
	// prefix (for example the ChatGPT subscription codex backend).
	passthrough bool
	// statsInterval controls the periodic metrics report; 0 disables it.
	statsInterval time.Duration
}

// ---------- periodic metrics report ----------

// statsState tracks request outcomes and prints a compact report every
// statsInterval: increments since the previous report, today's totals, and
// process-lifetime totals, plus in-flight/stuck detection.
type statsState struct {
	mu       sync.Mutex
	day      string
	inflight map[string]time.Time

	dStart, dOK, dFail, dRescue int // today
	tStart, tOK, tFail, tRescue int // process lifetime

	pdStart, pdOK, pdFail, pdRescue int // previous snapshot (daily deltas)
	ptStart, ptOK, ptFail, ptRescue int // previous snapshot (total deltas)
}

var relayStats = &statsState{inflight: make(map[string]time.Time)}

func (s *statsState) today() string { return time.Now().Format("2006/01/02") }

func (s *statsState) rollDay() {
	if s.day != s.today() {
		s.day = s.today()
		s.dStart, s.dOK, s.dFail, s.dRescue = 0, 0, 0, 0
		s.pdStart, s.pdOK, s.pdFail, s.pdRescue = 0, 0, 0, 0
	}
}

func (s *statsState) begin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	s.inflight[id] = time.Now()
	s.dStart++
	s.tStart++
}

func (s *statsState) touch(id string) {
	s.mu.Lock()
	s.inflight[id] = time.Now()
	s.mu.Unlock()
}

// finish records the outcome of a proxied request: ok counts real successes
// (a delivered failure event such as response.failed counts as a failure via
// leak), rescued marks successes that needed more than one attempt.
func (s *statsState) finish(id string, ok, leak, rescued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	delete(s.inflight, id)
	if ok && !leak {
		if rescued {
			s.dRescue++
			s.tRescue++
		}
		s.dOK++
		s.tOK++
		return
	}
	s.dFail++
	s.tFail++
}

// drop removes an abandoned request (client canceled) from in-flight
// tracking without counting it as success or failure.
func (s *statsState) drop(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

func statsPct(part, whole int) string {
	if whole <= 0 {
		return "n/a"
	}
	return strconv.FormatFloat(float64(part)*100/float64(whole), 'f', 1, 64) + "%"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *statsState) report(window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	now := time.Now()
	stuck := 0
	for _, t := range s.inflight {
		if now.Sub(t) > 10*time.Minute {
			stuck++
		}
	}
	log.Printf("[stats] 📊 %s", now.Format("2006/01/02 15:04:05"))
	log.Printf("[stats] [增量 %s] 调用 +%d | 成功 +%d | 在途 %d | 救回 +%d | 成功率 %s",
		window.Round(time.Second), maxInt(s.dStart-s.pdStart, 0), maxInt(s.dOK-s.pdOK, 0), len(s.inflight), maxInt(s.dRescue-s.pdRescue, 0),
		statsPct(maxInt(s.dOK-s.pdOK, 0), maxInt(s.dOK-s.pdOK, 0)+maxInt(s.dFail-s.pdFail, 0)))
	log.Printf("[stats] [今日] 调用 %d | 成功 %d | 失败(未救回) %d | 救回 %d | 成功率 %s（若无重试保护 %s）",
		s.dStart, s.dOK, s.dFail, s.dRescue, statsPct(s.dOK, s.dStart), statsPct(s.dOK-s.dRescue, s.dStart))
	log.Printf("[stats] [累计] 调用 %d | 成功 %d | 失败(未救回) %d | 救回 %d | 成功率 %s（若无重试保护 %s）",
		s.tStart, s.tOK, s.tFail, s.tRescue, statsPct(s.tOK, s.tStart), statsPct(s.tOK-s.tRescue, s.tStart))
	if stuck > 0 {
		log.Printf("[stats] ⚠️ 疑似卡死(>10min无进展) %d 个", stuck)
	} else {
		log.Printf("[stats] 卡死: 0")
	}
	s.pdStart, s.pdOK, s.pdFail, s.pdRescue = s.dStart, s.dOK, s.dFail, s.dRescue
	s.ptStart, s.ptOK, s.ptFail, s.ptRescue = s.tStart, s.tOK, s.tFail, s.tRescue
}

func statsLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		relayStats.report(interval)
	}
}

type proxy struct {
	cfg    config
	client *http.Client
}

var requestSequence atomic.Uint64

const maxBufferedStreamBytes int64 = 64 * 1024 * 1024

var errSSEBufferLimit = errors.New("upstream SSE response exceeded the 64 MiB buffering limit")

type streamDiagnostics struct {
	bytes        int64
	events       int
	firstByte    time.Duration
	terminal     string
	errorType    string
	errorCode    string
	errorMessage string
	errorClass   string
	lineBuffer   []byte
	eventName    string
	dataBuffer   []byte
	pending      []byte
	committed    bool
	commitEvent  string
}

var errSSEPreCommitFailure = errors.New("upstream stream failed before output commit")

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	p := &proxy{
		cfg:    cfg,
		client: newUpstreamClient(cfg),
	}

	listener, actualListen, err := listenWithFallback(cfg.listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              actualListen,
		Handler:           p,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("steady-relay version=%s listening on http://%s -> %s", buildVersion, actualListen, safeUpstreamURL(cfg.upstream))
	localAPIBaseURL := "http://" + actualListen + "/v1"
	if cfg.passthrough {
		localAPIBaseURL = "http://" + actualListen
	}
	log.Printf("[startup] local_api_base_url=%s health=http://%s/healthz passthrough=%t", localAPIBaseURL, actualListen, cfg.passthrough)
	if cfg.upstreamIP != "" {
		log.Printf("[startup] upstream_host=%s pinned_ip=%s dns_bypass=enabled", cfg.upstream.Hostname(), cfg.upstreamIP)
		if proxyURL, proxyErr := http.ProxyFromEnvironment(&http.Request{URL: cfg.upstream}); proxyErr == nil && proxyURL != nil {
			log.Printf("[startup-warning] an HTTPS proxy is configured; the upstream IP pin applies only to direct connections; configure NO_PROXY for %s if company policy permits", cfg.upstream.Hostname())
		}
	} else {
		log.Printf("[startup] upstream_host=%s pinned_ip=none dns_bypass=disabled", cfg.upstream.Hostname())
	}
	log.Printf("[startup] buffer_until_success=%t max_buffer=%dMiB", cfg.bufferUntilSuccess, maxBufferedStreamBytes/(1024*1024))
	if cfg.statsInterval > 0 {
		log.Printf("[startup] stats_interval=%s", cfg.statsInterval)
		go statsLoop(cfg.statsInterval)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newUpstreamClient(cfg config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Automatic decompression would change the upstream response body.
	transport.DisableCompression = true
	dialer := &net.Dialer{Timeout: cfg.requestLimit, KeepAlive: 30 * time.Second}
	transport.DialContext = pinnedDialContext(dialer, cfg.upstream.Hostname(), cfg.upstreamIP)
	transport.ResponseHeaderTimeout = cfg.requestLimit
	transport.TLSHandshakeTimeout = minDuration(10*time.Second, cfg.requestLimit)
	transport.ExpectContinueTimeout = minDuration(time.Second, cfg.requestLimit)
	return &http.Client{
		Transport: transport,
		// Redirects are responses too; forwarding them is more transparent
		// than silently following them inside the proxy.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// pinnedDialContext replaces only the TCP destination for the configured
// upstream host. The request URL remains unchanged, so HTTP Host, TLS SNI and
// certificate verification continue to use the upstream domain name.
func pinnedDialContext(dialer *net.Dialer, upstreamHost, upstreamIP string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, pinnedDialAddress(address, upstreamHost, upstreamIP))
	}
}

func pinnedDialAddress(address, upstreamHost, upstreamIP string) string {
	if upstreamIP == "" {
		return address
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(upstreamHost, ".")) {
		return address
	}
	return net.JoinHostPort(upstreamIP, port)
}

// safeUpstreamURL returns only the non-sensitive identifying parts of an
// upstream URL for diagnostics. It must not mutate the URL used for requests.
func safeUpstreamURL(upstream *url.URL) string {
	if upstream == nil {
		return ""
	}
	redacted := *upstream
	redacted.User = nil
	redacted.RawQuery = ""
	redacted.ForceQuery = false
	redacted.Fragment = ""
	redacted.RawFragment = ""
	return redacted.String()
}

// listenWithFallback prefers the configured port and tries subsequent ports
// on the same host when that port is already occupied.
func listenWithFallback(address string) (net.Listener, string, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, "", fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, "", fmt.Errorf("invalid listen port %q", portText)
	}
	for candidate := port; candidate <= 65535; candidate++ {
		candidateAddress := net.JoinHostPort(host, strconv.Itoa(candidate))
		listener, listenErr := net.Listen("tcp", candidateAddress)
		if listenErr == nil {
			actual := listener.Addr().String()
			if candidate != port {
				log.Printf("listen address %s is occupied; using %s", address, actual)
			}
			return listener, actual, nil
		}
		if !errors.Is(listenErr, syscall.EADDRINUSE) || port == 0 {
			return nil, "", fmt.Errorf("listen %s: %w", candidateAddress, listenErr)
		}
	}
	return nil, "", fmt.Errorf("no available TCP port from %s through 65535", address)
}

func loadConfig() (config, error) {
	upstreamDefault := strings.TrimSpace(os.Getenv("UPSTREAM_BASE_URL"))
	listenDefault := os.Getenv("LISTEN_ADDR")
	if listenDefault == "" {
		listenDefault = net.JoinHostPort(envString("LISTEN_HOST", "127.0.0.1"), envString("LISTEN_PORT", "8080"))
	}
	retriesDefault, err := envInt("MAX_RETRIES", 10)
	if err != nil {
		return config{}, err
	}
	backoffDefault, err := envDuration("RETRY_BACKOFF", 500*time.Millisecond)
	if err != nil {
		return config{}, err
	}
	timeoutDefault, err := envDuration("REQUEST_TIMEOUT", 120*time.Second)
	if err != nil {
		return config{}, err
	}
	maxRetryDefault, err := envDuration("MAX_RETRY_AFTER", 60*time.Second)
	if err != nil {
		return config{}, err
	}
	bufferUntilSuccess, err := envBool("BUFFER_UNTIL_SUCCESS", false)
	if err != nil {
		return config{}, err
	}
	passthroughDefault, err := envBool("PASSTHROUGH", false)
	if err != nil {
		return config{}, err
	}
	statsIntervalDefault, err := envDuration("STATS_INTERVAL", 3*time.Minute)
	if err != nil {
		return config{}, err
	}

	upstreamValue := flag.String("upstream", upstreamDefault, "required upstream OpenAI-compatible base URL")
	upstreamIPValue := flag.String("upstream-ip", os.Getenv("UPSTREAM_IP"), "optional fixed IP for the upstream host; omit to use system DNS")
	listenValue := flag.String("listen", listenDefault, "listen address, for example 127.0.0.1:8080")
	maxRetries := flag.Int("max-retries", retriesDefault, "number of retries after the first attempt")
	backoff := flag.Duration("retry-backoff", backoffDefault, "initial exponential retry backoff")
	timeout := flag.Duration("request-timeout", timeoutDefault, "limit for connecting and receiving upstream response headers")
	maxRetryWait := flag.Duration("max-retry-after", maxRetryDefault, "maximum Retry-After/backoff delay")
	bufferMode := flag.Bool("buffer-until-success", bufferUntilSuccess, "buffer SSE responses until response.completed before sending them to the client")
	passthroughMode := flag.Bool("passthrough", passthroughDefault, "forward any request path unchanged; use for upstreams without a /v1 prefix such as the ChatGPT subscription codex backend")
	statsInterval := flag.Duration("stats-interval", statsIntervalDefault, "periodically log a metrics report (delta/today/total); 0 disables")
	flag.Parse()

	if *maxRetries < 0 || *backoff < 0 || *timeout <= 0 || *maxRetryWait < 0 || *statsInterval < 0 {
		return config{}, errors.New("max-retries and durations must be non-negative; request-timeout must be positive")
	}
	if strings.TrimSpace(*upstreamValue) == "" {
		return config{}, errors.New("an upstream URL is required: set UPSTREAM_BASE_URL or pass --upstream https://api.example.com/v1")
	}
	upstream, err := url.Parse(*upstreamValue)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return config{}, fmt.Errorf("invalid upstream URL %q", *upstreamValue)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return config{}, fmt.Errorf("upstream URL scheme must be http or https")
	}
	upstreamIP := strings.TrimSpace(*upstreamIPValue)
	if strings.EqualFold(upstreamIP, "dns") || strings.EqualFold(upstreamIP, "auto") || strings.EqualFold(upstreamIP, "off") {
		upstreamIP = ""
	} else if upstreamIP != "" {
		parsedIP := net.ParseIP(upstreamIP)
		if parsedIP == nil {
			return config{}, fmt.Errorf("invalid upstream IP %q", upstreamIP)
		}
		upstreamIP = parsedIP.String()
	}

	return config{
		upstream:           upstream,
		upstreamIP:         upstreamIP,
		listen:             *listenValue,
		maxRetries:         *maxRetries,
		backoff:            *backoff,
		requestLimit:       *timeout,
		maxRetryWait:       *maxRetryWait,
		bufferUntilSuccess: *bufferMode,
		passthrough:        *passthroughMode,
		statsInterval:      *statsInterval,
	}, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := fmt.Sprintf("req-%06x", requestSequence.Add(1))
	startedAt := time.Now()
	logPath := r.URL.EscapedPath()
	if logPath == "" {
		logPath = "/"
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		log.Printf("[request] id=%s %s %s -> health", requestID, r.Method, logPath)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		log.Printf("[response] id=%s %s %s status=200 duration=%s", requestID, r.Method, logPath, time.Since(startedAt).Round(time.Millisecond))
		return
	}
	if !p.cfg.passthrough && r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
		log.Printf("[request] id=%s %s %s rejected status=404", requestID, r.Method, logPath)
		http.Error(w, `{"error":"path must start with /v1"}`, http.StatusNotFound)
		return
	}

	relayStats.begin(requestID)
	defer relayStats.drop(requestID) // safety net for silent returns (client cancels)

	requestHadBody := r.Body != nil && r.Body != http.NoBody
	var body []byte
	if requestHadBody {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"unable to read request body"}`, http.StatusBadRequest)
			return
		}
	}
	requestedModel := requestedModelFromBody(body)
	target := p.targetURL(r.URL)
	log.Printf("[request] id=%s method=%s path=%s model=%q forwarding", requestID, r.Method, logPath, requestedModel)

	var lastErr error
	for attempt := 0; attempt <= p.cfg.maxRetries; attempt++ {
		attemptStarted := time.Now()
		relayStats.touch(requestID)
		resp, err := p.attempt(r.Context(), r, target, body, requestHadBody)
		if err != nil {
			lastErr = err
			if r.Context().Err() != nil {
				return
			}
			if attempt < p.cfg.maxRetries {
				delay := p.retryDelay(attempt, "")
				log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d reason=%v wait=%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, err, delay.Round(time.Millisecond))
				if !sleepContext(r.Context(), delay) {
					return
				}
				continue
			}
			break
		}

		if isRetryStatus(resp.StatusCode) && attempt < p.cfg.maxRetries {
			retryAfter := resp.Header.Get("Retry-After")
			upstreamID := upstreamRequestID(resp.Header)
			failureInfo, _, _, _, _ := inspectFailureInfo(resp)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream HTTP %d", resp.StatusCode)
			delay := p.retryDelay(attempt, retryAfter)
			log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d upstream_status=%d upstream_request_id=%q header_time=%s wait=%s%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, resp.StatusCode, upstreamID, time.Since(attemptStarted).Round(time.Millisecond), delay.Round(time.Millisecond), failureInfo)
			if !sleepContext(r.Context(), delay) {
				return
			}
			continue
		}

		if (isEventStream(resp.Header.Get("Content-Type")) || bodyLooksLikeSSE(resp)) && r.Method != http.MethodHead {
			upstreamID := upstreamRequestID(resp.Header)
			diagnostics, started, streamErr := p.forwardStream(w, resp, attemptStarted, attempt < p.cfg.maxRetries, p.cfg.bufferUntilSuccess)
			if streamErr == nil {
				log.Printf("[response] id=%s method=%s path=%s model=%q status=%d attempts=%d duration=%s first_byte=%s stream=complete committed=%t commit_event=%q bytes=%d events=%d terminal=%q upstream_request_id=%q%s", requestID, r.Method, logPath, requestedModel, resp.StatusCode, attempt+1, time.Since(startedAt).Round(time.Millisecond), diagnostics.firstByte.Round(time.Millisecond), diagnostics.committed, diagnostics.commitEvent, diagnostics.bytes, diagnostics.events, diagnostics.terminal, upstreamID, diagnostics.errorLogFields())
				relayStats.finish(requestID, resp.StatusCode < 400, diagnostics.terminal == "response.failed" || diagnostics.terminal == "error", attempt > 0)
				return
			}
			lastErr = streamErr
			if started || attempt >= p.cfg.maxRetries {
				// Once headers/data have reached the client, retrying would create a
				// second response or duplicate generated tokens.
				if !started {
					p.writeUnavailable(w, lastErr)
				}
				log.Printf("[response] id=%s method=%s path=%s model=%q status=%d attempts=%d duration=%s stream=incomplete committed=%t commit_event=%q bytes=%d events=%d terminal=%q upstream_request_id=%q transport_error=%q%s", requestID, r.Method, logPath, requestedModel, resp.StatusCode, attempt+1, time.Since(startedAt).Round(time.Millisecond), diagnostics.committed, diagnostics.commitEvent, diagnostics.bytes, diagnostics.events, diagnostics.terminal, upstreamID, safeLogText(streamErr.Error(), 300), diagnostics.errorLogFields())
				// An incomplete stream whose terminal event was delivered (e.g. the
				// client hung up right after response.completed) still counts as a
				// success; only missing/failure terminals count against it.
				relayStats.finish(requestID, resp.StatusCode < 400, diagnostics.terminal == "response.failed" || diagnostics.terminal == "error", attempt > 0)
				return
			}
			if errors.Is(streamErr, errSSEPreCommitFailure) {
				// No bytes have reached the client. Discard the buffered SSE frames and
				// retry the identical request safely.
				delay := p.retryDelay(attempt, "")
				log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d sse_error_before_commit=%v wait=%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, streamErr, delay.Round(time.Millisecond))
				if !sleepContext(r.Context(), delay) {
					return
				}
				continue
			}
			delay := p.retryDelay(attempt, "")
			log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d stream_not_started error=%v wait=%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, streamErr, delay.Round(time.Millisecond))
			if !sleepContext(r.Context(), delay) {
				return
			}
			continue
		}

		if class := retryableEmbeddedError(resp); class != "" && attempt < p.cfg.maxRetries {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream HTTP %d with embedded error class=%s", resp.StatusCode, class)
			delay := p.retryDelay(attempt, resp.Header.Get("Retry-After"))
			log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d upstream_status=%d embedded_error_class=%s wait=%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, resp.StatusCode, class, delay.Round(time.Millisecond))
			if !sleepContext(r.Context(), delay) {
				return
			}
			continue
		}

		if err := p.forwardBuffered(w, r.Method, resp); err != nil {
			lastErr = err
			if attempt < p.cfg.maxRetries {
				delay := p.retryDelay(attempt, "")
				log.Printf("[retry] id=%s method=%s path=%s model=%q attempt=%d/%d response_read_error=%v wait=%s", requestID, r.Method, logPath, requestedModel, attempt+1, p.cfg.maxRetries, err, delay.Round(time.Millisecond))
				if !sleepContext(r.Context(), delay) {
					return
				}
				continue
			}
			break
		}
		log.Printf("[response] id=%s method=%s path=%s model=%q status=%d attempts=%d duration=%s header_time=%s upstream_request_id=%q", requestID, r.Method, logPath, requestedModel, resp.StatusCode, attempt+1, time.Since(startedAt).Round(time.Millisecond), time.Since(attemptStarted).Round(time.Millisecond), upstreamRequestID(resp.Header))
		relayStats.finish(requestID, resp.StatusCode < 400, false, attempt > 0)
		return
	}
	p.writeUnavailable(w, lastErr)
	log.Printf("[response] id=%s method=%s path=%s model=%q status=502 attempts=%d duration=%s error=%v", requestID, r.Method, logPath, requestedModel, p.cfg.maxRetries+1, time.Since(startedAt).Round(time.Millisecond), lastErr)
	relayStats.finish(requestID, false, false, true)
}

// requestedModelFromBody extracts only the top-level model field for diagnosis;
// it never changes the bytes forwarded upstream.
func requestedModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return safeLogText(payload.Model, 120)
}

type prefixReadCloser struct {
	io.Reader
	io.Closer
}

// inspectFailureInfo reads only a bounded prefix for classification, then
// restores those exact bytes in front of the unread body. A response that is
// not retried can therefore still be forwarded byte-for-byte.
func inspectFailureInfo(resp *http.Response) (logFields, errorType, errorCode, errorMessage string, err error) {
	original := resp.Body
	data, readErr := io.ReadAll(io.LimitReader(original, 64*1024))
	resp.Body = &prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
	if readErr != nil || len(data) == 0 {
		return "", "", "", "", readErr
	}
	var payload struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return "", "", "", "", nil
	}
	if payload.Error != nil {
		return fmt.Sprintf(" upstream_error_class=%q upstream_error_type=%q upstream_error_code=%q upstream_error_message=%q", classifyUpstreamError(payload.Error.Type, payload.Error.Code, payload.Error.Message), safeLogText(payload.Error.Type, 100), safeLogText(payload.Error.Code, 100), safeLogText(payload.Error.Message, 300)), payload.Error.Type, payload.Error.Code, payload.Error.Message, nil
	}
	if payload.Type != "" || payload.Code != "" || payload.Message != "" {
		return fmt.Sprintf(" upstream_error_class=%q upstream_error_type=%q upstream_error_code=%q upstream_error_message=%q", classifyUpstreamError(payload.Type, payload.Code, payload.Message), safeLogText(payload.Type, 100), safeLogText(payload.Code, 100), safeLogText(payload.Message, 300)), payload.Type, payload.Code, payload.Message, nil
	}
	return "", "", "", "", nil
}

// retryableEmbeddedError reports the transient error class when a successful
// HTTP 200 JSON body itself carries an upstream error payload. Some backends
// (notably the ChatGPT codex backend) wrap "model at capacity" failures this
// way instead of returning a 429/5xx status, so the status-based retry alone
// never sees them. Bytes read during inspection are restored in front of the
// body: a response that is not retried is still forwarded byte-for-byte.
func retryableEmbeddedError(resp *http.Response) string {
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	_, errorType, errorCode, errorMessage, err := inspectFailureInfo(resp)
	if err != nil || (errorType == "" && errorCode == "" && errorMessage == "") {
		return ""
	}
	switch class := classifyUpstreamError(errorType, errorCode, errorMessage); class {
	case "model_capacity", "rate_limit", "timeout":
		return class
	default:
		return ""
	}
}

func (p *proxy) attempt(ctx context.Context, original *http.Request, target *url.URL, body []byte, hadBody bool) (*http.Response, error) {
	var reader io.Reader
	if hadBody {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, original.Method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header = cloneEndToEndHeaders(original.Header)
	// Keep an explicitly empty body distinct from no body. The exact payload
	// bytes are reused unchanged for every retry.
	if hadBody {
		req.ContentLength = int64(len(body))
	}
	return p.client.Do(req)
}

func (p *proxy) targetURL(in *url.URL) *url.URL {
	target := *p.cfg.upstream
	basePath := strings.TrimSuffix(target.Path, "/")
	inEscaped := in.EscapedPath()
	if strings.HasSuffix(basePath, "/v1") && (inEscaped == "/v1" || strings.HasPrefix(inEscaped, "/v1/")) {
		inEscaped = strings.TrimPrefix(inEscaped, "/v1")
	}
	joinedEscaped := basePath + inEscaped
	decoded, err := url.PathUnescape(joinedEscaped)
	if err != nil {
		// URL paths received by net/http are validly escaped in practice; use
		// the decoded path as a safe fallback if an exotic client sends bad data.
		decoded = target.Path + in.Path
	}
	target.Path = decoded
	if target.Path == "" {
		target.Path = "/"
	}
	// RawPath preserves significant escapes such as %2F, which must not be
	// turned into a path separator by the transparent proxy.
	if escaped := target.EscapedPath(); escaped != joinedEscaped {
		target.RawPath = joinedEscaped
	} else {
		target.RawPath = ""
	}
	target.RawQuery = in.RawQuery
	target.Fragment = ""
	return &target
}

func (p *proxy) forwardBuffered(w http.ResponseWriter, method string, resp *http.Response) error {
	defer resp.Body.Close()
	var body []byte
	var err error
	if method != http.MethodHead {
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading upstream response: %w", err)
		}
	}
	copyEndToEndHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if method != http.MethodHead && len(body) != 0 {
		_, _ = w.Write(body)
	}
	return nil
}

func (p *proxy) forwardStream(w http.ResponseWriter, resp *http.Response, attemptStarted time.Time, canRetry bool, bufferUntilSuccess bool) (diagnostics streamDiagnostics, started bool, err error) {
	defer resp.Body.Close()
	buf := make([]byte, 64*1024)
	frameBuffer := make([]byte, 0, 64*1024)
	commit := func() error {
		if diagnostics.committed {
			return nil
		}
		copyEndToEndHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		started, diagnostics.committed = true, true
		if len(diagnostics.pending) > 0 {
			if _, err := w.Write(diagnostics.pending); err != nil {
				return err
			}
			flush(w)
			diagnostics.pending = nil
		}
		return nil
	}
	consume := func(chunk []byte) error {
		frameBuffer = append(frameBuffer, chunk...)
		if bufferUntilSuccess && int64(len(frameBuffer)) > maxBufferedStreamBytes {
			return errSSEBufferLimit
		}
		for {
			end, sep := sseFrameEnd(frameBuffer)
			if end < 0 {
				return nil
			}
			frame := append([]byte(nil), frameBuffer[:end+sep]...)
			frameBuffer = frameBuffer[end+sep:]
			diagnostics.observe(frame)
			if diagnostics.committed {
				if _, err := w.Write(frame); err != nil {
					return err
				}
				flush(w)
			} else {
				diagnostics.pending = append(diagnostics.pending, frame...)
				if bufferUntilSuccess && int64(len(diagnostics.pending)) > maxBufferedStreamBytes {
					return errSSEBufferLimit
				}
			}
			eventType := sseFrameType(frame)
			if (eventType == "response.failed" || eventType == "error") && !diagnostics.committed {
				if canRetry {
					return errSSEPreCommitFailure
				}
				// No retry remains. Forward the final upstream failure event exactly
				// as received so the client can show the real error to the user. In
				// buffer mode, do not release partial output from this failed attempt.
				if bufferUntilSuccess {
					diagnostics.pending = append(diagnostics.pending[:0], frame...)
				}
				diagnostics.commitEvent = eventType
				if err := commit(); err != nil {
					return err
				}
			}
			// In the opt-in mode, every frame remains buffered until the upstream
			// explicitly reports a successful terminal event. A late
			// response.failed can therefore discard the entire attempt safely.
			if (!bufferUntilSuccess && sseFrameCommitsOutput(frame)) || (bufferUntilSuccess && eventType == "response.completed") {
				diagnostics.commitEvent = eventType
				if err := commit(); err != nil {
					return err
				}
			}
		}
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if diagnostics.firstByte == 0 {
				diagnostics.firstByte = time.Since(attemptStarted)
			}
			if err := consume(buf[:n]); err != nil {
				return diagnostics, started, err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if len(frameBuffer) > 0 {
					if err := consume(frameBuffer); err != nil {
						return diagnostics, started, err
					}
				}
				diagnostics.finish()
				if !diagnostics.committed {
					if bufferUntilSuccess && diagnostics.terminal != "response.completed" {
						return diagnostics, false, errSSEPreCommitFailure
					}
					if canRetry && (bufferUntilSuccess || diagnostics.terminal == "response.failed" || diagnostics.terminal == "error") {
						return diagnostics, false, errSSEPreCommitFailure
					}
					diagnostics.commitEvent = diagnostics.terminal
					if err := commit(); err != nil {
						return diagnostics, started, err
					}
				}
				if diagnostics.terminal == "" {
					diagnostics.terminal = "unexpected_eof"
				}
				return diagnostics, started, nil
			}
			return diagnostics, started, readErr
		}
	}
}

func sseFrameEnd(data []byte) (int, int) {
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i, 2
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return i, 4
	}
	return -1, 0
}

func sseFrameType(frame []byte) string {
	for _, line := range strings.Split(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "event:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	var payload struct {
		Type string `json:"type"`
	}
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "data:") {
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &payload)
			break
		}
	}
	return payload.Type
}

func sseEventCommitsOutput(event string) bool {
	// Heartbeat/keepalive events indicate that the upstream connection is
	// still alive, but do not contain model output. Keep them buffered so a
	// subsequent pre-output response.failed/error can be retried safely. The
	// upstream has used both a bare "keepalive" event and namespaced variants
	// over time; accept the common ping/heartbeat spellings as well.
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "keepalive", "response.keepalive", "ping", "response.ping", "heartbeat", "response.heartbeat":
		return false
	}
	// These are response lifecycle/metadata events. Keep them buffered until
	// actual output appears, so a later pre-output response.failed/error can be
	// retried without exposing a partial failed response to the client.
	if event == "response.created" || event == "response.queued" || event == "response.in_progress" || event == "response.metadata" || event == "response.output_item.added" || event == "response.content_part.added" {
		return false
	}
	// Tool-argument deltas cannot be acted on by the client until the call is
	// complete anyway (a client cannot execute a partial tool call), so
	// releasing them early buys nothing. Keeping them buffered preserves the
	// safe-retry window for mid-stream response.failed/error events that the
	// ChatGPT codex backend emits while a large tool call is being generated.
	if event == "response.custom_tool_call_input.delta" || event == "response.function_call_arguments.delta" {
		return false
	}
	// Completed output items are held back for the same reason: for tool-call
	// items the client still cannot act before the response finishes (codex
	// executes tools only after response.completed), and the backend has been
	// observed failing with capacity errors right after output_item.done.
	// Responses that begin with visible text still commit on their text
	// deltas below, so perceived streaming is unchanged.
	if event == "response.output_item.done" {
		return false
	}
	if event == "" { // Unknown/data-only frames are committed conservatively.
		return true
	}
	return event != "response.failed" && event != "error"
}

// sseFrameCommitsOutput distinguishes structural SSE events from events that
// carry actual model output. In particular, Responses API sends
// response.output_item.added (and often response.content_part.added) before
// any text or tool-call arguments. Treating those announcements as output
// prevents a safe retry when the upstream then reports a capacity failure in
// the same stream. The announcements remain buffered and are discarded if a
// pre-output retry occurs.
func sseFrameCommitsOutput(frame []byte) bool {
	return sseEventCommitsOutput(sseFrameType(frame))
}

func (d *streamDiagnostics) observe(chunk []byte) {
	d.bytes += int64(len(chunk))
	d.lineBuffer = append(d.lineBuffer, chunk...)
	for {
		index := bytes.IndexByte(d.lineBuffer, '\n')
		if index < 0 {
			// Diagnostics are bounded even if an upstream sends malformed SSE.
			if len(d.lineBuffer) > 64*1024 {
				d.lineBuffer = append([]byte(nil), d.lineBuffer[len(d.lineBuffer)-64*1024:]...)
			}
			return
		}
		line := strings.TrimSuffix(string(d.lineBuffer[:index]), "\r")
		d.lineBuffer = d.lineBuffer[index+1:]
		d.observeLine(line)
	}
}

func (d *streamDiagnostics) observeLine(line string) {
	if line == "" {
		d.finishEvent()
		return
	}
	if strings.HasPrefix(line, "event:") {
		d.eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return
	}
	if strings.HasPrefix(line, "data:") && len(d.dataBuffer) < 64*1024 {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if len(d.dataBuffer) > 0 {
			d.dataBuffer = append(d.dataBuffer, '\n')
		}
		remaining := 64*1024 - len(d.dataBuffer)
		if len(data) > remaining {
			data = data[:remaining]
		}
		d.dataBuffer = append(d.dataBuffer, data...)
	}
}

func (d *streamDiagnostics) finishEvent() {
	if d.eventName == "" && len(d.dataBuffer) == 0 {
		return
	}
	d.events++
	var payload struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Response *struct {
			Status string `json:"status"`
			Error  *struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(d.dataBuffer, &payload)
	eventType := d.eventName
	if eventType == "" {
		eventType = payload.Type
	}
	if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete" || eventType == "error" {
		d.terminal = eventType
	}
	if payload.Error != nil {
		d.setError(payload.Error.Type, payload.Error.Code, payload.Error.Message)
	} else if payload.Response != nil && payload.Response.Error != nil {
		d.setError(payload.Response.Error.Type, payload.Response.Error.Code, payload.Response.Error.Message)
	} else if eventType == "error" || eventType == "response.failed" {
		d.setError(payload.Type, payload.Code, payload.Message)
	}
	d.eventName = ""
	d.dataBuffer = d.dataBuffer[:0]
}

func (d *streamDiagnostics) setError(errorType, code, message string) {
	if errorType != "" {
		d.errorType = safeLogText(errorType, 100)
	}
	if code != "" {
		d.errorCode = safeLogText(code, 100)
	}
	if message != "" {
		d.errorMessage = safeLogText(message, 300)
	}
	d.errorClass = classifyUpstreamError(errorType, code, message)
}

func (d *streamDiagnostics) finish() {
	if len(d.lineBuffer) > 0 {
		d.observeLine(strings.TrimSuffix(string(d.lineBuffer), "\r"))
		d.lineBuffer = nil
	}
	d.finishEvent()
}

func (d streamDiagnostics) errorLogFields() string {
	if d.errorType == "" && d.errorCode == "" && d.errorMessage == "" {
		return ""
	}
	return fmt.Sprintf(" upstream_error_class=%q upstream_error_type=%q upstream_error_code=%q upstream_error_message=%q", d.errorClass, d.errorType, d.errorCode, d.errorMessage)
}

func classifyUpstreamError(errorType, code, message string) string {
	text := strings.ToLower(errorType + " " + code + " " + message)
	switch {
	case strings.Contains(text, "usage_limit_reached") || strings.Contains(text, "usage limit"):
		return "usage_limit"
	case strings.Contains(text, "capacity") || strings.Contains(text, "overload"):
		return "model_capacity"
	case strings.Contains(text, "rate_limit") || strings.Contains(text, "rate limit") || strings.Contains(text, "too_many_requests"):
		return "rate_limit"
	case strings.Contains(text, "timeout") || strings.Contains(text, "timed out"):
		return "timeout"
	case strings.Contains(text, "auth") || strings.Contains(text, "unauthorized") || strings.Contains(text, "forbidden"):
		return "authentication"
	default:
		return "upstream_error"
	}
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{"X-Request-Id", "Request-Id", "Openai-Request-Id", "Cf-Ray"} {
		if value := header.Get(name); value != "" {
			return safeLogText(value, 200)
		}
	}
	return ""
}

func safeLogText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	if len(value) > limit {
		value = value[:limit] + "..."
	}
	return value
}

func flush(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (p *proxy) writeUnavailable(w http.ResponseWriter, err error) {
	detail := "request failed"
	if err != nil {
		detail = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "upstream unavailable", "detail": detail})
}

func cloneEndToEndHeaders(src http.Header) http.Header {
	dst := src.Clone()
	removeHopByHop(dst)
	return dst
}

func copyEndToEndHeaders(dst, src http.Header) {
	clean := src.Clone()
	removeHopByHop(clean)
	for key, values := range clean {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHop(header http.Header) {
	for _, connectionValue := range header.Values("Connection") {
		for _, token := range strings.Split(connectionValue, ",") {
			header.Del(strings.TrimSpace(token))
		}
	}
	for name := range hopByHopHeaders {
		header.Del(name)
	}
}

func isRetryStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// bodyLooksLikeSSE reports whether a response body uses Server-Sent Events
// framing even though the upstream labelled it with a non-SSE Content-Type
// (some backends, notably the ChatGPT codex backend, stream SSE declaring
// "text/plain"). A bounded prefix is peeked and restored in front of the
// body, so a body that is not treated as a stream is still forwarded
// byte-for-byte.
func bodyLooksLikeSSE(resp *http.Response) bool {
	if resp.Body == nil {
		return false
	}
	original := resp.Body
	data, err := io.ReadAll(io.LimitReader(original, 256))
	resp.Body = &prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
	if err != nil || len(data) == 0 {
		return false
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n\xef\xbb\xbf")
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

func (p *proxy) retryDelay(attempt int, retryAfter string) time.Duration {
	if delay, ok := parseRetryAfter(retryAfter, time.Now()); ok {
		return minDuration(delay, p.cfg.maxRetryWait)
	}
	ceiling := p.cfg.backoff
	for i := 0; i < attempt && ceiling < p.cfg.maxRetryWait; i++ {
		if ceiling > p.cfg.maxRetryWait/2 {
			ceiling = p.cfg.maxRetryWait
			break
		}
		ceiling *= 2
	}
	ceiling = minDuration(ceiling, p.cfg.maxRetryWait)
	if ceiling <= 0 {
		return 0
	}
	// Equal jitter keeps the delay in the upper half of the exponential
	// window, so later retries cannot randomly collapse back to near-zero.
	floor := ceiling / 2
	span := ceiling - floor
	return floor + time.Duration(rand.Int63n(int64(span)+1))
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return maxDuration(0, when.Sub(now)), true
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", name, value, err)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid %s=%q: use true or false", name, value)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return duration, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: use seconds or a Go duration such as 500ms", name, value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
