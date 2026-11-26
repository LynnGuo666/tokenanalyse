package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/storage"
)

type ServerClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewServerClient(baseURL, token string) *ServerClient {
	return &ServerClient{baseURL: strings.TrimRight(baseURL, "/"), token: token,
		client: &http.Client{Timeout: 45 * time.Second}}
}

func (c *ServerClient) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("server HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (c *ServerClient) PushBatch(ctx context.Context, batch EventBatch) (BatchAck, error) {
	var ack BatchAck
	err := c.request(ctx, http.MethodPost, "/api/v1/events/batch", batch, &ack)
	return ack, err
}

func (c *ServerClient) Heartbeat(ctx context.Context, heartbeat model.DeviceHeartbeat) error {
	return c.request(ctx, http.MethodPost, "/api/v1/devices/heartbeat", HeartbeatRequest{Device: heartbeat}, nil)
}

func (c *ServerClient) FetchEvents(ctx context.Context, cursor string, limit int) (EventsResponse, error) {
	var result EventsResponse
	path := fmt.Sprintf("/api/v1/events?cursor=%s&limit=%d", cursor, limit)
	err := c.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *ServerClient) FetchPricing(ctx context.Context) (storage.PricingSnapshot, error) {
	var snapshot storage.PricingSnapshot
	err := c.request(ctx, http.MethodGet, "/api/v1/pricing", nil, &snapshot)
	return snapshot, err
}

func (c *ServerClient) MergePricing(ctx context.Context, snapshot storage.PricingSnapshot, policy string) (storage.PricingSnapshot, int, error) {
	var result struct {
		Snapshot  storage.PricingSnapshot `json:"snapshot"`
		Conflicts int                     `json:"conflicts"`
	}
	err := c.request(ctx, http.MethodPost, "/api/v1/pricing/merge", map[string]any{
		"snapshot": snapshot, "policy": policy,
	}, &result)
	return result.Snapshot, result.Conflicts, err
}
