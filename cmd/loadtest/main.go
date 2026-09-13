package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type options struct {
	baseURL          string
	users            int
	rate             int
	duration         time.Duration
	warmup           time.Duration
	grace            time.Duration
	messageBytes     int
	setupConcurrency int
	requestTimeout   time.Duration
	output           string
}

type testUser struct {
	id    uint
	token string
	conn  *websocket.Conn
}

type apiResponse[T any] struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
}

type loginData struct {
	Token    string `json:"token"`
	UserInfo struct {
		ID uint `json:"ID"`
	} `json:"userInfo"`
}

type receivedEnvelope struct {
	Type string `json:"type"`
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

type pendingMessage struct {
	sentAt time.Time
}

type metrics struct {
	connected       atomic.Int64
	active          atomic.Int64
	unexpectedClose atomic.Int64
	sent            atomic.Int64
	accepted        atomic.Int64
	httpFailed      atomic.Int64
	received        atomic.Int64

	pending sync.Map
	mu      sync.Mutex
	e2e     []time.Duration
	http    []time.Duration
	connect []time.Duration
}

type result struct {
	StartedAt             time.Time `json:"started_at"`
	BaseURL               string    `json:"base_url"`
	GoVersion             string    `json:"go_version"`
	OS                    string    `json:"os"`
	Architecture          string    `json:"architecture"`
	LogicalCPUs           int       `json:"logical_cpus"`
	ConfiguredUsers       int       `json:"configured_users"`
	ConnectedUsers        int64     `json:"connected_users"`
	ActiveAtEnd           int64     `json:"active_at_end"`
	UnexpectedDisconnects int64     `json:"unexpected_disconnects"`
	TargetRate            int       `json:"target_messages_per_second"`
	DurationSeconds       float64   `json:"duration_seconds"`
	Sent                  int64     `json:"sent"`
	Accepted              int64     `json:"accepted"`
	HTTPFailed            int64     `json:"http_failed"`
	Received              int64     `json:"received"`
	DeliveryRate          float64   `json:"delivery_rate_percent"`
	ActualThroughput      float64   `json:"actual_messages_per_second"`
	ConnectP95MS          float64   `json:"connect_p95_ms"`
	HTTPP50MS             float64   `json:"http_p50_ms"`
	HTTPP95MS             float64   `json:"http_p95_ms"`
	HTTPP99MS             float64   `json:"http_p99_ms"`
	E2EP50MS              float64   `json:"e2e_p50_ms"`
	E2EP95MS              float64   `json:"e2e_p95_ms"`
	E2EP99MS              float64   `json:"e2e_p99_ms"`
	E2EMaxMS              float64   `json:"e2e_max_ms"`
}

func main() {
	opt := parseOptions()
	if opt.users < 2 {
		fatalf("users must be at least 2")
	}
	if opt.rate < 0 {
		fatalf("rate cannot be negative")
	}
	if opt.setupConcurrency < 1 {
		fatalf("setup-concurrency must be at least 1")
	}
	if opt.messageBytes < 1 {
		fatalf("message-bytes must be at least 1")
	}
	if opt.duration <= 0 {
		fatalf("duration must be positive")
	}

	startedAt := time.Now()
	httpClient := &http.Client{
		Timeout: opt.requestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        opt.setupConcurrency * 4,
			MaxIdleConnsPerHost: opt.setupConcurrency * 4,
			MaxConnsPerHost:     opt.setupConcurrency * 4,
		},
	}

	fmt.Printf("Preparing %d users...\n", opt.users)
	users, err := prepareUsers(httpClient, opt)
	if err != nil {
		fatalf("prepare users: %v", err)
	}

	var stats metrics
	var closing atomic.Bool
	fmt.Printf("Opening %d WebSocket connections...\n", len(users))
	if err := connectUsers(users, opt, &stats, &closing); err != nil {
		closeConnections(users, &closing)
		fatalf("connect users: %v", err)
	}

	fmt.Printf("Connected: %d/%d; warming up for %s...\n", stats.connected.Load(), opt.users, opt.warmup)
	time.Sleep(opt.warmup)

	if opt.rate == 0 {
		fmt.Printf("Holding connections for %s without sending messages...\n", opt.duration)
		time.Sleep(opt.duration)
	} else {
		fmt.Printf("Sending at %d msg/s for %s...\n", opt.rate, opt.duration)
		runMessageLoad(httpClient, users, opt, &stats)
		fmt.Printf("Waiting %s for in-flight delivery...\n", opt.grace)
		time.Sleep(opt.grace)
	}
	activeAtEnd := stats.active.Load()

	closeConnections(users, &closing)
	report := buildResult(startedAt, opt.duration, activeAtEnd, opt, &stats)
	printResult(report)
	if opt.output != "" {
		if err := writeResult(opt.output, report); err != nil {
			fatalf("write result: %v", err)
		}
		fmt.Printf("Result written to %s\n", opt.output)
	}
}

func parseOptions() options {
	var opt options
	flag.StringVar(&opt.baseURL, "base-url", "http://127.0.0.1:8080", "iChat HTTP base URL")
	flag.IntVar(&opt.users, "users", 100, "number of authenticated WebSocket users")
	flag.IntVar(&opt.rate, "rate", 100, "target messages per second; 0 tests connections only")
	flag.DurationVar(&opt.duration, "duration", 5*time.Minute, "measurement duration")
	flag.DurationVar(&opt.warmup, "warmup", 15*time.Second, "warm-up time after connecting")
	flag.DurationVar(&opt.grace, "grace", 10*time.Second, "delivery wait after sending stops")
	flag.IntVar(&opt.messageBytes, "message-bytes", 256, "message content size in ASCII bytes")
	flag.IntVar(&opt.setupConcurrency, "setup-concurrency", 20, "registration/login/connection concurrency")
	flag.DurationVar(&opt.requestTimeout, "request-timeout", 10*time.Second, "HTTP request timeout")
	flag.StringVar(&opt.output, "out", "", "optional JSON result path")
	flag.Parse()
	opt.baseURL = strings.TrimRight(opt.baseURL, "/")
	return opt
}

func prepareUsers(client *http.Client, opt options) ([]testUser, error) {
	users := make([]testUser, opt.users)
	runID := fmt.Sprintf("%x", time.Now().UnixNano())
	jobs := make(chan int)
	errCh := make(chan error, opt.users)
	var wg sync.WaitGroup

	workers := opt.setupConcurrency
	if workers > opt.users {
		workers = opt.users
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				phone := fmt.Sprintf("lt%s%05d", runID, index)
				password := "loadtest-only"
				registerBody := map[string]any{
					"phone":      phone,
					"name":       fmt.Sprintf("loadtest-%d", index),
					"password":   password,
					"confirmPsw": password,
				}
				var registerResponse apiResponse[json.RawMessage]
				if err := postJSON(client, opt.baseURL+"/user/register", "", registerBody, &registerResponse); err != nil {
					errCh <- fmt.Errorf("register user %d: %w", index, err)
					continue
				}
				if registerResponse.Code != 0 {
					errCh <- fmt.Errorf("register user %d: %s", index, registerResponse.Msg)
					continue
				}

				loginBody := map[string]any{"account": phone, "password": password}
				var loginResponse apiResponse[loginData]
				if err := postJSON(client, opt.baseURL+"/user/login", "", loginBody, &loginResponse); err != nil {
					errCh <- fmt.Errorf("login user %d: %w", index, err)
					continue
				}
				if loginResponse.Code != 0 {
					errCh <- fmt.Errorf("login user %d: %s", index, loginResponse.Msg)
					continue
				}
				users[index] = testUser{id: loginResponse.Data.UserInfo.ID, token: loginResponse.Data.Token}
			}
		}()
	}

	for index := range users {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return nil, err
	}
	return users, nil
}

func postJSON(client *http.Client, endpoint, token string, body any, destination any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return err
	}
	return nil
}

func connectUsers(users []testUser, opt options, stats *metrics, closing *atomic.Bool) error {
	websocketURL, err := url.Parse(opt.baseURL)
	if err != nil {
		return err
	}
	if websocketURL.Scheme == "https" {
		websocketURL.Scheme = "wss"
	} else {
		websocketURL.Scheme = "ws"
	}
	websocketURL.Path = "/auth/chat"

	jobs := make(chan int)
	errCh := make(chan error, len(users))
	var wg sync.WaitGroup
	workers := opt.setupConcurrency
	if workers > len(users) {
		workers = len(users)
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				userURL := *websocketURL
				query := userURL.Query()
				query.Set("token", users[index].token)
				userURL.RawQuery = query.Encode()

				started := time.Now()
				conn, response, err := websocket.DefaultDialer.Dial(userURL.String(), nil)
				stats.addConnectLatency(time.Since(started))
				if err != nil {
					if response != nil {
						errCh <- fmt.Errorf("WebSocket user %d: HTTP %d: %w", index, response.StatusCode, err)
					} else {
						errCh <- fmt.Errorf("WebSocket user %d: %w", index, err)
					}
					continue
				}
				users[index].conn = conn
				stats.connected.Add(1)
				stats.active.Add(1)
				go readWebsocket(conn, stats, closing)
			}
		}()
	}

	for index := range users {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return err
	}
	return nil
}

func readWebsocket(conn *websocket.Conn, stats *metrics, closing *atomic.Bool) {
	defer stats.active.Add(-1)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if !closing.Load() {
				stats.unexpectedClose.Add(1)
			}
			return
		}
		var envelope receivedEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			continue
		}
		if envelope.Type != "simple" && envelope.Type != "group" {
			continue
		}
		pending, ok := stats.pending.LoadAndDelete(envelope.Data.ID)
		if !ok {
			continue
		}
		stats.received.Add(1)
		stats.addE2ELatency(time.Since(pending.(pendingMessage).sentAt))
	}
}

func closeConnections(users []testUser, closing *atomic.Bool) {
	closing.Store(true)
	for index := range users {
		if users[index].conn != nil {
			_ = users[index].conn.Close()
		}
	}
}

func runMessageLoad(client *http.Client, users []testUser, opt options, stats *metrics) {
	interval := time.Second / time.Duration(opt.rate)
	if interval <= 0 {
		fatalf("rate is too high")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(opt.duration)
	defer deadline.Stop()

	content := strings.Repeat("x", opt.messageBytes)
	semaphore := make(chan struct{}, opt.setupConcurrency*8)
	var sends sync.WaitGroup
	var sequence atomic.Uint64

	for {
		select {
		case <-deadline.C:
			sends.Wait()
			return
		case <-ticker.C:
			current := sequence.Add(1)
			senderIndex := int((current - 1) % uint64(len(users)))
			receiverIndex := (senderIndex + 1) % len(users)
			semaphore <- struct{}{}
			sentAt := time.Now()
			messageID := fmt.Sprintf("load-%d-%d", sentAt.UnixNano(), current)
			message := map[string]any{
				"id":          messageID,
				"identifier":  current,
				"content":     content,
				"sendTime":    sentAt.UnixMilli(),
				"toContactId": users[receiverIndex].id,
				"type":        "text",
				"fromUser": map[string]any{
					"id": users[senderIndex].id,
				},
				"is_group": 0,
			}

			sends.Add(1)
			stats.sent.Add(1)
			stats.pending.Store(messageID, pendingMessage{sentAt: sentAt})
			go func(sender testUser, id string, body map[string]any) {
				defer sends.Done()
				defer func() { <-semaphore }()
				started := time.Now()
				var response apiResponse[json.RawMessage]
				err := postJSON(client, opt.baseURL+"/auth/sendMessage", sender.token, body, &response)
				if err != nil || response.Code != 0 {
					stats.pending.Delete(id)
					stats.httpFailed.Add(1)
					return
				}
				stats.accepted.Add(1)
				stats.addHTTPLatency(time.Since(started))
			}(users[senderIndex], messageID, message)
		}
	}
}

func (stats *metrics) addE2ELatency(value time.Duration) {
	stats.mu.Lock()
	stats.e2e = append(stats.e2e, value)
	stats.mu.Unlock()
}

func (stats *metrics) addHTTPLatency(value time.Duration) {
	stats.mu.Lock()
	stats.http = append(stats.http, value)
	stats.mu.Unlock()
}

func (stats *metrics) addConnectLatency(value time.Duration) {
	stats.mu.Lock()
	stats.connect = append(stats.connect, value)
	stats.mu.Unlock()
}

func buildResult(startedAt time.Time, duration time.Duration, activeAtEnd int64, opt options, stats *metrics) result {
	stats.mu.Lock()
	e2e := append([]time.Duration(nil), stats.e2e...)
	httpLatencies := append([]time.Duration(nil), stats.http...)
	connectLatencies := append([]time.Duration(nil), stats.connect...)
	stats.mu.Unlock()

	accepted := stats.accepted.Load()
	received := stats.received.Load()
	deliveryRate := 0.0
	if accepted > 0 {
		deliveryRate = float64(received) / float64(accepted) * 100
	}
	throughput := 0.0
	if duration > 0 {
		throughput = float64(received) / duration.Seconds()
	}

	return result{
		StartedAt:             startedAt,
		BaseURL:               opt.baseURL,
		GoVersion:             runtime.Version(),
		OS:                    runtime.GOOS,
		Architecture:          runtime.GOARCH,
		LogicalCPUs:           runtime.NumCPU(),
		ConfiguredUsers:       opt.users,
		ConnectedUsers:        stats.connected.Load(),
		ActiveAtEnd:           activeAtEnd,
		UnexpectedDisconnects: stats.unexpectedClose.Load(),
		TargetRate:            opt.rate,
		DurationSeconds:       duration.Seconds(),
		Sent:                  stats.sent.Load(),
		Accepted:              accepted,
		HTTPFailed:            stats.httpFailed.Load(),
		Received:              received,
		DeliveryRate:          deliveryRate,
		ActualThroughput:      throughput,
		ConnectP95MS:          percentileMilliseconds(connectLatencies, 0.95),
		HTTPP50MS:             percentileMilliseconds(httpLatencies, 0.50),
		HTTPP95MS:             percentileMilliseconds(httpLatencies, 0.95),
		HTTPP99MS:             percentileMilliseconds(httpLatencies, 0.99),
		E2EP50MS:              percentileMilliseconds(e2e, 0.50),
		E2EP95MS:              percentileMilliseconds(e2e, 0.95),
		E2EP99MS:              percentileMilliseconds(e2e, 0.99),
		E2EMaxMS:              percentileMilliseconds(e2e, 1),
	}
}

func percentileMilliseconds(values []time.Duration, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(float64(len(values)-1) * percentile)
	return float64(values[index].Microseconds()) / 1000
}

func printResult(report result) {
	fmt.Println()
	fmt.Println("=== iChat load test result ===")
	fmt.Printf("Connections: %d/%d, active at end: %d, unexpected disconnects: %d\n",
		report.ConnectedUsers, report.ConfiguredUsers, report.ActiveAtEnd, report.UnexpectedDisconnects)
	fmt.Printf("Messages: sent=%d accepted=%d received=%d HTTP-failed=%d delivery=%.2f%%\n",
		report.Sent, report.Accepted, report.Received, report.HTTPFailed, report.DeliveryRate)
	fmt.Printf("Throughput: %.2f msg/s (target %d msg/s)\n", report.ActualThroughput, report.TargetRate)
	fmt.Printf("Connect P95: %.2f ms\n", report.ConnectP95MS)
	fmt.Printf("HTTP latency: P50 %.2f ms, P95 %.2f ms, P99 %.2f ms\n",
		report.HTTPP50MS, report.HTTPP95MS, report.HTTPP99MS)
	fmt.Printf("End-to-end latency: P50 %.2f ms, P95 %.2f ms, P99 %.2f ms, max %.2f ms\n",
		report.E2EP50MS, report.E2EP95MS, report.E2EP99MS, report.E2EMaxMS)
}

func writeResult(path string, report result) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(1)
}
