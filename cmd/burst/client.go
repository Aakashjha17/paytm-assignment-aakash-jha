package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type client struct {
	base string
	http *http.Client
}

func newClient(base string, concurrency int, timeout time.Duration) *client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true, // multiplex over few TLS connections to a live edge
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{Transport: tr, Timeout: timeout}}
}

type response struct {
	Status int
	Body   map[string]any
	Raw    []byte
	Err    error // transport failure: no HTTP response at all
	Dur    time.Duration
}

func (r response) str(k string) string { s, _ := r.Body[k].(string); return s }

func (r response) errCode() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (c *client) do(ctx context.Context, method, path string, headers map[string]string, body any) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return response{Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		return response{Err: err, Dur: time.Since(start)}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode, Raw: raw, Dur: time.Since(start)}
	if err != nil {
		out.Err = err
		return out
	}
	if len(raw) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &out.Body)
	}
	return out
}

func (c *client) reserve(ctx context.Context, token string, show int64, key string, seats []string, extra map[string]any) response {
	body := map[string]any{"seats": seats}
	for k, v := range extra {
		body[k] = v
	}
	h := map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": key}
	return c.do(ctx, "POST", fmt.Sprintf("/shows/%d/reservations", show), h, body)
}

func (c *client) cancel(ctx context.Context, token, id string) response {
	return c.do(ctx, "DELETE", "/reservations/"+id, map[string]string{"Authorization": "Bearer " + token}, nil)
}

type counts struct{ Available, Held, Confirmed, Total, Listed int }

func (c *client) showCounts(ctx context.Context, show int64) (counts, error) {
	r := c.do(ctx, "GET", fmt.Sprintf("/shows/%d", show), nil, nil)
	if r.Err != nil {
		return counts{}, r.Err
	}
	if r.Status != 200 {
		return counts{}, fmt.Errorf("GET /shows/%d: %d %s", show, r.Status, r.Raw)
	}
	m, _ := r.Body["counts"].(map[string]any)
	seats, _ := r.Body["seats"].([]any)
	num := func(v any) int { f, _ := v.(float64); return int(f) }
	return counts{
		Available: num(m["available"]), Held: num(m["held"]), Confirmed: num(m["confirmed"]),
		Total: num(r.Body["total_seats"]), Listed: len(seats),
	}, nil
}

// metrics returns every /metrics sample keyed by its series text, e.g.
// `seats_held{show_id="3"}`.
func (c *client) metrics(ctx context.Context) (map[string]float64, error) {
	r := c.do(ctx, "GET", "/metrics", nil, nil)
	if r.Err != nil {
		return nil, r.Err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("GET /metrics: %d", r.Status)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(r.Raw), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		out[line[:i]] = v
	}
	return out, nil
}

func (c *client) waitReady(ctx context.Context, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		r := c.do(ctx, "GET", "/readyz", nil, nil)
		if r.Err == nil && r.Status == 200 {
			return nil
		}
		if time.Now().After(deadline) {
			if r.Err != nil {
				return fmt.Errorf("not ready after %v: %v", max, r.Err)
			}
			return fmt.Errorf("not ready after %v: /readyz %d %s", max, r.Status, r.Raw)
		}
		time.Sleep(time.Second)
	}
}
