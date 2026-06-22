// ============================================
// APOLLO SchedulingPolicyService Server
// AI-Storage Scheduler에 스케줄링 정책 제공
// ============================================

package scheduling

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"apollo/pkg/config"
	"apollo/pkg/insight"
	"apollo/pkg/types"
)

// SchedulingPolicyService provides scheduling policies to AI-Storage Scheduler
type SchedulingPolicyService struct {
	// Reference to insight service for data access
	insightService *insight.InsightService

	// Configuration (설정값)
	config *config.SchedulingConfig

	// Policy cache
	policyCache map[string]*types.SchedulingPolicy // key: pod_namespace/pod_name
	cacheMux    sync.RWMutex

	// Policy generation function (can be customized)
	policyGenerator func(sig *types.WorkloadSignature, insights map[string]*types.ClusterInsight) *types.SchedulingPolicy

	// Statistics
	stats SchedulingStats
}

// SchedulingStats tracks scheduling service statistics
type SchedulingStats struct {
	TotalPolicyRequests int64
	TotalResultReports  int64
	SuccessfulSchedules int64
	FailedSchedules     int64
}

// NewSchedulingPolicyService creates a new SchedulingPolicyService
func NewSchedulingPolicyService(insightService *insight.InsightService) *SchedulingPolicyService {
	return NewSchedulingPolicyServiceWithConfig(insightService, nil)
}

// NewSchedulingPolicyServiceWithConfig creates a new SchedulingPolicyService with custom config
func NewSchedulingPolicyServiceWithConfig(insightService *insight.InsightService, cfg *config.SchedulingConfig) *SchedulingPolicyService {
	if cfg == nil {
		cfg = config.LoadSchedulingConfigFromEnv()
	}

	svc := &SchedulingPolicyService{
		insightService: insightService,
		config:         cfg,
		policyCache:    make(map[string]*types.SchedulingPolicy),
	}

	// Set default policy generator
	svc.policyGenerator = svc.defaultPolicyGenerator

	log.Printf("[SchedulingPolicy] Initialized with config: PreferThreshold=%d, GPUBonus=%d, CSDBonus=%d",
		cfg.PreferThreshold, cfg.GPUBonusScore, cfg.CSDBonusScore)

	return svc
}

// GetConfig returns current scheduling configuration
func (s *SchedulingPolicyService) GetConfig() *config.SchedulingConfig {
	return s.config
}

// SetPolicyGenerator sets custom policy generation function
func (s *SchedulingPolicyService) SetPolicyGenerator(gen func(*types.WorkloadSignature, map[string]*types.ClusterInsight) *types.SchedulingPolicy) {
	s.policyGenerator = gen
}

// GetSchedulingPolicy returns scheduling policy for a pod
func (s *SchedulingPolicyService) GetSchedulingPolicy(ctx context.Context, req *SchedulingPolicyRequest) (*types.SchedulingPolicy, error) {
	s.stats.TotalPolicyRequests++

	key := req.PodNamespace + "/" + req.PodName

	log.Printf("[SchedulingPolicy] Policy requested for pod: %s", key)

	// Check cache first
	s.cacheMux.RLock()
	if cached, ok := s.policyCache[key]; ok {
		if cached.ExpiresAt == nil || time.Now().Before(*cached.ExpiresAt) {
			s.cacheMux.RUnlock()
			log.Printf("[SchedulingPolicy] Returning cached policy for: %s", key)
			return cached, nil
		}
	}
	s.cacheMux.RUnlock()

	// Get workload signature from insight service
	sig := s.insightService.GetWorkloadSignature(req.PodNamespace, req.PodName)

	// Get all cluster insights
	insights := s.insightService.GetAllClusterInsights()

	// Generate policy
	policy := s.policyGenerator(sig, insights)
	policy.RequestID = fmt.Sprintf("sched-%d", time.Now().UnixNano())
	policy.PodName = req.PodName
	policy.PodNamespace = req.PodNamespace
	policy.CreatedAt = time.Now()

	// Set expiration (5 minutes)
	expiry := time.Now().Add(5 * time.Minute)
	policy.ExpiresAt = &expiry

	// Cache the policy
	s.cacheMux.Lock()
	s.policyCache[key] = policy
	s.cacheMux.Unlock()

	log.Printf("[SchedulingPolicy] Generated policy for %s: decision=%s, nodes=%d",
		key, policy.Decision, len(policy.NodePreferences))

	return policy, nil
}

// ReportSchedulingResult receives scheduling result feedback and calculates reward
func (s *SchedulingPolicyService) ReportSchedulingResult(ctx context.Context, result *SchedulingResult) (*ReportResponse, error) {
	s.stats.TotalResultReports++

	key := result.PodNamespace + "/" + result.PodName

	// Get cached policy and workload signature for reward calculation
	s.cacheMux.RLock()
	cachedPolicy := s.policyCache[key]
	s.cacheMux.RUnlock()

	// Get workload signature and cluster insights for reward calculation
	sig := s.insightService.GetWorkloadSignature(result.PodNamespace, result.PodName)
	insights := s.insightService.GetAllClusterInsights()
	nodeInsight := insights[result.ScheduledNode]

	// Calculate reward based on actual metrics
	reward := s.calculateMetricsBasedReward(result, cachedPolicy, sig, nodeInsight)

	if result.Success {
		s.stats.SuccessfulSchedules++
		log.Printf("[SchedulingPolicy] Scheduling succeeded: pod=%s/%s -> node=%s (duration=%dms, reward=%.3f)",
			result.PodNamespace, result.PodName, result.ScheduledNode, result.SchedulingDurationMs, reward)
	} else {
		s.stats.FailedSchedules++
		log.Printf("[SchedulingPolicy] Scheduling failed: pod=%s/%s reason=%s (reward=%.3f)",
			result.PodNamespace, result.PodName, result.FailureReason, reward)
	}

	// Remove from cache after scheduling
	s.cacheMux.Lock()
	delete(s.policyCache, key)
	s.cacheMux.Unlock()

	return &ReportResponse{
		Success:   true,
		Message:   fmt.Sprintf("result recorded, reward=%.3f", reward),
		RequestID: result.RequestID,
	}, nil
}

// calculateMetricsBasedReward calculates reward based on actual insight-scope metrics
func (s *SchedulingPolicyService) calculateMetricsBasedReward(
	result *SchedulingResult,
	cachedPolicy *types.SchedulingPolicy,
	sig *types.WorkloadSignature,
	nodeInsight *types.ClusterInsight,
) float64 {
	// Base reward for successful/failed scheduling
	if !result.Success {
		return -1.0 // Penalty for failed scheduling
	}

	reward := 0.5 // Base success reward

	// 1. Node preference match (0 ~ 0.3)
	// Check if scheduled node was in APOLLO's recommended nodes
	if cachedPolicy != nil {
		for i, pref := range cachedPolicy.NodePreferences {
			if pref.NodeName == result.ScheduledNode {
				// Higher reward for higher-ranked preference
				preferenceBonus := 0.3 * float64(len(cachedPolicy.NodePreferences)-i) / float64(len(cachedPolicy.NodePreferences))
				reward += preferenceBonus
				log.Printf("[Reward] Node preference match: node=%s, rank=%d, bonus=%.3f",
					result.ScheduledNode, i+1, preferenceBonus)
				break
			}
		}
	}

	// 2. Resource utilization match (0 ~ 0.3)
	// Reward for scheduling to nodes with available resources (from insight-scope)
	if nodeInsight != nil && nodeInsight.NodeResources != nil {
		res := nodeInsight.NodeResources

		// CPU availability reward
		cpuAvailRatio := res.CPUAvailableCores / res.CPUAllocatableCores
		if cpuAvailRatio > 0.3 {
			reward += 0.1 * cpuAvailRatio
		}

		// Memory availability reward
		memAvailRatio := float64(res.MemoryAvailableBytes) / float64(res.MemoryAllocatableBytes)
		if memAvailRatio > 0.3 {
			reward += 0.1 * memAvailRatio
		}

		// GPU availability for GPU workloads
		if sig != nil && sig.IsGPUWorkload && res.GPUAvailable > 0 {
			reward += 0.1
			log.Printf("[Reward] GPU available on node: bonus=0.1")
		}

		log.Printf("[Reward] Resource availability: CPU=%.2f%%, Memory=%.2f%%, bonus=%.3f",
			cpuAvailRatio*100, memAvailRatio*100, 0.1*cpuAvailRatio+0.1*memAvailRatio)
	}

	// 3. Storage performance match (0 ~ 0.2)
	// Check if node's storage matches workload I/O requirements
	if nodeInsight != nil && sig != nil && cachedPolicy != nil && cachedPolicy.StorageRequirements != nil {
		storageReward := s.calculateStorageReward(nodeInsight, cachedPolicy.StorageRequirements)
		reward += storageReward
		log.Printf("[Reward] Storage performance: bonus=%.3f", storageReward)
	}

	// 4. CSD availability for I/O intensive workloads (0 ~ 0.1)
	if nodeInsight != nil && sig != nil {
		if sig.IOPattern == types.IOPatternReadHeavy || sig.IOPattern == types.IOPatternWriteHeavy {
			for _, csd := range nodeInsight.CSDDevices {
				if csd.IsAvailable && csd.ComputeUtilization < 0.7 {
					reward += 0.1
					log.Printf("[Reward] CSD available for I/O workload: bonus=0.1")
					break
				}
			}
		}
	}

	// 5. Scheduling latency bonus/penalty (-0.1 ~ 0.1)
	if result.SchedulingDurationMs < 50 {
		reward += 0.1 // Fast scheduling
	} else if result.SchedulingDurationMs < 100 {
		reward += 0.05 // Good scheduling
	} else if result.SchedulingDurationMs > 500 {
		reward -= 0.1 // Slow scheduling
	}

	// Clamp reward to [-1, 2]
	if reward < -1.0 {
		reward = -1.0
	} else if reward > 2.0 {
		reward = 2.0
	}

	return reward
}

// calculateStorageReward calculates reward based on storage performance match
func (s *SchedulingPolicyService) calculateStorageReward(
	insight *types.ClusterInsight,
	requirements *types.StorageRequirements,
) float64 {
	if len(insight.StorageDevices) == 0 {
		return 0.0
	}

	reward := 0.0

	// Find best matching storage device
	for _, device := range insight.StorageDevices {
		// Check storage class match
		classMatch := false
		switch requirements.StorageClass {
		case types.StorageClassUltraFast:
			classMatch = device.DeviceType == "nvme"
		case types.StorageClassFast:
			classMatch = device.DeviceType == "ssd" || device.DeviceType == "nvme"
		case types.StorageClassCSD:
			classMatch = device.DeviceType == "csd"
		default:
			classMatch = true
		}

		if classMatch {
			// IOPS match
			if device.MaxIOPS >= requirements.MinIOPS {
				reward += 0.1
			}

			// Throughput match
			if device.MaxThroughputMBPS >= float64(requirements.MinThroughputMBPS) {
				reward += 0.1
			}

			break // Use first matching device
		}
	}

	return reward
}

// defaultPolicyGenerator generates scheduling policy based on workload signature
func (s *SchedulingPolicyService) defaultPolicyGenerator(sig *types.WorkloadSignature, insights map[string]*types.ClusterInsight) *types.SchedulingPolicy {
	policy := &types.SchedulingPolicy{
		Decision:        types.SchedulingDecisionAllow,
		NodePreferences: []types.NodePreference{},
		Priority:        s.config.DefaultPriority,
	}

	// If no signature, allow scheduling to any node with default weights
	if sig == nil {
		policy.Reason = "no workload signature available, using default policy"
		policy.PluginWeights = s.computeDefaultPluginWeights()
		return policy
	}

	// Compute plugin weights based on workload signature
	policy.PluginWeights = s.computePluginWeights(sig)

	// Analyze workload and generate preferences
	for nodeName, insight := range insights {
		score := s.calculateNodeScore(sig, insight)
		if score > 0 {
			policy.NodePreferences = append(policy.NodePreferences, types.NodePreference{
				NodeName: nodeName,
				Score:    score,
				Reason:   s.generateScoreReason(sig, insight, score),
			})
		}
	}

	// Set storage requirements based on I/O pattern
	policy.StorageRequirements = s.computeStorageRequirements(sig)

	// Set resource requirements
	policy.ResourceRequirements = s.computeResourceRequirements(sig)

	// Generate affinity rules for Argo workflows
	if sig.ArgoContext != nil {
		policy.AffinityRules = s.generateArgoAffinityRules(sig)
	}

	// Set decision based on analysis
	if len(policy.NodePreferences) == 0 {
		policy.Decision = types.SchedulingDecisionAllow
		policy.Reason = "no specific node preferences, allow scheduling to any node"
	} else if policy.NodePreferences[0].Score >= s.config.PreferThreshold {
		policy.Decision = types.SchedulingDecisionPrefer
		policy.Reason = fmt.Sprintf("strongly prefer node %s (score=%d, threshold=%d)",
			policy.NodePreferences[0].NodeName, policy.NodePreferences[0].Score, s.config.PreferThreshold)
	} else {
		policy.Decision = types.SchedulingDecisionAllow
		policy.Reason = "allow scheduling with preferences"
	}

	log.Printf("[SchedulingPolicy] PluginWeights: DLA=%.2f, STA=%.2f, IOPB=%.2f, KA=%.2f, PSA=%.2f",
		policy.PluginWeights.DataLocalityAware, policy.PluginWeights.StorageTierAware,
		policy.PluginWeights.IOPatternBased, policy.PluginWeights.KueueAware,
		policy.PluginWeights.PipelineStageAware)

	return policy
}

// computeDefaultPluginWeights returns default weights when no signature is available
func (s *SchedulingPolicyService) computeDefaultPluginWeights() *types.PluginWeights {
	return &types.PluginWeights{
		DataLocalityAware:  0.5,
		StorageTierAware:   0.5,
		IOPatternBased:     0.5,
		KueueAware:         0.5,
		PipelineStageAware: 0.5,
	}
}

// computePluginWeights calculates dynamic plugin weights based on workload characteristics
func (s *SchedulingPolicyService) computePluginWeights(sig *types.WorkloadSignature) *types.PluginWeights {
	weights := &types.PluginWeights{
		DataLocalityAware:  0.5, // Default
		StorageTierAware:   0.5,
		IOPatternBased:     0.5,
		KueueAware:         0.5,
		PipelineStageAware: 0.5,
	}

	// Adjust weights based on I/O pattern
	switch sig.IOPattern {
	case types.IOPatternReadHeavy, types.IOPatternWriteHeavy:
		// I/O intensive workloads benefit from good storage placement
		weights.DataLocalityAware = 0.8
		weights.StorageTierAware = 0.9
		weights.IOPatternBased = 0.9
	case types.IOPatternSequential:
		weights.StorageTierAware = 0.8
		weights.IOPatternBased = 0.7
	case types.IOPatternRandom:
		weights.StorageTierAware = 0.9 // NVMe/SSD important for random I/O
		weights.IOPatternBased = 0.8
	case types.IOPatternBursty:
		weights.StorageTierAware = 0.7
		weights.IOPatternBased = 0.6
	}

	// Adjust weights based on pipeline stage
	switch sig.CurrentStage {
	case types.PipelineStageDataLoading, types.PipelineStagePreprocessing:
		// Data-intensive stages benefit from locality
		weights.DataLocalityAware = 0.9
		weights.PipelineStageAware = 0.8
	case types.PipelineStageTraining:
		// Training benefits from GPU and balanced storage
		weights.PipelineStageAware = 0.7
		if sig.IsGPUWorkload {
			weights.StorageTierAware = 0.6 // GPU matters more than storage
		}
	case types.PipelineStageInference:
		// Inference is often latency-sensitive
		weights.StorageTierAware = 0.8
		weights.PipelineStageAware = 0.7
	case types.PipelineStageCheckpointing:
		// Checkpointing is write-heavy
		weights.StorageTierAware = 0.9
		weights.IOPatternBased = 0.8
	}

	// Argo workflow context affects scheduling
	if sig.ArgoContext != nil {
		// Workflows with dependencies benefit from locality
		if len(sig.ArgoContext.Dependencies) > 0 {
			weights.DataLocalityAware = 0.9
			weights.PipelineStageAware = 0.8
		}
	}

	// Kueue is relevant for batch workloads
	if sig.WorkloadType == types.WorkloadTypeImage || sig.WorkloadType == types.WorkloadTypeText {
		weights.KueueAware = 0.7
	}

	return weights
}

// calculateNodeScore calculates a score for a node based on workload requirements
func (s *SchedulingPolicyService) calculateNodeScore(sig *types.WorkloadSignature, insight *types.ClusterInsight) int {
	if insight == nil || insight.NodeResources == nil {
		return 0
	}

	score := s.config.BaseNodeScore

	// Check GPU availability for GPU workloads
	if sig.IsGPUWorkload {
		if insight.NodeResources.GPUAvailable > 0 {
			score += s.config.GPUBonusScore
		} else {
			return 0 // No GPU, not suitable
		}
	}

	// Check CSD availability for storage-intensive workloads
	if sig.IOPattern == types.IOPatternReadHeavy || sig.IOPattern == types.IOPatternWriteHeavy {
		for _, csd := range insight.CSDDevices {
			if csd.IsAvailable && csd.ComputeUtilization < s.config.CSDUtilizationThreshold {
				score += s.config.CSDBonusScore
				break
			}
		}
	}

	// Check storage device performance
	for _, storage := range insight.StorageDevices {
		if storage.DeviceType == "nvme" || storage.DeviceType == "ssd" {
			if storage.UtilizationPercent < s.config.StorageUtilizationThreshold {
				score += s.config.FastStorageBonusScore
			}
		}
	}

	// Check resource availability
	if insight.NodeResources.CPUAvailableCores > s.config.CPUAvailableThreshold {
		score += s.config.CPUBonusScore
	}
	if insight.NodeResources.MemoryAvailableBytes > s.config.MemoryAvailableThreshold {
		score += s.config.MemoryBonusScore
	}

	// Cap score at 100
	if score > 100 {
		score = 100
	}

	return score
}

// generateScoreReason generates human-readable reason for the score
func (s *SchedulingPolicyService) generateScoreReason(sig *types.WorkloadSignature, insight *types.ClusterInsight, score int) string {
	reasons := []string{}

	if sig.IsGPUWorkload && insight.NodeResources.GPUAvailable > 0 {
		reasons = append(reasons, fmt.Sprintf("GPU available (%d)", insight.NodeResources.GPUAvailable))
	}

	for _, csd := range insight.CSDDevices {
		if csd.IsAvailable {
			reasons = append(reasons, "CSD available")
			break
		}
	}

	if len(reasons) == 0 {
		return "general compute node"
	}

	return fmt.Sprintf("score=%d: %v", score, reasons)
}

// computeStorageRequirements computes storage requirements from signature
func (s *SchedulingPolicyService) computeStorageRequirements(sig *types.WorkloadSignature) *types.StorageRequirements {
	req := &types.StorageRequirements{
		StorageClass:      types.StorageClassFast, // Default to SSD
		ExpectedIOPattern: sig.IOPattern,
	}

	// Set storage class based on I/O pattern
	switch sig.IOPattern {
	case types.IOPatternReadHeavy:
		req.StorageClass = types.StorageClassUltraFast
		req.MinIOPS = s.config.ReadHeavyIOPS
		req.MinThroughputMBPS = s.config.ReadHeavyThroughput
	case types.IOPatternWriteHeavy:
		req.StorageClass = types.StorageClassUltraFast
		req.MinIOPS = s.config.WriteHeavyIOPS
		req.MinThroughputMBPS = s.config.WriteHeavyThroughput
	case types.IOPatternBursty:
		req.StorageClass = types.StorageClassFast
		req.MinIOPS = s.config.BurstyIOPS
		req.MinThroughputMBPS = s.config.BurstyThroughput
	default:
		req.StorageClass = types.StorageClassStandard
		req.MinIOPS = s.config.DefaultIOPS
		req.MinThroughputMBPS = s.config.DefaultThroughput
	}

	// Use storage recommendation if available
	if sig.StorageRecommendation != nil {
		req.StorageClass = sig.StorageRecommendation.RecommendedClass
		if sig.StorageRecommendation.RecommendedIOPS > req.MinIOPS {
			req.MinIOPS = sig.StorageRecommendation.RecommendedIOPS
		}
		if sig.StorageRecommendation.RecommendedThroughputMBPS > req.MinThroughputMBPS {
			req.MinThroughputMBPS = sig.StorageRecommendation.RecommendedThroughputMBPS
		}
	}

	return req
}

// computeResourceRequirements computes resource requirements from signature
func (s *SchedulingPolicyService) computeResourceRequirements(sig *types.WorkloadSignature) *types.ComputedResourceRequirements {
	req := &types.ComputedResourceRequirements{
		CPUCores:    1,
		MemoryBytes: 2 * 1024 * 1024 * 1024, // 2GB default
	}

	// Adjust based on workload type
	switch sig.WorkloadType {
	case types.WorkloadTypeImage:
		req.CPUCores = 4
		req.MemoryBytes = 8 * 1024 * 1024 * 1024
		if sig.IsGPUWorkload {
			req.GPUCount = 1
		}
	case types.WorkloadTypeText:
		req.CPUCores = 4
		req.MemoryBytes = 16 * 1024 * 1024 * 1024
		if sig.IsGPUWorkload {
			req.GPUCount = 1
		}
	case types.WorkloadTypeVideo:
		req.CPUCores = 8
		req.MemoryBytes = 32 * 1024 * 1024 * 1024
		if sig.IsGPUWorkload {
			req.GPUCount = 2
		}
	}

	// Use current metrics if available
	if sig.CurrentMetrics != nil {
		if sig.CurrentMetrics.CPURequestCores > req.CPUCores {
			req.CPUCores = sig.CurrentMetrics.CPURequestCores
		}
		if sig.CurrentMetrics.MemoryRequestBytes > req.MemoryBytes {
			req.MemoryBytes = sig.CurrentMetrics.MemoryRequestBytes
		}
	}

	return req
}

// generateArgoAffinityRules generates affinity rules for Argo workflow pods
func (s *SchedulingPolicyService) generateArgoAffinityRules(sig *types.WorkloadSignature) []types.AffinityRule {
	rules := []types.AffinityRule{}

	if sig.ArgoContext == nil {
		return rules
	}

	// If there are dependencies, try to schedule on same node for data locality
	if len(sig.ArgoContext.Dependencies) > 0 {
		rules = append(rules, types.AffinityRule{
			RuleType:     "pod_affinity",
			TopologyKey:  "kubernetes.io/hostname",
			TargetLabels: []string{"workflows.argoproj.io/workflow=" + sig.ArgoContext.WorkflowName},
			IsRequired:   false, // Preferred, not required
			Weight:       80,
		})
	}

	return rules
}

// GetStats returns service statistics
func (s *SchedulingPolicyService) GetStats() SchedulingStats {
	return s.stats
}

// ClearCache clears the policy cache
func (s *SchedulingPolicyService) ClearCache() {
	s.cacheMux.Lock()
	s.policyCache = make(map[string]*types.SchedulingPolicy)
	s.cacheMux.Unlock()
}

// SchedulingPolicyRequest represents a request for scheduling policy
type SchedulingPolicyRequest struct {
	PodName        string            `json:"pod_name"`
	PodNamespace   string            `json:"pod_namespace"`
	PodUID         string            `json:"pod_uid,omitempty"`
	PodLabels      map[string]string `json:"pod_labels,omitempty"`
	PodAnnotations map[string]string `json:"pod_annotations,omitempty"`
}

// SchedulingResult represents the result of a scheduling attempt
type SchedulingResult struct {
	RequestID            string `json:"request_id"`
	PodName              string `json:"pod_name"`
	PodNamespace         string `json:"pod_namespace"`
	ScheduledNode        string `json:"scheduled_node,omitempty"`
	Success              bool   `json:"success"`
	FailureReason        string `json:"failure_reason,omitempty"`
	SchedulingDurationMs int64  `json:"scheduling_duration_ms"`
}

// ReportResponse represents a response to a report request
type ReportResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}
