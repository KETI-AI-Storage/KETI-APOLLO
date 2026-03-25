/*
Operator Client - AI Storage Orchestrator API 클라이언트
6가지 오퍼레이터와 통신:
1. Migration Operator - 티어 마이그레이션
2. AutoScaling Operator - 오토스케일링
3. Loadbalance Operator - 로드밸런싱
4. Caching Operator - 글로벌 캐싱
5. Provision Operator - 사전 프로비저닝
6. Preemption Operator - 선점
*/

package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Client AI Storage Orchestrator API 클라이언트
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient 새로운 Operator 클라이언트 생성
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ============================================
// Migration Operations
// ============================================

// StartMigration 마이그레이션 시작
func (c *Client) StartMigration(ctx context.Context, req *MigrationRequest) (*MigrationResponse, error) {
	log.Printf("[Operator] Starting migration: pod=%s/%s, source=%s, target=%s",
		req.PodNamespace, req.PodName, req.SourceNode, req.TargetNode)

	url := fmt.Sprintf("%s/api/v1/migrations", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("migration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("migration failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var migrationResp MigrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&migrationResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Migration started: id=%s, status=%s", migrationResp.MigrationID, migrationResp.Status)
	return &migrationResp, nil
}

// GetMigrationStatus 마이그레이션 상태 조회
func (c *Client) GetMigrationStatus(ctx context.Context, migrationID string) (*MigrationResponse, error) {
	url := fmt.Sprintf("%s/api/v1/migrations/%s", c.baseURL, migrationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get migration status failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get migration status failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var migrationResp MigrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&migrationResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &migrationResp, nil
}

// ============================================
// Autoscaling Operations
// ============================================

// ConfigureAutoscaling 오토스케일링 설정
func (c *Client) ConfigureAutoscaling(ctx context.Context, req *AutoscalingRequest) (*AutoscalingResponse, error) {
	log.Printf("[Operator] Configuring autoscaling: workload=%s/%s, min=%d, max=%d",
		req.WorkloadNamespace, req.WorkloadName, req.MinReplicas, req.MaxReplicas)

	url := fmt.Sprintf("%s/api/v1/autoscaling", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("autoscaling request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("autoscaling failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var autoscalingResp AutoscalingResponse
	if err := json.NewDecoder(resp.Body).Decode(&autoscalingResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Autoscaling configured: id=%s, status=%s", autoscalingResp.AutoscalingID, autoscalingResp.Status)
	return &autoscalingResp, nil
}

// ============================================
// Provisioning Operations
// ============================================

// StartProvisioning 프로비저닝 시작
func (c *Client) StartProvisioning(ctx context.Context, req *ProvisioningRequest) (*ProvisioningResponse, error) {
	log.Printf("[Operator] Starting provisioning: workload=%s/%s, size=%s, class=%s",
		req.WorkloadNamespace, req.WorkloadName, req.StorageSize, req.StorageClass)

	url := fmt.Sprintf("%s/api/v1/provisioning", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provisioning request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provisioning failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var provisioningResp ProvisioningResponse
	if err := json.NewDecoder(resp.Body).Decode(&provisioningResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Provisioning started: id=%s, status=%s", provisioningResp.ProvisioningID, provisioningResp.Status)
	return &provisioningResp, nil
}

// ============================================
// Caching Operations
// ============================================

// ConfigureCaching 캐싱 설정
func (c *Client) ConfigureCaching(ctx context.Context, req *CachingRequest) (*CachingResponse, error) {
	log.Printf("[Operator] Configuring caching: pvc=%s/%s, tier=%s",
		req.SourceNamespace, req.SourcePVC, req.TargetTier)

	url := fmt.Sprintf("%s/api/v1/caching", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("caching request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("caching failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var cachingResp CachingResponse
	if err := json.NewDecoder(resp.Body).Decode(&cachingResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Caching configured: id=%s, status=%s", cachingResp.CacheID, cachingResp.Status)
	return &cachingResp, nil
}

// ============================================
// Loadbalancing Operations
// ============================================

// ConfigureLoadbalance 로드밸런싱 설정
func (c *Client) ConfigureLoadbalance(ctx context.Context, req *LoadbalanceRequest) (*LoadbalanceResponse, error) {
	log.Printf("[Operator] Configuring loadbalance: node=%s, strategy=%s",
		req.TargetNode, req.Strategy)

	url := fmt.Sprintf("%s/api/v1/loadbalancing", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("loadbalance request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("loadbalance failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var loadbalanceResp LoadbalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&loadbalanceResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Loadbalance configured: id=%s, status=%s", loadbalanceResp.LoadbalanceID, loadbalanceResp.Status)
	return &loadbalanceResp, nil
}

// ============================================
// Preemption Operations
// ============================================

// StartPreemption 선점 시작
func (c *Client) StartPreemption(ctx context.Context, req *PreemptionRequest) (*PreemptionResponse, error) {
	log.Printf("[Operator] Starting preemption: workload=%s/%s, priority=%d",
		req.WorkloadNamespace, req.WorkloadName, req.Priority)

	url := fmt.Sprintf("%s/api/v1/preemption", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("preemption request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("preemption failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var preemptionResp PreemptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&preemptionResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Preemption started: id=%s, status=%s", preemptionResp.PreemptionID, preemptionResp.Status)
	return &preemptionResp, nil
}

// ============================================
// Health Check
// ============================================

// HealthCheck 오케스트레이터 헬스 체크
func (c *Client) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("%s/health", c.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}
