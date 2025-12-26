// ============================================
// APOLLO Scheduler Client
// AI-Storage Scheduler에 정책 전달
// ============================================

package scheduling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"apollo/pkg/types"
)

// SchedulerClient communicates with AI-Storage Scheduler
type SchedulerClient struct {
	// Connection settings
	endpoint   string
	httpClient *http.Client

	// Retry configuration
	maxRetries    int
	retryInterval time.Duration

	// Connection state
	connected bool
	connMux   sync.RWMutex

	// Statistics
	stats SchedulerClientStats
}

// SchedulerClientStats tracks client statistics
type SchedulerClientStats struct {
	TotalPoliciesSent     int64
	TotalPoliciesAccepted int64
	TotalPoliciesRejected int64
	TotalErrors           int64
	LastSuccessTime       time.Time
	LastErrorTime         time.Time
}

// SchedulerClientConfig holds client configuration
type SchedulerClientConfig struct {
	Endpoint      string
	TimeoutSec    int
	MaxRetries    int
	RetryInterval time.Duration
}

// NewSchedulerClient creates a new SchedulerClient
func NewSchedulerClient(config SchedulerClientConfig) *SchedulerClient {
	if config.TimeoutSec == 0 {
		config.TimeoutSec = 10
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = time.Second
	}

	return &SchedulerClient{
		endpoint: config.Endpoint,
		httpClient: &http.Client{
			Timeout: time.Duration(config.TimeoutSec) * time.Second,
		},
		maxRetries:    config.MaxRetries,
		retryInterval: config.RetryInterval,
	}
}

// Connect establishes connection to the scheduler
func (c *SchedulerClient) Connect(ctx context.Context) error {
	// Test connection with health check
	healthURL := fmt.Sprintf("%s/health", c.endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.stats.TotalErrors++
		c.stats.LastErrorTime = time.Now()
		return fmt.Errorf("failed to connect to scheduler: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scheduler health check failed: status %d", resp.StatusCode)
	}

	c.connMux.Lock()
	c.connected = true
	c.connMux.Unlock()

	log.Printf("[SchedulerClient] Connected to scheduler at %s", c.endpoint)
	return nil
}

// IsConnected returns connection status
func (c *SchedulerClient) IsConnected() bool {
	c.connMux.RLock()
	defer c.connMux.RUnlock()
	return c.connected
}

// SendSchedulingPolicy sends a scheduling policy to the scheduler
func (c *SchedulerClient) SendSchedulingPolicy(ctx context.Context, policy *types.SchedulingPolicy) error {
	c.stats.TotalPoliciesSent++

	url := fmt.Sprintf("%s/api/v1/policies/scheduling", c.endpoint)

	// Serialize policy
	data, err := json.Marshal(policy)
	if err != nil {
		c.stats.TotalErrors++
		return fmt.Errorf("failed to marshal policy: %w", err)
	}

	// Retry loop
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(c.retryInterval)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		// Create request body (need to recreate for each attempt)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url,
			bytes.NewReader(data))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			c.stats.TotalPoliciesAccepted++
			c.stats.LastSuccessTime = time.Now()
			log.Printf("[SchedulerClient] Policy %s accepted by scheduler", policy.RequestID)
			return nil
		}

		lastErr = fmt.Errorf("scheduler rejected policy: status %d", resp.StatusCode)
		c.stats.TotalPoliciesRejected++
	}

	c.stats.TotalErrors++
	c.stats.LastErrorTime = time.Now()
	return fmt.Errorf("failed to send policy after %d attempts: %w", c.maxRetries, lastErr)
}

// NotifyPolicyUpdate notifies scheduler of policy updates (via webhook)
func (c *SchedulerClient) NotifyPolicyUpdate(ctx context.Context, podNamespace, podName string) error {
	url := fmt.Sprintf("%s/api/v1/policies/notify", c.endpoint)

	notification := map[string]string{
		"pod_namespace": podNamespace,
		"pod_name":      podName,
		"action":        "policy_available",
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.stats.TotalErrors++
		return fmt.Errorf("failed to notify scheduler: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scheduler notification failed: status %d", resp.StatusCode)
	}

	return nil
}

// GetStats returns client statistics
func (c *SchedulerClient) GetStats() SchedulerClientStats {
	return c.stats
}

// Close closes the client connection
func (c *SchedulerClient) Close() error {
	c.connMux.Lock()
	c.connected = false
	c.connMux.Unlock()
	return nil
}
