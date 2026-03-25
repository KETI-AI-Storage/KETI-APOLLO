/*
Forecaster Client - Node Resource Forecaster gRPC 클라이언트
*/

package forecaster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// Client는 Node Resource Forecaster와 통신하는 클라이언트
type Client struct {
	httpEndpoint string
	grpcEndpoint string
	httpClient   *http.Client
	thresholds   ThresholdConfig

	// 캐시
	cacheMu        sync.RWMutex
	forecastCache  map[string]*CachedForecast
	cacheTTL       time.Duration
}

// CachedForecast 캐시된 예측 결과
type CachedForecast struct {
	Response  *ForecastNodeResponse
	FetchedAt time.Time
}

// NewClient 새로운 Forecaster 클라이언트 생성
func NewClient(httpEndpoint, grpcEndpoint string) *Client {
	return &Client{
		httpEndpoint: httpEndpoint,
		grpcEndpoint: grpcEndpoint,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		thresholds:    DefaultThresholdConfig(),
		forecastCache: make(map[string]*CachedForecast),
		cacheTTL:      30 * time.Second,
	}
}

// SetThresholds 임계값 설정
func (c *Client) SetThresholds(thresholds ThresholdConfig) {
	c.thresholds = thresholds
}

// HealthCheck Forecaster 헬스 체크
func (c *Client) HealthCheck(ctx context.Context) (*HealthCheckResponse, error) {
	url := fmt.Sprintf("%s/health", c.httpEndpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	var health HealthCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &health, nil
}

// ForecastNode 특정 노드의 리소스 예측 조회
func (c *Client) ForecastNode(ctx context.Context, nodeName string, horizons []int32) (*ForecastNodeResponse, error) {
	// 캐시 확인
	c.cacheMu.RLock()
	if cached, ok := c.forecastCache[nodeName]; ok {
		if time.Since(cached.FetchedAt) < c.cacheTTL {
			c.cacheMu.RUnlock()
			return cached.Response, nil
		}
	}
	c.cacheMu.RUnlock()

	// API 호출
	url := fmt.Sprintf("%s/api/v1/forecast/node/%s", c.httpEndpoint, nodeName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forecast request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("forecast returned status %d: %s", resp.StatusCode, string(body))
	}

	var forecast ForecastNodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&forecast); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// 캐시 저장
	c.cacheMu.Lock()
	c.forecastCache[nodeName] = &CachedForecast{
		Response:  &forecast,
		FetchedAt: time.Now(),
	}
	c.cacheMu.Unlock()

	return &forecast, nil
}

// ForecastCluster 클러스터 전체 예측 조회
func (c *Client) ForecastCluster(ctx context.Context, horizons []int32, includeNodeBreakdown bool) (*ForecastClusterResponse, error) {
	url := fmt.Sprintf("%s/api/v1/forecast/cluster?include_nodes=%v", c.httpEndpoint, includeNodeBreakdown)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cluster forecast request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cluster forecast returned status %d: %s", resp.StatusCode, string(body))
	}

	var forecast ForecastClusterResponse
	if err := json.NewDecoder(resp.Body).Decode(&forecast); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &forecast, nil
}

// AnalyzeAndRecommend 예측 결과 분석 및 정책 추천
// 발표자료 기반: Multi-LightGBM 정책 결정 사용 (있으면), 없으면 기존 threshold 기반
func (c *Client) AnalyzeAndRecommend(ctx context.Context, nodeName string) ([]PolicyRecommendation, error) {
	// Try Multi-LightGBM policy recommendations first (발표자료 구현)
	recommendations, err := c.GetPolicyRecommendations(ctx, nodeName)
	if err == nil && len(recommendations) > 0 {
		log.Printf("[Forecaster] Using Multi-LightGBM recommendations for node %s: %d recommendations", nodeName, len(recommendations))
		return recommendations, nil
	}

	// Fallback to threshold-based analysis
	log.Printf("[Forecaster] Multi-LightGBM not available, using threshold-based analysis for node %s", nodeName)
	return c.analyzeWithThresholds(ctx, nodeName)
}

// GetPolicyRecommendations Multi-LightGBM 정책 추천 조회 (발표자료 구현)
func (c *Client) GetPolicyRecommendations(ctx context.Context, nodeName string) ([]PolicyRecommendation, error) {
	url := fmt.Sprintf("%s/api/v1/policy/recommendations/%s", c.httpEndpoint, nodeName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("policy recommendations request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("policy recommendations returned status %d: %s", resp.StatusCode, string(body))
	}

	var recommendations []PolicyRecommendation
	if err := json.NewDecoder(resp.Body).Decode(&recommendations); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return recommendations, nil
}

// GetOrchestrationPolicy Multi-LightGBM 전체 정책 결정 조회 (7개 모델)
func (c *Client) GetOrchestrationPolicy(ctx context.Context, nodeName string) (*OrchestrationPolicyResponse, error) {
	url := fmt.Sprintf("%s/api/v1/policy/%s", c.httpEndpoint, nodeName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("orchestration policy request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("orchestration policy returned status %d: %s", resp.StatusCode, string(body))
	}

	var policy OrchestrationPolicyResponse
	if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &policy, nil
}

// analyzeWithThresholds 기존 threshold 기반 분석 (fallback)
func (c *Client) analyzeWithThresholds(ctx context.Context, nodeName string) ([]PolicyRecommendation, error) {
	forecast, err := c.ForecastNode(ctx, nodeName, []int32{15, 30, 60, 120})
	if err != nil {
		return nil, fmt.Errorf("failed to get forecast: %w", err)
	}

	var recommendations []PolicyRecommendation

	for _, f := range forecast.Forecasts {
		// CPU 분석
		if rec := c.analyzeResource(nodeName, "CPU", f.PredictedCPUUtilization, f.HorizonMinutes, c.thresholds.CPUWarning, c.thresholds.CPUCritical); rec != nil {
			recommendations = append(recommendations, *rec)
		}

		// Memory 분석
		if rec := c.analyzeResource(nodeName, "MEMORY", f.PredictedMemoryUtilization, f.HorizonMinutes, c.thresholds.MemoryWarning, c.thresholds.MemoryCritical); rec != nil {
			recommendations = append(recommendations, *rec)
		}

		// GPU 분석
		if rec := c.analyzeResource(nodeName, "GPU", f.PredictedGPUUtilization, f.HorizonMinutes, c.thresholds.GPUWarning, c.thresholds.GPUCritical); rec != nil {
			recommendations = append(recommendations, *rec)
		}

		// Storage I/O 분석
		if rec := c.analyzeResource(nodeName, "STORAGE_IO", f.PredictedStorageIOUtilization, f.HorizonMinutes, c.thresholds.StorageWarning, c.thresholds.StorageCritical); rec != nil {
			recommendations = append(recommendations, *rec)
		}
	}

	return recommendations, nil
}

// analyzeResource 단일 리소스 분석
func (c *Client) analyzeResource(nodeName, resourceType string, predicted float64, horizon int32, warning, critical float64) *PolicyRecommendation {
	if predicted < warning {
		return nil // 정상 범위
	}

	rec := &PolicyRecommendation{
		NodeName:             nodeName,
		ResourceType:         resourceType,
		PredictedUtilization: predicted,
		HorizonMinutes:       horizon,
	}

	if predicted >= critical {
		// 위험 수준 - 즉시 조치 필요
		rec.Urgency = "CRITICAL"
		rec.Probability = int32(predicted * 100)
		rec.Threshold = critical

		// 리소스 타입에 따른 정책 결정
		switch resourceType {
		case "CPU", "MEMORY":
			rec.PolicyType = "migration"
			rec.Reason = fmt.Sprintf("%s 사용률 %.1f%% 예측 (임계값 %.0f%% 초과) - %d분 후 - 마이그레이션 권장",
				resourceType, predicted*100, critical*100, horizon)
		case "GPU":
			rec.PolicyType = "preemption"
			rec.Reason = fmt.Sprintf("GPU 사용률 %.1f%% 예측 (임계값 %.0f%% 초과) - %d분 후 - 선점 또는 마이그레이션 권장",
				predicted*100, critical*100, horizon)
		case "STORAGE_IO":
			rec.PolicyType = "caching"
			rec.Reason = fmt.Sprintf("Storage I/O %.1f%% 예측 (임계값 %.0f%% 초과) - %d분 후 - 캐싱 또는 로드밸런싱 권장",
				predicted*100, critical*100, horizon)
		}
	} else {
		// 경고 수준 - 사전 조치 권장
		rec.Urgency = "HIGH"
		rec.Probability = int32(predicted * 100)
		rec.Threshold = warning

		switch resourceType {
		case "CPU", "MEMORY":
			rec.PolicyType = "scaling"
			rec.Reason = fmt.Sprintf("%s 사용률 %.1f%% 예측 (경고 %.0f%% 초과) - %d분 후 - 스케일링 권장",
				resourceType, predicted*100, warning*100, horizon)
		case "GPU":
			rec.PolicyType = "provisioning"
			rec.Reason = fmt.Sprintf("GPU 사용률 %.1f%% 예측 (경고 %.0f%% 초과) - %d분 후 - 사전 프로비저닝 권장",
				predicted*100, warning*100, horizon)
		case "STORAGE_IO":
			rec.PolicyType = "loadbalance"
			rec.Reason = fmt.Sprintf("Storage I/O %.1f%% 예측 (경고 %.0f%% 초과) - %d분 후 - 로드밸런싱 권장",
				predicted*100, warning*100, horizon)
		}
	}

	return rec
}

// GetAllNodeForecasts 모든 노드의 예측 조회
func (c *Client) GetAllNodeForecasts(ctx context.Context, nodeNames []string) (map[string]*ForecastNodeResponse, error) {
	results := make(map[string]*ForecastNodeResponse)
	var wg sync.WaitGroup
	var mu sync.Mutex
	errCh := make(chan error, len(nodeNames))

	for _, nodeName := range nodeNames {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			forecast, err := c.ForecastNode(ctx, name, []int32{5, 10, 30, 60})
			if err != nil {
				log.Printf("Failed to get forecast for node %s: %v", name, err)
				errCh <- err
				return
			}

			mu.Lock()
			results[name] = forecast
			mu.Unlock()
		}(nodeName)
	}

	wg.Wait()
	close(errCh)

	// 에러가 있어도 성공한 결과는 반환
	return results, nil
}

// ClearCache 캐시 초기화
func (c *Client) ClearCache() {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.forecastCache = make(map[string]*CachedForecast)
}
