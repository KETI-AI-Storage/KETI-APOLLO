// ============================================
// APOLLO Orchestrator Client
// AI-Storage Orchestrator에 마이그레이션 정책 전달
// ============================================

package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"apollo/pkg/types"
)

// OrchestratorClient communicates with AI-Storage Orchestrator
type OrchestratorClient struct {
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
	stats OrchestratorClientStats
}

// OrchestratorClientStats tracks client statistics
type OrchestratorClientStats struct {
	TotalPoliciesSent       int64
	TotalMigrationsStarted  int64
	TotalMigrationsComplete int64
	TotalErrors             int64
	LastSuccessTime         time.Time
	LastErrorTime           time.Time
}

// OrchestratorClientConfig holds client configuration
type OrchestratorClientConfig struct {
	Endpoint      string
	TimeoutSec    int
	MaxRetries    int
	RetryInterval time.Duration
}

// MigrationRequest represents a request to start migration
type MigrationRequest struct {
	PodName      string `json:"pod_name"`
	PodNamespace string `json:"pod_namespace"`
	SourceNode   string `json:"source_node"`
	TargetNode   string `json:"target_node"`
	PreservePV   bool   `json:"preserve_pv"`
	Timeout      int    `json:"timeout"`
}

// MigrationResponse represents the orchestrator's response
type MigrationResponse struct {
	MigrationID string `json:"migration_id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}

// NewOrchestratorClient creates a new OrchestratorClient
func NewOrchestratorClient(config OrchestratorClientConfig) *OrchestratorClient {
	if config.TimeoutSec == 0 {
		config.TimeoutSec = 30 // Longer timeout for migrations
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = 2 * time.Second
	}

	return &OrchestratorClient{
		endpoint: config.Endpoint,
		httpClient: &http.Client{
			Timeout: time.Duration(config.TimeoutSec) * time.Second,
		},
		maxRetries:    config.MaxRetries,
		retryInterval: config.RetryInterval,
	}
}

// Connect establishes connection to the orchestrator
func (c *OrchestratorClient) Connect(ctx context.Context) error {
	healthURL := fmt.Sprintf("%s/health", c.endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.stats.TotalErrors++
		c.stats.LastErrorTime = time.Now()
		return fmt.Errorf("failed to connect to orchestrator: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("orchestrator health check failed: status %d", resp.StatusCode)
	}

	c.connMux.Lock()
	c.connected = true
	c.connMux.Unlock()

	log.Printf("[OrchestratorClient] Connected to orchestrator at %s", c.endpoint)
	return nil
}

// IsConnected returns connection status
func (c *OrchestratorClient) IsConnected() bool {
	c.connMux.RLock()
	defer c.connMux.RUnlock()
	return c.connected
}

// SendOrchestrationPolicy sends an orchestration policy to the orchestrator
func (c *OrchestratorClient) SendOrchestrationPolicy(ctx context.Context, policy *types.OrchestrationPolicy) error {
	c.stats.TotalPoliciesSent++

	// If policy decision is migrate, trigger migration
	if policy.Decision == types.OrchDecisionMigrate {
		return c.TriggerMigration(ctx, policy)
	}

	// For other decisions, just notify
	url := fmt.Sprintf("%s/api/v1/policies/orchestration", c.endpoint)

	data, err := json.Marshal(policy)
	if err != nil {
		c.stats.TotalErrors++
		return fmt.Errorf("failed to marshal policy: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.stats.TotalErrors++
		c.stats.LastErrorTime = time.Now()
		return fmt.Errorf("failed to send policy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("orchestrator rejected policy: status %d", resp.StatusCode)
	}

	c.stats.LastSuccessTime = time.Now()
	return nil
}

// TriggerMigration triggers a pod migration via the orchestrator API
func (c *OrchestratorClient) TriggerMigration(ctx context.Context, policy *types.OrchestrationPolicy) error {
	c.stats.TotalMigrationsStarted++

	url := fmt.Sprintf("%s/api/v1/migrations", c.endpoint)

	// Build migration request
	migReq := MigrationRequest{
		PodName:      policy.PodName,
		PodNamespace: policy.PodNamespace,
		SourceNode:   "", // Will be determined by orchestrator
		TargetNode:   policy.TargetNode,
		PreservePV:   true,
		Timeout:      600, // 10 minutes default
	}

	if policy.MigrationConstraints != nil {
		migReq.PreservePV = policy.MigrationConstraints.PreservePV
		migReq.Timeout = policy.MigrationConstraints.TimeoutMinutes * 60
	}

	data, err := json.Marshal(migReq)
	if err != nil {
		c.stats.TotalErrors++
		return fmt.Errorf("failed to marshal migration request: %w", err)
	}

	// Retry loop
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(c.retryInterval)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
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
			// Parse response
			var migResp MigrationResponse
			if err := json.NewDecoder(resp.Body).Decode(&migResp); err != nil {
				log.Printf("[OrchestratorClient] Warning: failed to decode response: %v", err)
			}

			c.stats.LastSuccessTime = time.Now()
			log.Printf("[OrchestratorClient] Migration started: %s -> %s (id: %s)",
				policy.PodName, policy.TargetNode, migResp.MigrationID)
			return nil
		}

		body, _ := io.ReadAll(resp.Body)
		lastErr = fmt.Errorf("orchestrator rejected migration: status %d, body: %s", resp.StatusCode, string(body))
	}

	c.stats.TotalErrors++
	c.stats.LastErrorTime = time.Now()
	return fmt.Errorf("failed to trigger migration after %d attempts: %w", c.maxRetries, lastErr)
}

// GetMigrationStatus gets the status of a migration
func (c *OrchestratorClient) GetMigrationStatus(ctx context.Context, migrationID string) (*MigrationResponse, error) {
	url := fmt.Sprintf("%s/api/v1/migrations/%s", c.endpoint, migrationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.stats.TotalErrors++
		return nil, fmt.Errorf("failed to get migration status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get migration status: %d", resp.StatusCode)
	}

	var migResp MigrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&migResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &migResp, nil
}

// GetStats returns client statistics
func (c *OrchestratorClient) GetStats() OrchestratorClientStats {
	return c.stats
}

// Close closes the client connection
func (c *OrchestratorClient) Close() error {
	c.connMux.Lock()
	c.connected = false
	c.connMux.Unlock()
	return nil
}

// jsonReader creates an io.Reader from JSON data
func jsonReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}
