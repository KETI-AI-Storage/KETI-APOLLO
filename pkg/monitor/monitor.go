// ============================================
// APOLLO gRPC Monitor
// 수신된 모든 gRPC 메시지를 로그로 출력
// ============================================

package monitor

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Monitor tracks and logs all received gRPC messages
type Monitor struct {
	mu sync.RWMutex

	// Counters
	workloadSignatureCount  atomic.Int64
	clusterInsightCount     atomic.Int64
	schedulingRequestCount  atomic.Int64
	orchestrationEventCount atomic.Int64

	// Recent messages (ring buffer)
	recentWorkloads []WorkloadLog
	recentInsights  []InsightLog
	maxRecent       int

	// Start time
	startTime time.Time

	// Verbose mode
	verbose bool
}

// WorkloadLog represents a logged workload signature
type WorkloadLog struct {
	Timestamp     time.Time `json:"timestamp"`
	PodName       string    `json:"pod_name"`
	PodNamespace  string    `json:"pod_namespace"`
	NodeName      string    `json:"node_name"`
	WorkloadType  string    `json:"workload_type"`
	Stage         string    `json:"stage"`
	IOPattern     string    `json:"io_pattern"`
	Framework     string    `json:"framework"`
	IsGPU         bool      `json:"is_gpu"`
	Confidence    float64   `json:"confidence"`
}

// InsightLog represents a logged cluster insight
type InsightLog struct {
	Timestamp       time.Time `json:"timestamp"`
	NodeName        string    `json:"node_name"`
	CPUUsage        float64   `json:"cpu_usage"`
	MemoryUsage     float64   `json:"memory_usage"`
	StorageDevices  int       `json:"storage_devices"`
	GPUDevices      int       `json:"gpu_devices"`
	CSDDevices      int       `json:"csd_devices"`
	RunningPods     int       `json:"running_pods"`
}

// NewMonitor creates a new monitor instance
func NewMonitor(verbose bool) *Monitor {
	return &Monitor{
		recentWorkloads: make([]WorkloadLog, 0, 100),
		recentInsights:  make([]InsightLog, 0, 100),
		maxRecent:       100,
		startTime:       time.Now(),
		verbose:         verbose,
	}
}

// LogWorkloadSignature logs a received workload signature
func (m *Monitor) LogWorkloadSignature(podName, podNamespace, nodeName, workloadType, stage, ioPattern, framework string, isGPU bool, confidence float64) {
	m.workloadSignatureCount.Add(1)
	count := m.workloadSignatureCount.Load()

	entry := WorkloadLog{
		Timestamp:    time.Now(),
		PodName:      podName,
		PodNamespace: podNamespace,
		NodeName:     nodeName,
		WorkloadType: workloadType,
		Stage:        stage,
		IOPattern:    ioPattern,
		Framework:    framework,
		IsGPU:        isGPU,
		Confidence:   confidence,
	}

	// Store in ring buffer
	m.mu.Lock()
	if len(m.recentWorkloads) >= m.maxRecent {
		m.recentWorkloads = m.recentWorkloads[1:]
	}
	m.recentWorkloads = append(m.recentWorkloads, entry)
	m.mu.Unlock()

	// Log to console
	log.Printf("┌─────────────────────────────────────────────────────────────────┐")
	log.Printf("│ [GRPC RECV] WorkloadSignature #%d                               │", count)
	log.Printf("├─────────────────────────────────────────────────────────────────┤")
	log.Printf("│ Pod:       %s/%s", podNamespace, podName)
	log.Printf("│ Node:      %s", nodeName)
	log.Printf("│ Type:      %s", workloadType)
	log.Printf("│ Stage:     %s", stage)
	log.Printf("│ I/O:       %s", ioPattern)
	log.Printf("│ Framework: %s", framework)
	log.Printf("│ GPU:       %v", isGPU)
	log.Printf("│ Confidence: %.2f", confidence)
	log.Printf("└─────────────────────────────────────────────────────────────────┘")

	if m.verbose {
		jsonData, _ := json.MarshalIndent(entry, "", "  ")
		log.Printf("[VERBOSE] Full WorkloadSignature:\n%s", string(jsonData))
	}
}

// LogClusterInsight logs a received cluster insight
func (m *Monitor) LogClusterInsight(nodeName string, cpuUsage, memUsage float64, storageDevices, gpuDevices, csdDevices, runningPods int) {
	m.clusterInsightCount.Add(1)
	count := m.clusterInsightCount.Load()

	entry := InsightLog{
		Timestamp:      time.Now(),
		NodeName:       nodeName,
		CPUUsage:       cpuUsage,
		MemoryUsage:    memUsage,
		StorageDevices: storageDevices,
		GPUDevices:     gpuDevices,
		CSDDevices:     csdDevices,
		RunningPods:    runningPods,
	}

	// Store in ring buffer
	m.mu.Lock()
	if len(m.recentInsights) >= m.maxRecent {
		m.recentInsights = m.recentInsights[1:]
	}
	m.recentInsights = append(m.recentInsights, entry)
	m.mu.Unlock()

	// Log to console
	log.Printf("┌─────────────────────────────────────────────────────────────────┐")
	log.Printf("│ [GRPC RECV] ClusterInsight #%d                                  │", count)
	log.Printf("├─────────────────────────────────────────────────────────────────┤")
	log.Printf("│ Node:      %s", nodeName)
	log.Printf("│ CPU:       %.1f%%", cpuUsage)
	log.Printf("│ Memory:    %.1f%%", memUsage)
	log.Printf("│ Storage:   %d devices", storageDevices)
	log.Printf("│ GPU:       %d devices", gpuDevices)
	log.Printf("│ CSD:       %d devices", csdDevices)
	log.Printf("│ Pods:      %d running", runningPods)
	log.Printf("└─────────────────────────────────────────────────────────────────┘")

	if m.verbose {
		jsonData, _ := json.MarshalIndent(entry, "", "  ")
		log.Printf("[VERBOSE] Full ClusterInsight:\n%s", string(jsonData))
	}
}

// LogSchedulingRequest logs a scheduling policy request
func (m *Monitor) LogSchedulingRequest(podName, podNamespace string) {
	m.schedulingRequestCount.Add(1)
	count := m.schedulingRequestCount.Load()

	log.Printf("┌─────────────────────────────────────────────────────────────────┐")
	log.Printf("│ [GRPC RECV] SchedulingPolicyRequest #%d                         │", count)
	log.Printf("├─────────────────────────────────────────────────────────────────┤")
	log.Printf("│ Pod: %s/%s", podNamespace, podName)
	log.Printf("└─────────────────────────────────────────────────────────────────┘")
}

// LogOrchestrationEvent logs an orchestration event
func (m *Monitor) LogOrchestrationEvent(eventType, podName, podNamespace, details string) {
	m.orchestrationEventCount.Add(1)
	count := m.orchestrationEventCount.Load()

	log.Printf("┌─────────────────────────────────────────────────────────────────┐")
	log.Printf("│ [GRPC RECV] OrchestrationEvent #%d                              │", count)
	log.Printf("├─────────────────────────────────────────────────────────────────┤")
	log.Printf("│ Type:    %s", eventType)
	log.Printf("│ Pod:     %s/%s", podNamespace, podName)
	log.Printf("│ Details: %s", details)
	log.Printf("└─────────────────────────────────────────────────────────────────┘")
}

// GetStats returns current statistics
func (m *Monitor) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"uptime_seconds":            time.Since(m.startTime).Seconds(),
		"workload_signatures_recv":  m.workloadSignatureCount.Load(),
		"cluster_insights_recv":     m.clusterInsightCount.Load(),
		"scheduling_requests_recv":  m.schedulingRequestCount.Load(),
		"orchestration_events_recv": m.orchestrationEventCount.Load(),
	}
}

// GetRecentWorkloads returns recent workload logs
func (m *Monitor) GetRecentWorkloads() []WorkloadLog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]WorkloadLog, len(m.recentWorkloads))
	copy(result, m.recentWorkloads)
	return result
}

// GetRecentInsights returns recent insight logs
func (m *Monitor) GetRecentInsights() []InsightLog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]InsightLog, len(m.recentInsights))
	copy(result, m.recentInsights)
	return result
}

// PrintSummary prints a summary of received messages
func (m *Monitor) PrintSummary() {
	fmt.Println()
	log.Printf("╔═════════════════════════════════════════════════════════════════╗")
	log.Printf("║              APOLLO gRPC Monitor Summary                        ║")
	log.Printf("╠═════════════════════════════════════════════════════════════════╣")
	log.Printf("║ Uptime:              %s", time.Since(m.startTime).Round(time.Second))
	log.Printf("║ WorkloadSignatures:  %d received", m.workloadSignatureCount.Load())
	log.Printf("║ ClusterInsights:     %d received", m.clusterInsightCount.Load())
	log.Printf("║ SchedulingRequests:  %d received", m.schedulingRequestCount.Load())
	log.Printf("║ OrchestrationEvents: %d received", m.orchestrationEventCount.Load())
	log.Printf("╚═════════════════════════════════════════════════════════════════╝")
	fmt.Println()
}
