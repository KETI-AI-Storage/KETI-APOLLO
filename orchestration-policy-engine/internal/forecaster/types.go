/*
Forecaster Types - Node Resource Forecaster gRPC 응답 타입
*/

package forecaster

import "time"

// ForecastNodeRequest 노드 리소스 예측 요청
type ForecastNodeRequest struct {
	NodeName               string  `json:"node_name"`
	ForecastHorizonsMinutes []int32 `json:"forecast_horizons_minutes"` // e.g., [5, 10, 30, 60]
}

// ForecastClusterRequest 클러스터 전체 예측 요청
type ForecastClusterRequest struct {
	ForecastHorizonsMinutes []int32 `json:"forecast_horizons_minutes"`
	IncludeNodeBreakdown    bool    `json:"include_node_breakdown"`
}

// ForecastNodeResponse 노드 예측 응답
type ForecastNodeResponse struct {
	NodeName   string             `json:"node_name"`
	Forecasts  []ResourceForecast `json:"forecasts"`
	Confidence float64            `json:"confidence"`
}

// ForecastClusterResponse 클러스터 예측 응답
type ForecastClusterResponse struct {
	ClusterForecasts []ResourceForecast           `json:"cluster_forecasts"`
	NodeForecasts    map[string]NodeForecastResult `json:"node_forecasts"`
}

// NodeForecastResult 노드별 예측 결과
type NodeForecastResult struct {
	Forecasts  []ResourceForecast `json:"forecasts"`
	Confidence float64            `json:"confidence"`
}

// ResourceForecast 리소스 예측 결과
type ResourceForecast struct {
	HorizonMinutes                 int32   `json:"horizon_minutes"`
	PredictedCPUUtilization        float64 `json:"predicted_cpu_utilization"`
	PredictedMemoryUtilization     float64 `json:"predicted_memory_utilization"`
	PredictedGPUUtilization        float64 `json:"predicted_gpu_utilization"`
	PredictedStorageIOUtilization  float64 `json:"predicted_storage_io_utilization"`
	ConfidenceIntervalLower        float64 `json:"confidence_interval_lower"`
	ConfidenceIntervalUpper        float64 `json:"confidence_interval_upper"`
}

// HealthCheckResponse 헬스체크 응답
type HealthCheckResponse struct {
	Healthy          bool      `json:"healthy"`
	Version          string    `json:"version"`
	ModelReady       int32     `json:"model_ready"` // 0: not ready, 1: ready
	TotalPredictions int64     `json:"total_predictions"`
	LastTrainingTime time.Time `json:"last_training_time"`
}

// PolicyRecommendation 정책 추천 (예측 기반)
type PolicyRecommendation struct {
	NodeName       string  `json:"node_name"`
	PolicyType     string  `json:"policy_type"`     // migration, scaling, provisioning, etc.
	Urgency        string  `json:"urgency"`         // LOW, MEDIUM, HIGH, CRITICAL
	Probability    int32   `json:"probability"`     // 0-100
	ResourceType   string  `json:"resource_type"`   // CPU, MEMORY, GPU, STORAGE_IO
	Reason         string  `json:"reason"`
	HorizonMinutes int32   `json:"horizon_minutes"`

	// 상세 정보
	CurrentUtilization   float64 `json:"current_utilization"`
	PredictedUtilization float64 `json:"predicted_utilization"`
	Threshold            float64 `json:"threshold"`
}

// ThresholdConfig 임계값 설정
type ThresholdConfig struct {
	CPUWarning      float64 `json:"cpu_warning"`      // 70%
	CPUCritical     float64 `json:"cpu_critical"`     // 85%
	MemoryWarning   float64 `json:"memory_warning"`   // 75%
	MemoryCritical  float64 `json:"memory_critical"`  // 90%
	GPUWarning      float64 `json:"gpu_warning"`      // 80%
	GPUCritical     float64 `json:"gpu_critical"`     // 95%
	StorageWarning  float64 `json:"storage_warning"`  // 70%
	StorageCritical float64 `json:"storage_critical"` // 85%
}

// DefaultThresholdConfig 기본 임계값
func DefaultThresholdConfig() ThresholdConfig {
	return ThresholdConfig{
		CPUWarning:      0.70,
		CPUCritical:     0.85,
		MemoryWarning:   0.75,
		MemoryCritical:  0.90,
		GPUWarning:      0.80,
		GPUCritical:     0.95,
		StorageWarning:  0.70,
		StorageCritical: 0.85,
	}
}

// ============================================
// Multi-LightGBM Policy Decision Types
// 발표자료 기반: 7개 정책 결정 모델
// ============================================

// OrchestrationPolicyResponse Multi-LightGBM 정책 결정 응답
type OrchestrationPolicyResponse struct {
	NodeName   string           `json:"node_name"`
	Timestamp  int64            `json:"timestamp"`
	Decisions  []PolicyDecision `json:"decisions"`
	Predictions PolicyPredictions `json:"predictions"`
}

// PolicyDecision 단일 정책 결정 (7개 Task 중 하나)
type PolicyDecision struct {
	TaskName    string                 `json:"task_name"`    // node_health, autoscale, migration, caching, load_balancing, provisioning, storage_tiering
	Decision    string                 `json:"decision"`     // YES/NO or NORMAL/STRESSED/CRITICAL
	Probability float64                `json:"probability"`  // 0.0 ~ 1.0
	Urgency     string                 `json:"urgency"`      // LOW, MEDIUM, HIGH, CRITICAL
	Confidence  float64                `json:"confidence"`   // 0.0 ~ 1.0
	Reason      string                 `json:"reason"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// PolicyPredictions LSTM 예측 결과 (15/30/60분)
type PolicyPredictions struct {
	Min15 map[string]float64 `json:"15min"`
	Min30 map[string]float64 `json:"30min"`
	Min60 map[string]float64 `json:"60min"`
}
