// ============================================
// APOLLO InsightService Server
// Insight Trace/Scope로부터 데이터 수신
// ============================================

package insight

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"apollo/pkg/types"
)

// InsightService handles incoming data from Insight Trace and Insight Scope
type InsightService struct {
	// Data stores
	workloadSignatures map[string]*types.WorkloadSignature // key: pod_namespace/pod_name
	clusterInsights    map[string]*types.ClusterInsight    // key: node_name

	// Mutex for thread safety
	sigMux     sync.RWMutex
	insightMux sync.RWMutex

	// Callbacks for policy engine
	onWorkloadUpdate func(*types.WorkloadSignature)
	onClusterUpdate  func(*types.ClusterInsight)

	// Statistics
	stats InsightStats
}

// InsightStats tracks service statistics
type InsightStats struct {
	TotalSignaturesReceived int64
	TotalInsightsReceived   int64
	LastSignatureTime       time.Time
	LastInsightTime         time.Time
}

// NewInsightService creates a new InsightService
func NewInsightService() *InsightService {
	return &InsightService{
		workloadSignatures: make(map[string]*types.WorkloadSignature),
		clusterInsights:    make(map[string]*types.ClusterInsight),
	}
}

// SetWorkloadUpdateCallback sets callback for workload signature updates
func (s *InsightService) SetWorkloadUpdateCallback(cb func(*types.WorkloadSignature)) {
	s.onWorkloadUpdate = cb
}

// SetWorkloadSignatureCallback is an alias for SetWorkloadUpdateCallback
func (s *InsightService) SetWorkloadSignatureCallback(cb func(*types.WorkloadSignature)) {
	s.onWorkloadUpdate = cb
}

// SetClusterUpdateCallback sets callback for cluster insight updates
func (s *InsightService) SetClusterUpdateCallback(cb func(*types.ClusterInsight)) {
	s.onClusterUpdate = cb
}

// SetClusterInsightCallback is an alias for SetClusterUpdateCallback
func (s *InsightService) SetClusterInsightCallback(cb func(*types.ClusterInsight)) {
	s.onClusterUpdate = cb
}

// ReceiveWorkloadSignature handles incoming workload signature (HTTP API)
func (s *InsightService) ReceiveWorkloadSignature(ctx context.Context, sig *types.WorkloadSignature) error {
	_, err := s.ReportWorkloadSignature(ctx, sig)
	return err
}

// ReceiveClusterInsight handles incoming cluster insight (HTTP API)
func (s *InsightService) ReceiveClusterInsight(ctx context.Context, insight *types.ClusterInsight) error {
	_, err := s.ReportClusterInsight(ctx, insight)
	return err
}

// GetWorkloadSignatureCount returns the number of stored signatures
func (s *InsightService) GetWorkloadSignatureCount() int {
	s.sigMux.RLock()
	defer s.sigMux.RUnlock()
	return len(s.workloadSignatures)
}

// GetClusterInsightCount returns the number of stored insights
func (s *InsightService) GetClusterInsightCount() int {
	s.insightMux.RLock()
	defer s.insightMux.RUnlock()
	return len(s.clusterInsights)
}

// ReportWorkloadSignature handles single workload signature report
func (s *InsightService) ReportWorkloadSignature(ctx context.Context, sig *types.WorkloadSignature) (*ReportResponse, error) {
	if sig == nil {
		return &ReportResponse{
			Success: false,
			Message: "nil signature received",
		}, nil
	}

	key := sig.PodNamespace + "/" + sig.PodName

	s.sigMux.Lock()
	s.workloadSignatures[key] = sig
	s.stats.TotalSignaturesReceived++
	s.stats.LastSignatureTime = time.Now()
	s.sigMux.Unlock()

	log.Printf("[InsightService] Received workload signature: %s (type=%s, stage=%s, io=%s)",
		key, sig.WorkloadType, sig.CurrentStage, sig.IOPattern)

	// Trigger callback if set
	if s.onWorkloadUpdate != nil {
		go s.onWorkloadUpdate(sig)
	}

	return &ReportResponse{
		Success:   true,
		Message:   "signature received",
		RequestID: key,
	}, nil
}

// StreamWorkloadSignatures handles streaming workload signatures
func (s *InsightService) StreamWorkloadSignatures(stream WorkloadSignatureStream) (*StreamResponse, error) {
	var count int64

	for {
		sig, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[InsightService] Stream error: %v", err)
			return &StreamResponse{
				MessagesReceived:  count,
				MessagesProcessed: count,
				Status:            "error: " + err.Error(),
			}, err
		}

		// Process each signature
		_, _ = s.ReportWorkloadSignature(context.Background(), sig)
		count++
	}

	return &StreamResponse{
		MessagesReceived:  count,
		MessagesProcessed: count,
		Status:            "completed",
	}, nil
}

// ReportClusterInsight handles single cluster insight report
func (s *InsightService) ReportClusterInsight(ctx context.Context, insight *types.ClusterInsight) (*ReportResponse, error) {
	if insight == nil {
		return &ReportResponse{
			Success: false,
			Message: "nil insight received",
		}, nil
	}

	key := insight.NodeName

	s.insightMux.Lock()
	s.clusterInsights[key] = insight
	s.stats.TotalInsightsReceived++
	s.stats.LastInsightTime = time.Now()
	s.insightMux.Unlock()

	log.Printf("[InsightService] Received cluster insight: node=%s (workloads=%d, gpus=%d, csds=%d)",
		key, len(insight.RunningWorkloads), len(insight.GPUDevices), len(insight.CSDDevices))

	// Trigger callback if set
	if s.onClusterUpdate != nil {
		go s.onClusterUpdate(insight)
	}

	return &ReportResponse{
		Success:   true,
		Message:   "insight received",
		RequestID: key,
	}, nil
}

// StreamClusterInsights handles streaming cluster insights
func (s *InsightService) StreamClusterInsights(stream ClusterInsightStream) (*StreamResponse, error) {
	var count int64

	for {
		insight, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[InsightService] Stream error: %v", err)
			return &StreamResponse{
				MessagesReceived:  count,
				MessagesProcessed: count,
				Status:            "error: " + err.Error(),
			}, err
		}

		// Process each insight
		_, _ = s.ReportClusterInsight(context.Background(), insight)
		count++
	}

	return &StreamResponse{
		MessagesReceived:  count,
		MessagesProcessed: count,
		Status:            "completed",
	}, nil
}

// GetWorkloadSignature returns a specific workload signature
func (s *InsightService) GetWorkloadSignature(namespace, name string) *types.WorkloadSignature {
	key := namespace + "/" + name
	s.sigMux.RLock()
	defer s.sigMux.RUnlock()
	return s.workloadSignatures[key]
}

// GetAllWorkloadSignatures returns all workload signatures
func (s *InsightService) GetAllWorkloadSignatures() map[string]*types.WorkloadSignature {
	s.sigMux.RLock()
	defer s.sigMux.RUnlock()

	result := make(map[string]*types.WorkloadSignature)
	for k, v := range s.workloadSignatures {
		result[k] = v
	}
	return result
}

// GetClusterInsight returns insight for a specific node
func (s *InsightService) GetClusterInsight(nodeName string) *types.ClusterInsight {
	s.insightMux.RLock()
	defer s.insightMux.RUnlock()
	return s.clusterInsights[nodeName]
}

// GetAllClusterInsights returns all cluster insights
func (s *InsightService) GetAllClusterInsights() map[string]*types.ClusterInsight {
	s.insightMux.RLock()
	defer s.insightMux.RUnlock()

	result := make(map[string]*types.ClusterInsight)
	for k, v := range s.clusterInsights {
		result[k] = v
	}
	return result
}

// GetStats returns service statistics
func (s *InsightService) GetStats() InsightStats {
	s.sigMux.RLock()
	s.insightMux.RLock()
	defer s.sigMux.RUnlock()
	defer s.insightMux.RUnlock()
	return s.stats
}

// CleanupStaleEntries removes entries older than maxAge
func (s *InsightService) CleanupStaleEntries(maxAge time.Duration) {
	now := time.Now()

	s.sigMux.Lock()
	for key, sig := range s.workloadSignatures {
		if now.Sub(sig.LastSeen) > maxAge {
			delete(s.workloadSignatures, key)
			log.Printf("[InsightService] Removed stale signature: %s", key)
		}
	}
	s.sigMux.Unlock()

	s.insightMux.Lock()
	for key, insight := range s.clusterInsights {
		if now.Sub(insight.CollectedAt) > maxAge {
			delete(s.clusterInsights, key)
			log.Printf("[InsightService] Removed stale insight: %s", key)
		}
	}
	s.insightMux.Unlock()
}

// ReportResponse represents a response to a report request
type ReportResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// StreamResponse represents a response to a stream request
type StreamResponse struct {
	MessagesReceived  int64  `json:"messages_received"`
	MessagesProcessed int64  `json:"messages_processed"`
	Status            string `json:"status"`
}

// WorkloadSignatureStream interface for streaming signatures
type WorkloadSignatureStream interface {
	Recv() (*types.WorkloadSignature, error)
}

// ClusterInsightStream interface for streaming insights
type ClusterInsightStream interface {
	Recv() (*types.ClusterInsight, error)
}
