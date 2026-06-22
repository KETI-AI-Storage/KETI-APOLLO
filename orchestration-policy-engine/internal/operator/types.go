/*
Operator Types - AI Storage Orchestrator API 타입
*/

package operator

import "time"

// ============================================
// Migration Types
// ============================================

// MigrationRequest 마이그레이션 요청
type MigrationRequest struct {
	PodName            string                 `json:"pod_name,omitempty"`
	PodNamespace       string                 `json:"pod_namespace,omitempty"`
	WorkloadName       string                 `json:"workload_name,omitempty"`
	WorkloadNamespace  string                 `json:"workload_namespace,omitempty"`
	WorkloadType       string                 `json:"workload_type,omitempty"`
	SourceNode         string                 `json:"source_node,omitempty"`
	TargetNode         string                 `json:"target_node"`
	PreservePV         bool                   `json:"preserve_pv,omitempty"`
	ForceRestart       bool                   `json:"force_restart,omitempty"`
	Timeout            int                    `json:"timeout,omitempty"` // seconds
	Stage              string                 `json:"stage,omitempty"`
	RunID              string                 `json:"run_id,omitempty"`
	PolicyName         string                 `json:"policy_name,omitempty"`
	Action             string                 `json:"action,omitempty"`
	ResourceRequests   map[string]string      `json:"resource_requests,omitempty"`
	SchedulerName      string                 `json:"scheduler_name,omitempty"`
	NodeSelector       map[string]string      `json:"node_selector,omitempty"`
	Affinity           map[string]interface{} `json:"affinity,omitempty"`
	QueueLabel         string                 `json:"queue_label,omitempty"`
	StorageAnnotations map[string]string      `json:"storage_annotations,omitempty"`
	PolicyAnnotations  map[string]string      `json:"policy_annotations,omitempty"`
}

// MigrationResponse 마이그레이션 응답
type MigrationResponse struct {
	MigrationID string            `json:"migration_id"`
	Status      string            `json:"status"` // pending, running, completed, failed
	Message     string            `json:"message"`
	Details     *MigrationDetails `json:"details,omitempty"`
}

// MigrationDetails 마이그레이션 상세
type MigrationDetails struct {
	StartTime   time.Time  `json:"start_time"`
	EndTime     *time.Time `json:"end_time,omitempty"`
	NewPodName  string     `json:"new_pod_name,omitempty"`
	PVClaimName string     `json:"pv_claim_name,omitempty"`
}

// ============================================
// Autoscaling Types
// ============================================

// AutoscalingRequest 오토스케일링 요청
type AutoscalingRequest struct {
	WorkloadName      string `json:"workload_name"`
	WorkloadNamespace string `json:"workload_namespace"`
	WorkloadType      string `json:"workload_type"` // Deployment, StatefulSet
	MinReplicas       int32  `json:"min_replicas"`
	MaxReplicas       int32  `json:"max_replicas"`
	TargetCPU         int32  `json:"target_cpu_percent,omitempty"`
	TargetMemory      int32  `json:"target_memory_percent,omitempty"`
	TargetGPU         int32  `json:"target_gpu_percent,omitempty"`
}

// AutoscalingResponse 오토스케일링 응답
type AutoscalingResponse struct {
	AutoscalingID string              `json:"autoscaling_id"`
	Status        string              `json:"status"` // active, inactive, failed
	Message       string              `json:"message"`
	Details       *AutoscalingDetails `json:"details,omitempty"`
}

// AutoscalingDetails 오토스케일링 상세
type AutoscalingDetails struct {
	CurrentReplicas int32  `json:"current_replicas"`
	DesiredReplicas int32  `json:"desired_replicas"`
	HPAName         string `json:"hpa_name,omitempty"`
}

// ============================================
// Provisioning Types
// ============================================

// ProvisioningRequest 프로비저닝 요청
type ProvisioningRequest struct {
	WorkloadName      string            `json:"workload_name"`
	WorkloadNamespace string            `json:"workload_namespace"`
	WorkloadType      string            `json:"workload_type"` // training, inference, data-pipeline
	RunID             string            `json:"run_id,omitempty"`
	TargetStage       string            `json:"target_stage,omitempty"`
	StorageSize       string            `json:"storage_size,omitempty"`
	StorageClass      string            `json:"storage_class,omitempty"`
	AccessMode        string            `json:"access_mode,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// ProvisioningResponse 프로비저닝 응답
type ProvisioningResponse struct {
	ProvisioningID string               `json:"provisioning_id"`
	Status         string               `json:"status"` // pending, creating, ready, failed
	Message        string               `json:"message"`
	Details        *ProvisioningDetails `json:"details,omitempty"`
}

// ProvisioningDetails 프로비저닝 상세
type ProvisioningDetails struct {
	PVCName     string `json:"pvc_name"`
	ActualSize  string `json:"actual_size"`
	ActualClass string `json:"actual_class"`
}

// ============================================
// Caching Types
// ============================================

// CachingRequest 캐싱 요청
type CachingRequest struct {
	SourcePVC       string `json:"source_pvc"`
	SourceNamespace string `json:"source_namespace"`
	SourcePath      string `json:"source_path,omitempty"`
	TargetTier      string `json:"target_tier"` // nvme, ssd, hdd, auto
	CacheSize       string `json:"cache_size,omitempty"`
	CachePolicy     string `json:"cache_policy,omitempty"` // lru, lfu, fifo, ttl
	Priority        int32  `json:"priority,omitempty"`
	Prefetch        bool   `json:"prefetch,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// CachingResponse 캐싱 응답
type CachingResponse struct {
	CacheID string        `json:"cache_id"`
	Status  string        `json:"status"` // pending, loading, active, failed
	Message string        `json:"message"`
	Details *CacheDetails `json:"details,omitempty"`
}

// CacheDetails 캐시 상세
type CacheDetails struct {
	TargetTier     string  `json:"target_tier"`
	CacheSizeBytes int64   `json:"cache_size_bytes"`
	HitRatio       float64 `json:"hit_ratio,omitempty"`
}

// ============================================
// Loadbalancing Types
// ============================================

// LoadbalanceRequest 로드밸런싱 요청
type LoadbalanceRequest struct {
	Namespace   string   `json:"namespace,omitempty"`
	TargetNodes []string `json:"target_nodes,omitempty"`
	Strategy    string   `json:"strategy,omitempty"`

	// 임계치/한도는 0이면 오케스트레이터 기본값(80/80/80/5)이 적용된다.
	CPUThreshold          int32 `json:"cpu_threshold,omitempty"`
	MemoryThreshold       int32 `json:"memory_threshold,omitempty"`
	GPUThreshold          int32 `json:"gpu_threshold,omitempty"`
	MaxMigrationsPerCycle int32 `json:"max_migrations_per_cycle,omitempty"`
}

// LoadbalanceResponse 로드밸런싱 응답
type LoadbalanceResponse struct {
	LoadbalanceID string `json:"loadbalancing_id"`
	Status        string `json:"status"`
	Message       string `json:"message"`
}

// ============================================
// Preemption Types
// ============================================

// PreemptionRequest 선점 요청
type PreemptionRequest struct {
	NodeName     string `json:"node_name"`
	Namespace    string `json:"namespace,omitempty"`
	ResourceType string `json:"resource_type"`
	TargetAmount string `json:"target_amount"`
	Reason       string `json:"reason,omitempty"`

	// MinPriority: 이 값 미만 priority의 Pod만 후보가 된다.
	// 기본 0이면 일반 Pod(priority=0)도 후보 제외되므로 데모에서 양수 권장.
	Strategy            string   `json:"strategy,omitempty"`
	MinPriority         int32    `json:"min_priority,omitempty"`
	MaxPodsToPreempt    int32    `json:"max_pods_to_preempt,omitempty"`
	GracePeriodSeconds  int64    `json:"grace_period_seconds,omitempty"`
	ProtectedNamespaces []string `json:"protected_namespaces,omitempty"`
}

// PreemptionResponse 선점 응답
type PreemptionResponse struct {
	PreemptionID  string   `json:"preemption_id"`
	Status        string   `json:"status"`
	Message       string   `json:"message"`
	PreemptedPods []string `json:"preempted_pods,omitempty"`
}

// ============================================
// Common Types
// ============================================

// OperationStatus 오퍼레이션 상태 조회 응답
type OperationStatus struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"` // migration, scaling, provisioning, etc.
	Status    string     `json:"status"`
	Message   string     `json:"message"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}
