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
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// OperatorAPIError는 오케스트레이터 HTTP 오류 본문을 보존한다.
type OperatorAPIError struct {
	StatusCode int
	ErrorCode  string
	Status     string
	Message    string
	Body       string
}

func (e *OperatorAPIError) Error() string {
	if e == nil {
		return "operator api error"
	}
	return fmt.Sprintf("operator api status=%d error_code=%s status=%s message=%s body=%s",
		e.StatusCode, e.ErrorCode, e.Status, e.Message, e.Body)
}

// IsWaitingForTarget는 오케스트레이터가 타깃 대기 상태를 반환했는지 판별한다.
func (e *OperatorAPIError) IsWaitingForTarget() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusTooEarly {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(e.ErrorCode), "WaitingForTarget") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(e.Status), "waiting_for_target") {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(e.Message + " " + e.Body))
	return strings.Contains(msg, "waiting for target")
}

type operatorErrorBody struct {
	ErrorCode string `json:"error_code"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Message   string `json:"message"`
	Details   string `json:"details"`
}

func buildOperatorAPIError(statusCode int, body []byte) *OperatorAPIError {
	text := strings.TrimSpace(string(body))
	out := &OperatorAPIError{
		StatusCode: statusCode,
		Body:       text,
	}
	var payload operatorErrorBody
	if err := json.Unmarshal(body, &payload); err == nil {
		out.ErrorCode = strings.TrimSpace(payload.ErrorCode)
		out.Status = strings.TrimSpace(payload.Status)
		out.Message = strings.TrimSpace(payload.Message)
		if out.Message == "" {
			out.Message = strings.TrimSpace(payload.Error)
		}
		if out.Message == "" {
			out.Message = strings.TrimSpace(payload.Details)
		}
	}
	return out
}

// Client AI Storage Orchestrator API 클라이언트
type Client struct {
	baseURL      string
	httpClient   *http.Client
	maxRetries   int
	retryBackoff time.Duration
}

// NewClient는 Operator(오케스트레이터) HTTP 클라이언트를 생성한다.
//
// Parameters:
// - baseURL: 오케스트레이터 베이스 URL
// - httpTimeout: HTTP 타임아웃(0 이하면 30초)
//
// Returns:
// - *Client: 초기화된 클라이언트
func NewClient(baseURL string, httpTimeout time.Duration) *Client {
	if httpTimeout <= 0 {
		httpTimeout = 30 * time.Second
	}
	maxRetries := 2
	if v := os.Getenv("OPERATOR_HTTP_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	retryBackoff := 300 * time.Millisecond
	if v := os.Getenv("OPERATOR_HTTP_RETRY_BACKOFF_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			retryBackoff = time.Duration(n) * time.Millisecond
		}
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
		maxRetries:   maxRetries,
		retryBackoff: retryBackoff,
	}
}

// getJSON은 GET 요청 후 200일 때만 JSON을 디코딩한다.
//
// Returns:
// - int: HTTP 상태 코드
// - error: 네트워크·비-200·디코딩 오류
func (c *Client) getJSON(ctx context.Context, url string, out interface{}) (int, error) {
	var lastCode int
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, fmt.Errorf("failed to create request: %w", err)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if shouldRetryNetworkError(err) && attempt < c.maxRetries {
				time.Sleep(c.retryBackoff * time.Duration(attempt+1))
				continue
			}
			return 0, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastCode = resp.StatusCode
			lastErr = fmt.Errorf("read body: %w", readErr)
			if attempt < c.maxRetries {
				time.Sleep(c.retryBackoff * time.Duration(attempt+1))
				continue
			}
			return resp.StatusCode, lastErr
		}
		if resp.StatusCode != http.StatusOK {
			lastCode = resp.StatusCode
			lastErr = fmt.Errorf("GET failed with status %d: %s", resp.StatusCode, string(body))
			if shouldRetryStatus(resp.StatusCode) && attempt < c.maxRetries {
				time.Sleep(c.retryBackoff * time.Duration(attempt+1))
				continue
			}
			return resp.StatusCode, lastErr
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return resp.StatusCode, fmt.Errorf("failed to decode response: %w", err)
			}
		}
		return resp.StatusCode, nil
	}
	return lastCode, lastErr
}

func shouldRetryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func shouldRetryNetworkError(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout() || netErr.Temporary()
	}
	return false
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
		return nil, buildOperatorAPIError(resp.StatusCode, respBody)
	}

	var migrationResp MigrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&migrationResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[Operator] Migration started: id=%s, status=%s", migrationResp.MigrationID, migrationResp.Status)
	return &migrationResp, nil
}

// GetMigrationStatus는 마이그레이션 상태를 조회한다.
//
// Returns:
// - *MigrationResponse: 성공 시 본문(200일 때만 채워짐)
// - int: HTTP 상태 코드
// - error: 네트워크·비-200·디코딩 오류
func (c *Client) GetMigrationStatus(ctx context.Context, migrationID string) (*MigrationResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/migrations/%s", c.baseURL, migrationID)
	var migrationResp MigrationResponse
	code, err := c.getJSON(ctx, url, &migrationResp)
	if err != nil {
		return nil, code, err
	}
	return &migrationResp, code, nil
}

// GetAutoscaling은 오토스케일러 단건 상태를 조회한다.
func (c *Client) GetAutoscaling(ctx context.Context, autoscalerID string) (*AutoscalingResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/autoscaling/%s", c.baseURL, autoscalerID)
	var out AutoscalingResponse
	code, err := c.getJSON(ctx, url, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// GetProvisioning은 프로비저닝 단건 상태를 조회한다.
func (c *Client) GetProvisioning(ctx context.Context, provisioningID string) (*ProvisioningResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/provisioning/%s", c.baseURL, provisioningID)
	var out ProvisioningResponse
	code, err := c.getJSON(ctx, url, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// GetCaching은 캐시 단건 상태를 조회한다.
func (c *Client) GetCaching(ctx context.Context, cacheID string) (*CachingResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/caching/%s", c.baseURL, cacheID)
	var out CachingResponse
	code, err := c.getJSON(ctx, url, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// GetLoadbalancing은 로드밸런싱 작업 상태를 조회한다.
func (c *Client) GetLoadbalancing(ctx context.Context, jobID string) (*LoadbalanceResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/loadbalancing/%s", c.baseURL, jobID)
	var out LoadbalanceResponse
	code, err := c.getJSON(ctx, url, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// GetPreemption은 선점 작업 상태를 조회한다.
func (c *Client) GetPreemption(ctx context.Context, preemptionID string) (*PreemptionResponse, int, error) {
	url := fmt.Sprintf("%s/api/v1/preemption/%s", c.baseURL, preemptionID)
	var out PreemptionResponse
	code, err := c.getJSON(ctx, url, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
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
	log.Printf("[Operator] Configuring loadbalance: namespace=%s, target_nodes=%v, strategy=%s",
		req.Namespace, req.TargetNodes, req.Strategy)

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
	log.Printf("[Operator] Starting preemption: node=%s, namespace=%s, resource_type=%s, target_amount=%s",
		req.NodeName, req.Namespace, req.ResourceType, req.TargetAmount)

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

// deleteExpectNoContent는 DELETE 요청을 보내고 200/202/204/404를 성공으로 본다.
//
// Parameters:
// - url: 삭제 대상 URL
//
// Returns:
// - error: 네트워크 오류 또는 허용되지 않은 HTTP 상태
func (c *Client) deleteExpectNoContent(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create delete request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("delete failed with status %d: %s", resp.StatusCode, string(body))
	}
}

// DeleteAutoscaling은 POST로 생성된 오토스케일 리소스를 삭제한다 (DELETE /api/v1/autoscaling/:id).
func (c *Client) DeleteAutoscaling(ctx context.Context, autoscalerID string) error {
	url := fmt.Sprintf("%s/api/v1/autoscaling/%s", c.baseURL, autoscalerID)
	log.Printf("[Operator] Delete autoscaling: id=%s", autoscalerID)
	return c.deleteExpectNoContent(ctx, url)
}

// DeleteProvisioning은 프로비저닝 리소스를 삭제한다 (DELETE /api/v1/provisioning/:id).
func (c *Client) DeleteProvisioning(ctx context.Context, provisioningID string) error {
	url := fmt.Sprintf("%s/api/v1/provisioning/%s", c.baseURL, provisioningID)
	log.Printf("[Operator] Delete provisioning: id=%s", provisioningID)
	return c.deleteExpectNoContent(ctx, url)
}

// DeleteCaching은 캐시 리소스를 삭제한다 (DELETE /api/v1/caching/:id).
func (c *Client) DeleteCaching(ctx context.Context, cacheID string) error {
	url := fmt.Sprintf("%s/api/v1/caching/%s", c.baseURL, cacheID)
	log.Printf("[Operator] Delete caching: id=%s", cacheID)
	return c.deleteExpectNoContent(ctx, url)
}

// DeleteLoadbalancing은 로드밸런싱 작업을 취소한다 (DELETE /api/v1/loadbalancing/:id).
func (c *Client) DeleteLoadbalancing(ctx context.Context, jobID string) error {
	url := fmt.Sprintf("%s/api/v1/loadbalancing/%s", c.baseURL, jobID)
	log.Printf("[Operator] Delete loadbalancing: id=%s", jobID)
	return c.deleteExpectNoContent(ctx, url)
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
