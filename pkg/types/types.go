// ============================================
// APOLLO - Adaptive Policy Optimization for Low-Latency I/O
// KETI AI Storage System - Go Type Definitions
// ============================================

package types

import (
	"time"
)

// ============================================
// Enum Types
// ============================================

type WorkloadType string

const (
	WorkloadTypeUnknown    WorkloadType = "unknown"
	WorkloadTypeImage      WorkloadType = "image"      // CNN, Vision
	WorkloadTypeText       WorkloadType = "text"       // NLP, LLM
	WorkloadTypeTabular    WorkloadType = "tabular"    // ML
	WorkloadTypeAudio      WorkloadType = "audio"
	WorkloadTypeVideo      WorkloadType = "video"
	WorkloadTypeMultimodal WorkloadType = "multimodal"
	WorkloadTypeGeneric    WorkloadType = "generic"
)

type PipelineStage string

const (
	PipelineStageUnknown       PipelineStage = "unknown"
	PipelineStageDataLoading   PipelineStage = "data_loading"
	PipelineStagePreprocessing PipelineStage = "preprocessing"
	PipelineStageTraining      PipelineStage = "training"
	PipelineStageValidation    PipelineStage = "validation"
	PipelineStageInference     PipelineStage = "inference"
	PipelineStagePostprocessing PipelineStage = "postprocessing"
	PipelineStageCheckpointing PipelineStage = "checkpointing"
	PipelineStageIdle          PipelineStage = "idle"
)

type IOPattern string

const (
	IOPatternUnknown    IOPattern = "unknown"
	IOPatternReadHeavy  IOPattern = "read_heavy"
	IOPatternWriteHeavy IOPattern = "write_heavy"
	IOPatternBalanced   IOPattern = "balanced"
	IOPatternSequential IOPattern = "sequential"
	IOPatternRandom     IOPattern = "random"
	IOPatternBursty     IOPattern = "bursty"
)

type SchedulingDecision string

const (
	SchedulingDecisionUnspecified SchedulingDecision = "unspecified"
	SchedulingDecisionAllow       SchedulingDecision = "allow"
	SchedulingDecisionPrefer      SchedulingDecision = "prefer"
	SchedulingDecisionRequire     SchedulingDecision = "require"
	SchedulingDecisionDelay       SchedulingDecision = "delay"
	SchedulingDecisionReject      SchedulingDecision = "reject"
)

type OrchestrationAction string

const (
	OrchestrationActionUnspecified OrchestrationAction = "unspecified"
	OrchestrationActionMigrate     OrchestrationAction = "migrate"
	OrchestrationActionProvision   OrchestrationAction = "provision"
	OrchestrationActionScaleUp     OrchestrationAction = "scale_up"
	OrchestrationActionScaleDown   OrchestrationAction = "scale_down"
	OrchestrationActionRebalance   OrchestrationAction = "rebalance"
	OrchestrationActionOptimize    OrchestrationAction = "optimize"
)

// OrchestrationDecision for internal use
type OrchestrationDecision string

const (
	OrchDecisionNoAction OrchestrationDecision = "no_action"
	OrchDecisionMigrate  OrchestrationDecision = "migrate"
	OrchDecisionScale    OrchestrationDecision = "scale"
	OrchDecisionOptimize OrchestrationDecision = "optimize"
)

type PolicyPriority string

const (
	PolicyPriorityUnspecified PolicyPriority = "unspecified"
	PolicyPriorityLow         PolicyPriority = "low"
	PolicyPriorityMedium      PolicyPriority = "medium"
	PolicyPriorityHigh        PolicyPriority = "high"
	PolicyPriorityCritical    PolicyPriority = "critical"
)

type StorageClass string

const (
	StorageClassUnspecified StorageClass = "unspecified"
	StorageClassStandard    StorageClass = "standard"   // HDD
	StorageClassFast        StorageClass = "fast"       // SSD
	StorageClassUltraFast   StorageClass = "ultra_fast" // NVMe
	StorageClassCSD         StorageClass = "csd"        // Computational Storage
	StorageClassMemory      StorageClass = "memory"     // Memory-based
)

// ============================================
// 1. WorkloadSignature: Insight Trace -> APOLLO
// ============================================

type WorkloadSignature struct {
	// Identification
	PodName       string `json:"pod_name"`
	PodNamespace  string `json:"pod_namespace"`
	PodUID        string `json:"pod_uid,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
	ContainerName string `json:"container_name,omitempty"`

	// Detected characteristics
	WorkloadType WorkloadType  `json:"workload_type"`
	CurrentStage PipelineStage `json:"current_stage"`
	IOPattern    IOPattern     `json:"io_pattern"`
	Confidence   float64       `json:"confidence"` // 0.0 ~ 1.0

	// Framework detection
	Framework        string `json:"framework,omitempty"` // tensorflow, pytorch
	FrameworkVersion string `json:"framework_version,omitempty"`

	// Resource profile
	IsGPUWorkload      bool `json:"is_gpu_workload"`
	IsDistributed      bool `json:"is_distributed"`
	EstimatedBatchSize int  `json:"estimated_batch_size,omitempty"`

	// Current metrics
	CurrentMetrics *ResourceMetrics `json:"current_metrics,omitempty"`

	// Storage recommendations
	StorageRecommendation *StorageRecommendation `json:"storage_recommendation,omitempty"`

	// Argo Workflows context
	ArgoContext *ArgoContext `json:"argo_context,omitempty"`

	// Timestamps
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ============================================
// 2. ClusterInsight: Insight Scope -> APOLLO
// ============================================

type ClusterInsight struct {
	// Node information
	NodeName string `json:"node_name"`
	NodeUID  string `json:"node_uid,omitempty"`

	// Node resource status
	NodeResources *NodeResources `json:"node_resources,omitempty"`

	// Storage status
	StorageDevices []StorageDeviceStatus `json:"storage_devices,omitempty"`

	// Running workloads on this node
	RunningWorkloads []WorkloadSummary `json:"running_workloads,omitempty"`

	// CSD status
	CSDDevices []CSDStatus `json:"csd_devices,omitempty"`

	// GPU status
	GPUDevices []GPUStatus `json:"gpu_devices,omitempty"`

	// Network status
	NetworkStatus *NetworkStatus `json:"network_status,omitempty"`

	// I/O statistics
	IOStatistics *IOStatistics `json:"io_statistics,omitempty"`

	// Timestamp
	CollectedAt time.Time `json:"collected_at"`
}

// ============================================
// 3. SchedulingPolicy: APOLLO -> Scheduler
// ============================================

type SchedulingPolicy struct {
	// Request identification
	RequestID    string `json:"request_id"`
	PodName      string `json:"pod_name"`
	PodNamespace string `json:"pod_namespace"`

	// Scheduling decision
	Decision SchedulingDecision `json:"decision"`

	// Node preferences
	NodePreferences []NodePreference `json:"node_preferences,omitempty"`

	// Resource requirements
	ResourceRequirements *ComputedResourceRequirements `json:"resource_requirements,omitempty"`

	// Storage requirements
	StorageRequirements *StorageRequirements `json:"storage_requirements,omitempty"`

	// Constraints
	Constraints []SchedulingConstraint `json:"constraints,omitempty"`

	// Priority
	Priority        int  `json:"priority"`
	AllowPreemption bool `json:"allow_preemption"`

	// Affinity rules
	AffinityRules []AffinityRule `json:"affinity_rules,omitempty"`

	// Reasoning
	Reason string `json:"reason,omitempty"`

	// Timestamps
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ============================================
// 4. OrchestrationPolicy: APOLLO -> Orchestrator
// ============================================

type OrchestrationPolicy struct {
	// Request identification
	RequestID    string `json:"request_id"`
	PodName      string `json:"pod_name"`
	PodNamespace string `json:"pod_namespace"`

	// Decision (internal use)
	Decision OrchestrationDecision `json:"decision"`

	// Action type
	Action OrchestrationAction `json:"action"`

	// Target node for migration
	TargetNode string `json:"target_node,omitempty"`

	// Target (one of these will be set)
	MigrationTarget    *MigrationTarget    `json:"migration_target,omitempty"`
	ProvisioningTarget *ProvisioningTarget `json:"provisioning_target,omitempty"`
	AutoscalingTarget  *AutoscalingTarget  `json:"autoscaling_target,omitempty"`

	// Migration constraints
	MigrationConstraints *MigrationConstraints `json:"migration_constraints,omitempty"`

	// Priority (int for internal use)
	Priority    int            `json:"priority"`
	PolicyPrio  PolicyPriority `json:"policy_priority"`

	// Scale target
	ScaleTarget int `json:"scale_target,omitempty"`

	// Execution constraints
	Constraints *ExecutionConstraints `json:"constraints,omitempty"`

	// Reasoning
	Reason              string   `json:"reason,omitempty"`
	ContributingFactors []string `json:"contributing_factors,omitempty"`

	// Timestamps
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Deadline  *time.Time `json:"deadline,omitempty"`
}

// MigrationConstraints defines constraints for pod migration
type MigrationConstraints struct {
	MaxDowntimeSeconds int  `json:"max_downtime_seconds"`
	PreservePV         bool `json:"preserve_pv"`
	TimeoutMinutes     int  `json:"timeout_minutes"`
	GracePeriodSeconds int  `json:"grace_period_seconds"`
	CheckpointEnabled  bool `json:"checkpoint_enabled"`
	LiveMigration      bool `json:"live_migration"`
	RetryOnFailure     bool `json:"retry_on_failure"`
	MaxRetries         int  `json:"max_retries"`
}

// ============================================
// 5. ResourcePrediction: Forecaster
// ============================================

type ResourcePrediction struct {
	// Target
	NodeName     string `json:"node_name"`
	PredictionID string `json:"prediction_id"`

	// Timeframe
	PredictionTime           time.Time `json:"prediction_time"`
	PredictionStart          time.Time `json:"prediction_start"`
	PredictionEnd            time.Time `json:"prediction_end"`
	HorizonMinutes           int       `json:"horizon_minutes"`
	PredictionHorizonMinutes int       `json:"prediction_horizon_minutes"`

	// Simple predicted values (percentage 0-100)
	PredictedCPU      float64 `json:"predicted_cpu"`
	PredictedMemory   float64 `json:"predicted_memory"`
	PredictedIO       float64 `json:"predicted_io"`
	PredictedGPU      float64 `json:"predicted_gpu"`
	PredictedOverload bool    `json:"predicted_overload"`

	// Predicted usage time series
	PredictedUsage []TimestampedResources `json:"predicted_usage,omitempty"`

	// Confidence intervals
	Confidence      float64                `json:"confidence"`
	ConfidenceLevel float64                `json:"confidence_level"`
	UpperBound      []TimestampedResources `json:"upper_bound,omitempty"`
	LowerBound      []TimestampedResources `json:"lower_bound,omitempty"`

	// Anomaly detection
	AnomalyDetected bool    `json:"anomaly_detected"`
	AnomalyType     string  `json:"anomaly_type,omitempty"`
	AnomalyScore    float64 `json:"anomaly_score,omitempty"`

	// Recommendations
	Recommendations []PredictionRecommendation `json:"recommendations,omitempty"`

	// Model metadata
	ModelType     string  `json:"model_type,omitempty"`
	ModelAccuracy float64 `json:"model_accuracy,omitempty"`

	// Timestamp
	CreatedAt time.Time `json:"created_at"`
}

// ============================================
// Sub-types
// ============================================

type ResourceMetrics struct {
	// CPU
	CPUUsagePercent float64 `json:"cpu_usage_percent"`
	CPURequestCores float64 `json:"cpu_request_cores,omitempty"`
	CPULimitCores   float64 `json:"cpu_limit_cores,omitempty"`

	// Memory
	MemoryUsagePercent float64 `json:"memory_usage_percent"`
	MemoryUsageBytes   int64   `json:"memory_usage_bytes"`
	MemoryRequestBytes int64   `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes   int64   `json:"memory_limit_bytes,omitempty"`

	// Disk I/O
	DiskReadBytes         int64   `json:"disk_read_bytes"`
	DiskWriteBytes        int64   `json:"disk_write_bytes"`
	DiskReadIOPS          int64   `json:"disk_read_iops,omitempty"`
	DiskWriteIOPS         int64   `json:"disk_write_iops,omitempty"`
	DiskReadThroughputMBPS  float64 `json:"disk_read_throughput_mbps,omitempty"`
	DiskWriteThroughputMBPS float64 `json:"disk_write_throughput_mbps,omitempty"`

	// Network I/O
	NetworkRxBytes         int64   `json:"network_rx_bytes"`
	NetworkTxBytes         int64   `json:"network_tx_bytes"`
	NetworkRxThroughputMBPS float64 `json:"network_rx_throughput_mbps,omitempty"`
	NetworkTxThroughputMBPS float64 `json:"network_tx_throughput_mbps,omitempty"`

	// GPU
	GPUUsagePercent    float64 `json:"gpu_usage_percent,omitempty"`
	GPUMemoryUsedBytes int64   `json:"gpu_memory_used_bytes,omitempty"`
	GPUMemoryTotalBytes int64  `json:"gpu_memory_total_bytes,omitempty"`

	// Timestamp
	CollectedAt time.Time `json:"collected_at"`
}

type StorageRecommendation struct {
	RecommendedClass       StorageClass `json:"recommended_class"`
	RecommendedSize        string       `json:"recommended_size"` // e.g., "10Gi"
	RecommendedIOPS        int64        `json:"recommended_iops"`
	RecommendedThroughputMBPS int64     `json:"recommended_throughput_mbps"`
	Reason                 string       `json:"reason,omitempty"`
}

type ArgoContext struct {
	WorkflowName      string            `json:"workflow_name"`
	WorkflowNamespace string            `json:"workflow_namespace"`
	WorkflowUID       string            `json:"workflow_uid,omitempty"`
	StepName          string            `json:"step_name,omitempty"`
	TemplateName      string            `json:"template_name,omitempty"`
	Dependencies      []string          `json:"dependencies,omitempty"`
	NextSteps         []string          `json:"next_steps,omitempty"`
	IsDAGTask         bool              `json:"is_dag_task"`
	Phase             string            `json:"phase,omitempty"` // Running, Succeeded, Failed
	InputArtifacts    []ArgoArtifact    `json:"input_artifacts,omitempty"`
	OutputArtifacts   []ArgoArtifact    `json:"output_artifacts,omitempty"`
	InputParameters   map[string]string `json:"input_parameters,omitempty"`
}

type ArgoArtifact struct {
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	S3Key string `json:"s3_key,omitempty"`
	From  string `json:"from,omitempty"`
}

type NodeResources struct {
	// Total capacity
	CPUTotalCores      float64 `json:"cpu_total_cores"`
	MemoryTotalBytes   int64   `json:"memory_total_bytes"`
	StorageTotalBytes  int64   `json:"storage_total_bytes"`
	GPUTotal           int     `json:"gpu_total"`

	// Allocatable
	CPUAllocatableCores     float64 `json:"cpu_allocatable_cores"`
	MemoryAllocatableBytes  int64   `json:"memory_allocatable_bytes"`
	StorageAllocatableBytes int64   `json:"storage_allocatable_bytes"`
	GPUAllocatable          int     `json:"gpu_allocatable"`

	// Used
	CPUUsedCores     float64 `json:"cpu_used_cores"`
	MemoryUsedBytes  int64   `json:"memory_used_bytes"`
	StorageUsedBytes int64   `json:"storage_used_bytes"`
	GPUUsed          int     `json:"gpu_used"`

	// Available
	CPUAvailableCores     float64 `json:"cpu_available_cores"`
	MemoryAvailableBytes  int64   `json:"memory_available_bytes"`
	StorageAvailableBytes int64   `json:"storage_available_bytes"`
	GPUAvailable          int     `json:"gpu_available"`
}

type StorageDeviceStatus struct {
	DeviceName           string  `json:"device_name"`
	DevicePath           string  `json:"device_path"` // e.g., /dev/nvme0n1
	DeviceType           string  `json:"device_type"` // hdd, ssd, nvme, csd
	TotalBytes           int64   `json:"total_bytes"`
	UsedBytes            int64   `json:"used_bytes"`
	AvailableBytes       int64   `json:"available_bytes"`
	UtilizationPercent   float64 `json:"utilization_percent"`
	HealthScore          float64 `json:"health_score"` // 0.0 ~ 1.0
	CurrentIOPS          int64   `json:"current_iops"`
	MaxIOPS              int64   `json:"max_iops"`
	CurrentThroughputMBPS float64 `json:"current_throughput_mbps"`
	MaxThroughputMBPS     float64 `json:"max_throughput_mbps"`
}

type CSDStatus struct {
	DeviceID            string   `json:"device_id"`
	DeviceName          string   `json:"device_name"`
	IsAvailable         bool     `json:"is_available"`
	ComputeUtilization  float64  `json:"compute_utilization"`
	MemoryUsedBytes     int64    `json:"memory_used_bytes"`
	MemoryTotalBytes    int64    `json:"memory_total_bytes"`
	SupportedOperations []string `json:"supported_operations,omitempty"`
	HealthScore         float64  `json:"health_score"`
}

type GPUStatus struct {
	GPUID              string  `json:"gpu_id"`
	GPUModel           string  `json:"gpu_model"`
	UtilizationPercent float64 `json:"utilization_percent"`
	MemoryUsedBytes    int64   `json:"memory_used_bytes"`
	MemoryTotalBytes   int64   `json:"memory_total_bytes"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
	PowerUsageWatts    float64 `json:"power_usage_watts"`
	IsAvailable        bool    `json:"is_available"`
}

type NetworkStatus struct {
	BandwidthMBPS       float64 `json:"bandwidth_mbps"`
	CurrentUtilization  float64 `json:"current_utilization"`
	LatencyMS           float64 `json:"latency_ms"`
	PacketsRx           int64   `json:"packets_rx"`
	PacketsTx           int64   `json:"packets_tx"`
	Errors              int64   `json:"errors"`
}

type IOStatistics struct {
	TotalReadBytes     int64   `json:"total_read_bytes"`
	TotalWriteBytes    int64   `json:"total_write_bytes"`
	TotalReadOps       int64   `json:"total_read_ops"`
	TotalWriteOps      int64   `json:"total_write_ops"`
	AvgReadLatencyMS   float64 `json:"avg_read_latency_ms"`
	AvgWriteLatencyMS  float64 `json:"avg_write_latency_ms"`
	P99ReadLatencyMS   float64 `json:"p99_read_latency_ms"`
	P99WriteLatencyMS  float64 `json:"p99_write_latency_ms"`
}

type WorkloadSummary struct {
	PodName      string        `json:"pod_name"`
	PodNamespace string        `json:"pod_namespace"`
	WorkloadType WorkloadType  `json:"workload_type"`
	CurrentStage PipelineStage `json:"current_stage"`
	CPUUsage     float64       `json:"cpu_usage"`
	MemoryUsage  float64       `json:"memory_usage"`
	IOUsage      float64       `json:"io_usage"`
}

type NodePreference struct {
	NodeName string `json:"node_name"`
	Score    int    `json:"score"` // 0-100
	Reason   string `json:"reason,omitempty"`
}

type ComputedResourceRequirements struct {
	CPUCores              float64 `json:"cpu_cores"`
	MemoryBytes           int64   `json:"memory_bytes"`
	StorageBytes          int64   `json:"storage_bytes"`
	GPUCount              int     `json:"gpu_count"`
	ExpectedIOPS          int64   `json:"expected_iops"`
	ExpectedThroughputMBPS int64  `json:"expected_throughput_mbps"`
}

type StorageRequirements struct {
	StorageClass      StorageClass `json:"storage_class"`
	SizeBytes         int64        `json:"size_bytes"`
	MinIOPS           int64        `json:"min_iops"`
	MinThroughputMBPS int64        `json:"min_throughput_mbps"`
	ExpectedIOPattern IOPattern    `json:"expected_io_pattern"`
	RequiresCSD       bool         `json:"requires_csd"`
}

type SchedulingConstraint struct {
	ConstraintType string   `json:"constraint_type"` // node_selector, toleration, affinity
	Key            string   `json:"key"`
	Operator       string   `json:"operator"`
	Values         []string `json:"values,omitempty"`
}

type AffinityRule struct {
	RuleType     string   `json:"rule_type"` // pod_affinity, pod_anti_affinity, node_affinity
	TopologyKey  string   `json:"topology_key,omitempty"`
	TargetLabels []string `json:"target_labels,omitempty"`
	IsRequired   bool     `json:"is_required"`
	Weight       int      `json:"weight,omitempty"`
}

type MigrationTarget struct {
	PodName        string `json:"pod_name"`
	PodNamespace   string `json:"pod_namespace"`
	SourceNode     string `json:"source_node"`
	TargetNode     string `json:"target_node"`
	PreservePV     bool   `json:"preserve_pv"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type ProvisioningTarget struct {
	PVCName       string       `json:"pvc_name"`
	Namespace     string       `json:"namespace"`
	StorageClass  StorageClass `json:"storage_class"`
	SizeBytes     int64        `json:"size_bytes"`
	IOPS          int64        `json:"iops"`
	ThroughputMBPS int64       `json:"throughput_mbps"`
	AccessMode    string       `json:"access_mode"` // ReadWriteOnce, ReadWriteMany
}

type AutoscalingTarget struct {
	WorkloadName      string `json:"workload_name"`
	WorkloadNamespace string `json:"workload_namespace"`
	WorkloadKind      string `json:"workload_kind"` // Deployment, StatefulSet
	CurrentReplicas   int    `json:"current_replicas"`
	DesiredReplicas   int    `json:"desired_replicas"`
	ScalingReason     string `json:"scaling_reason,omitempty"`
}

type ExecutionConstraints struct {
	MaxRetries       int      `json:"max_retries"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	AllowDisruption  bool     `json:"allow_disruption"`
	BlackoutWindows  []string `json:"blackout_windows,omitempty"` // e.g., "09:00-17:00"
}

type TimestampedResources struct {
	Timestamp    time.Time `json:"timestamp"`
	CPUUsage     float64   `json:"cpu_usage"`
	MemoryUsage  float64   `json:"memory_usage"`
	IOUsage      float64   `json:"io_usage"`
	NetworkUsage float64   `json:"network_usage"`
}

type PredictionRecommendation struct {
	RecommendationType    string         `json:"recommendation_type"` // scale, migrate, provision
	Description           string         `json:"description"`
	Priority              PolicyPriority `json:"priority"`
	RecommendedActionTime *time.Time     `json:"recommended_action_time,omitempty"`
}
