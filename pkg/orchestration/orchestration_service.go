// ============================================
// APOLLO OrchestrationPolicyService Server
// AI-Storage Orchestrator에 마이그레이션 정책 제공
// ============================================

package orchestration

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"apollo/pkg/forecaster"
	"apollo/pkg/insight"
	"apollo/pkg/types"
)

// OrchestrationPolicyService provides orchestration policies to AI-Storage Orchestrator
type OrchestrationPolicyService struct {
	// Reference to insight service for data access
	insightService *insight.InsightService

	// Reference to forecast service for predictions
	forecastService *forecaster.ForecastService

	// Active migration tracking
	activeMigrations map[string]*types.OrchestrationPolicy // key: request_id
	migrationMux     sync.RWMutex

	// Policy generation configuration
	config OrchestrationConfig

	// Statistics
	stats OrchestrationStats
}

// OrchestrationConfig holds configuration for orchestration policies
type OrchestrationConfig struct {
	// Thresholds for triggering migrations
	CPUUtilizationThreshold    float64 // Trigger migration if above this (0-100)
	MemoryUtilizationThreshold float64 // Trigger migration if above this (0-100)
	IOUtilizationThreshold     float64 // Trigger migration if above this (0-100)

	// Cooldown periods
	MigrationCooldownMinutes int // Minimum time between migrations for same pod

	// Resource prediction settings
	UsePredictions       bool // Use forecasted values for decisions
	PredictionHorizonMin int  // How far ahead to predict

	// CSD-specific settings
	PreferCSDNodes bool // Prefer nodes with CSD for storage-intensive workloads
}

// OrchestrationStats tracks orchestration service statistics
type OrchestrationStats struct {
	TotalPolicyRequests     int64
	TotalMigrationDecisions int64
	TotalScaleDecisions     int64
	TotalStatusUpdates      int64
}

// NewOrchestrationPolicyService creates a new OrchestrationPolicyService
func NewOrchestrationPolicyService(insightService *insight.InsightService, forecastService *forecaster.ForecastService) *OrchestrationPolicyService {
	return &OrchestrationPolicyService{
		insightService:   insightService,
		forecastService:  forecastService,
		activeMigrations: make(map[string]*types.OrchestrationPolicy),
		config: OrchestrationConfig{
			CPUUtilizationThreshold:    80.0,
			MemoryUtilizationThreshold: 85.0,
			IOUtilizationThreshold:     90.0,
			MigrationCooldownMinutes:   5,
			UsePredictions:             true,
			PredictionHorizonMin:       10,
			PreferCSDNodes:             true,
		},
	}
}

// SetConfig updates the orchestration configuration
func (s *OrchestrationPolicyService) SetConfig(config OrchestrationConfig) {
	s.config = config
}

// GetOrchestrationPolicy returns orchestration policy for a pod
func (s *OrchestrationPolicyService) GetOrchestrationPolicy(ctx context.Context, req *OrchestrationPolicyRequest) (*types.OrchestrationPolicy, error) {
	s.stats.TotalPolicyRequests++

	key := req.PodNamespace + "/" + req.PodName
	log.Printf("[OrchestrationPolicy] Policy requested for pod: %s, current_node: %s", key, req.CurrentNode)

	// Get workload signature
	sig := s.insightService.GetWorkloadSignature(req.PodNamespace, req.PodName)

	// Get cluster insights
	insights := s.insightService.GetAllClusterInsights()

	// Get current node insight
	currentNodeInsight := insights[req.CurrentNode]

	// Generate policy
	policy := s.generateOrchestrationPolicy(sig, currentNodeInsight, insights, req)

	// Track active migration if decision is migrate
	if policy.Decision == types.OrchDecisionMigrate {
		s.migrationMux.Lock()
		s.activeMigrations[policy.RequestID] = policy
		s.migrationMux.Unlock()
		s.stats.TotalMigrationDecisions++
	} else if policy.Decision == types.OrchDecisionScale {
		s.stats.TotalScaleDecisions++
	}

	log.Printf("[OrchestrationPolicy] Generated policy for %s: decision=%s, priority=%d",
		key, policy.Decision, policy.Priority)

	return policy, nil
}

// generateOrchestrationPolicy generates orchestration policy based on current state
func (s *OrchestrationPolicyService) generateOrchestrationPolicy(
	sig *types.WorkloadSignature,
	currentNode *types.ClusterInsight,
	allNodes map[string]*types.ClusterInsight,
	req *OrchestrationPolicyRequest,
) *types.OrchestrationPolicy {
	policy := &types.OrchestrationPolicy{
		RequestID:    fmt.Sprintf("orch-%d", time.Now().UnixNano()),
		PodName:      req.PodName,
		PodNamespace: req.PodNamespace,
		Decision:     types.OrchDecisionNoAction,
		Priority:     50,
		CreatedAt:    time.Now(),
	}

	// Set expiration (10 minutes)
	expiry := time.Now().Add(10 * time.Minute)
	policy.ExpiresAt = &expiry

	// Check if migration is needed
	migrationNeeded, migrationReason := s.checkMigrationNeeded(sig, currentNode, req)

	if migrationNeeded {
		// Find best target node
		targetNode, nodeScore := s.findBestTargetNode(sig, currentNode, allNodes, req.CurrentNode)

		if targetNode != "" && nodeScore > 0 {
			policy.Decision = types.OrchDecisionMigrate
			policy.Reason = migrationReason
			policy.TargetNode = targetNode
			policy.Priority = s.calculateMigrationPriority(sig, currentNode)

			// Generate migration constraints
			policy.MigrationConstraints = s.generateMigrationConstraints(sig, req)
		} else {
			// No suitable target, check if scaling is possible
			if s.checkScalingNeeded(sig, currentNode) {
				policy.Decision = types.OrchDecisionScale
				policy.ScaleTarget = s.calculateScaleTarget(sig, currentNode)
				policy.Reason = "no suitable migration target, recommending scale"
			} else {
				policy.Reason = "migration needed but no suitable target available"
			}
		}
	} else {
		// Check if scaling or optimization is beneficial
		if s.checkScalingNeeded(sig, currentNode) {
			policy.Decision = types.OrchDecisionScale
			policy.ScaleTarget = s.calculateScaleTarget(sig, currentNode)
			policy.Reason = "resource optimization through scaling"
		} else if s.checkOptimizationPossible(sig, currentNode) {
			policy.Decision = types.OrchDecisionOptimize
			policy.Reason = "storage optimization possible"
		}
	}

	return policy
}

// checkMigrationNeeded determines if migration is needed
func (s *OrchestrationPolicyService) checkMigrationNeeded(
	sig *types.WorkloadSignature,
	currentNode *types.ClusterInsight,
	req *OrchestrationPolicyRequest,
) (bool, string) {
	if currentNode == nil {
		return false, ""
	}

	reasons := []string{}

	// Check CPU utilization
	if currentNode.NodeResources != nil {
		cpuUsed := float64(currentNode.NodeResources.CPUUsedCores) / float64(currentNode.NodeResources.CPUTotalCores) * 100
		if cpuUsed > s.config.CPUUtilizationThreshold {
			reasons = append(reasons, fmt.Sprintf("high CPU utilization (%.1f%%)", cpuUsed))
		}

		// Check memory utilization
		memUsed := float64(currentNode.NodeResources.MemoryUsedBytes) / float64(currentNode.NodeResources.MemoryTotalBytes) * 100
		if memUsed > s.config.MemoryUtilizationThreshold {
			reasons = append(reasons, fmt.Sprintf("high memory utilization (%.1f%%)", memUsed))
		}
	}

	// Check storage device utilization
	for _, storage := range currentNode.StorageDevices {
		if storage.UtilizationPercent > s.config.IOUtilizationThreshold {
			reasons = append(reasons, fmt.Sprintf("high I/O on %s (%.1f%%)", storage.DevicePath, storage.UtilizationPercent))
		}
	}

	// Check if workload needs GPU but node has none
	if sig != nil && sig.IsGPUWorkload && currentNode.NodeResources.GPUAvailable == 0 {
		reasons = append(reasons, "workload requires GPU but node has none")
	}

	// Check if workload is storage-intensive but no CSD available
	if sig != nil && (sig.IOPattern == types.IOPatternReadHeavy || sig.IOPattern == types.IOPatternWriteHeavy) {
		hasCSD := false
		for _, csd := range currentNode.CSDDevices {
			if csd.IsAvailable {
				hasCSD = true
				break
			}
		}
		if !hasCSD && s.config.PreferCSDNodes {
			reasons = append(reasons, "storage-intensive workload without CSD")
		}
	}

	// Use predictions if enabled
	if s.config.UsePredictions && s.forecastService != nil {
		prediction := s.forecastService.GetPrediction(req.CurrentNode)
		if prediction != nil && prediction.PredictedOverload {
			reasons = append(reasons, "predicted resource overload")
		}
	}

	if len(reasons) > 0 {
		return true, fmt.Sprintf("migration recommended: %v", reasons)
	}

	return false, ""
}

// findBestTargetNode finds the best node for migration
func (s *OrchestrationPolicyService) findBestTargetNode(
	sig *types.WorkloadSignature,
	currentNode *types.ClusterInsight,
	allNodes map[string]*types.ClusterInsight,
	currentNodeName string,
) (string, int) {
	var bestNode string
	var bestScore int

	for nodeName, insight := range allNodes {
		// Skip current node
		if nodeName == currentNodeName {
			continue
		}

		score := s.calculateNodeSuitability(sig, insight)

		// Apply CSD preference for storage-intensive workloads
		if sig != nil && s.config.PreferCSDNodes {
			if sig.IOPattern == types.IOPatternReadHeavy || sig.IOPattern == types.IOPatternWriteHeavy {
				for _, csd := range insight.CSDDevices {
					if csd.IsAvailable && csd.ComputeUtilization < 80 {
						score += 20
						break
					}
				}
			}
		}

		if score > bestScore {
			bestScore = score
			bestNode = nodeName
		}
	}

	return bestNode, bestScore
}

// calculateNodeSuitability calculates how suitable a node is for a workload
func (s *OrchestrationPolicyService) calculateNodeSuitability(sig *types.WorkloadSignature, insight *types.ClusterInsight) int {
	if insight == nil || insight.NodeResources == nil {
		return 0
	}

	score := 50 // Base score

	// Check resource availability
	cpuAvailPercent := float64(insight.NodeResources.CPUAvailableCores) / float64(insight.NodeResources.CPUTotalCores) * 100
	memAvailPercent := float64(insight.NodeResources.MemoryAvailableBytes) / float64(insight.NodeResources.MemoryTotalBytes) * 100

	if cpuAvailPercent > 50 {
		score += 15
	} else if cpuAvailPercent > 30 {
		score += 10
	}

	if memAvailPercent > 50 {
		score += 15
	} else if memAvailPercent > 30 {
		score += 10
	}

	// Check GPU availability for GPU workloads
	if sig != nil && sig.IsGPUWorkload {
		if insight.NodeResources.GPUAvailable > 0 {
			score += 25
		} else {
			return 0 // Not suitable for GPU workload
		}
	}

	// Check storage device health and utilization
	for _, storage := range insight.StorageDevices {
		if storage.DeviceType == "nvme" || storage.DeviceType == "ssd" {
			if storage.UtilizationPercent < 50 {
				score += 10
			}
		}
	}

	// Cap at 100
	if score > 100 {
		score = 100
	}

	return score
}

// calculateMigrationPriority calculates priority for migration (higher = more urgent)
func (s *OrchestrationPolicyService) calculateMigrationPriority(sig *types.WorkloadSignature, currentNode *types.ClusterInsight) int {
	priority := 50 // Base priority

	if currentNode == nil || currentNode.NodeResources == nil {
		return priority
	}

	// Increase priority based on resource pressure
	cpuUsed := float64(currentNode.NodeResources.CPUUsedCores) / float64(currentNode.NodeResources.CPUTotalCores) * 100
	if cpuUsed > 90 {
		priority += 30
	} else if cpuUsed > 80 {
		priority += 20
	}

	memUsed := float64(currentNode.NodeResources.MemoryUsedBytes) / float64(currentNode.NodeResources.MemoryTotalBytes) * 100
	if memUsed > 90 {
		priority += 30
	} else if memUsed > 85 {
		priority += 20
	}

	// GPU workloads get higher priority
	if sig != nil && sig.IsGPUWorkload {
		priority += 10
	}

	// Cap at 100
	if priority > 100 {
		priority = 100
	}

	return priority
}

// generateMigrationConstraints generates constraints for migration
func (s *OrchestrationPolicyService) generateMigrationConstraints(sig *types.WorkloadSignature, req *OrchestrationPolicyRequest) *types.MigrationConstraints {
	constraints := &types.MigrationConstraints{
		MaxDowntimeSeconds:  60,  // Default 1 minute max downtime
		PreservePV:          true,
		TimeoutMinutes:      10,
		GracePeriodSeconds:  30,
		CheckpointEnabled:   true,
		LiveMigration:       false, // Not supported yet
		RetryOnFailure:      true,
		MaxRetries:          3,
	}

	// Adjust based on workload type
	if sig != nil {
		switch sig.WorkloadType {
		case types.WorkloadTypeVideo:
			// Video processing needs more time
			constraints.MaxDowntimeSeconds = 120
			constraints.TimeoutMinutes = 20
		case types.WorkloadTypeText:
			// Text models are large, need PV
			constraints.PreservePV = true
			constraints.CheckpointEnabled = true
		case types.WorkloadTypeImage:
			// Image processing is standard
			constraints.MaxDowntimeSeconds = 90
		}

		// Argo workflow specific settings
		if sig.ArgoContext != nil {
			// Preserve PV for Argo workflows to maintain pipeline state
			constraints.PreservePV = true
		}
	}

	return constraints
}

// checkScalingNeeded checks if scaling would help
func (s *OrchestrationPolicyService) checkScalingNeeded(sig *types.WorkloadSignature, currentNode *types.ClusterInsight) bool {
	if currentNode == nil || currentNode.NodeResources == nil {
		return false
	}

	// Check if node is moderately loaded (not critical)
	cpuUsed := float64(currentNode.NodeResources.CPUUsedCores) / float64(currentNode.NodeResources.CPUTotalCores) * 100
	memUsed := float64(currentNode.NodeResources.MemoryUsedBytes) / float64(currentNode.NodeResources.MemoryTotalBytes) * 100

	// Scale if moderately loaded but not critical
	if cpuUsed > 60 && cpuUsed < 80 {
		return true
	}
	if memUsed > 70 && memUsed < 85 {
		return true
	}

	return false
}

// calculateScaleTarget calculates the target replica count
func (s *OrchestrationPolicyService) calculateScaleTarget(sig *types.WorkloadSignature, currentNode *types.ClusterInsight) int {
	// Simple scaling logic - increase by 1
	// In production, this would be more sophisticated
	return 2
}

// checkOptimizationPossible checks if storage optimization is possible
func (s *OrchestrationPolicyService) checkOptimizationPossible(sig *types.WorkloadSignature, currentNode *types.ClusterInsight) bool {
	if currentNode == nil {
		return false
	}

	// Check if CSD offloading is possible
	if sig != nil && (sig.IOPattern == types.IOPatternReadHeavy || sig.IOPattern == types.IOPatternWriteHeavy) {
		for _, csd := range currentNode.CSDDevices {
			if csd.IsAvailable && csd.ComputeUtilization < 50 {
				return true // CSD can take on more work
			}
		}
	}

	return false
}

// UpdateMigrationStatus updates the status of an active migration
func (s *OrchestrationPolicyService) UpdateMigrationStatus(ctx context.Context, update *MigrationStatusUpdate) (*ReportResponse, error) {
	s.stats.TotalStatusUpdates++

	s.migrationMux.Lock()
	defer s.migrationMux.Unlock()

	policy, exists := s.activeMigrations[update.RequestID]
	if !exists {
		return &ReportResponse{
			Success:   false,
			Message:   "migration not found",
			RequestID: update.RequestID,
		}, nil
	}

	log.Printf("[OrchestrationPolicy] Migration status update: %s -> %s", update.RequestID, update.Status)

	// Update status
	if update.Status == "completed" || update.Status == "failed" {
		delete(s.activeMigrations, update.RequestID)
	}

	return &ReportResponse{
		Success:   true,
		Message:   fmt.Sprintf("status updated for policy %s", policy.RequestID),
		RequestID: update.RequestID,
	}, nil
}

// GetStats returns service statistics
func (s *OrchestrationPolicyService) GetStats() OrchestrationStats {
	return s.stats
}

// GetActiveMigrations returns currently active migrations
func (s *OrchestrationPolicyService) GetActiveMigrations() []*types.OrchestrationPolicy {
	s.migrationMux.RLock()
	defer s.migrationMux.RUnlock()

	migrations := make([]*types.OrchestrationPolicy, 0, len(s.activeMigrations))
	for _, m := range s.activeMigrations {
		migrations = append(migrations, m)
	}
	return migrations
}

// OrchestrationPolicyRequest represents a request for orchestration policy
type OrchestrationPolicyRequest struct {
	PodName        string            `json:"pod_name"`
	PodNamespace   string            `json:"pod_namespace"`
	CurrentNode    string            `json:"current_node"`
	PodLabels      map[string]string `json:"pod_labels,omitempty"`
	PodAnnotations map[string]string `json:"pod_annotations,omitempty"`
	Reason         string            `json:"reason,omitempty"` // Why policy is being requested
}

// MigrationStatusUpdate represents a migration status update
type MigrationStatusUpdate struct {
	RequestID      string `json:"request_id"`
	PodName        string `json:"pod_name"`
	PodNamespace   string `json:"pod_namespace"`
	Status         string `json:"status"` // pending, in_progress, completed, failed
	Message        string `json:"message,omitempty"`
	TargetNode     string `json:"target_node,omitempty"`
	DurationMs     int64  `json:"duration_ms,omitempty"`
	BytesMigrated  int64  `json:"bytes_migrated,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
}

// ReportResponse represents a response to a report request
type ReportResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}
