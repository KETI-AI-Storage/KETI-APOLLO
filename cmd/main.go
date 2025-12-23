// ============================================
// APOLLO Policy Server - Main Entry Point
// AI Workload 기반 스케줄링/오케스트레이션 정책 엔진
// ============================================

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pb "apollo/api/proto"
	grpcserver "apollo/pkg/grpc"
	"apollo/pkg/monitor"
)

// Config holds the server configuration
type Config struct {
	// Server ports
	GRPCPort int
	HTTPPort int

	// External service endpoints
	SchedulerEndpoint    string
	OrchestratorEndpoint string

	// Service settings
	PolicyCacheTTLMinutes int
	ForecastModelType     string
	ForecastHorizonMin    int

	// Monitor settings
	VerboseLogging bool
}

// APOLLOServer is the main policy server
type APOLLOServer struct {
	config Config

	// gRPC server
	grpcServer *grpcserver.Server

	// Monitor
	monitor *monitor.Monitor

	// HTTP server
	httpServer *http.Server

	// Data stores
	workloadSignatures map[string]*pb.WorkloadSignature // key: namespace/name
	clusterInsights    map[string]*pb.ClusterInsight    // key: nodeName
	dataMu             sync.RWMutex

	// Shutdown
	shutdownOnce sync.Once
}

var startTime = time.Now()

func main() {
	log.Println("╔═══════════════════════════════════════════════════════════════╗")
	log.Println("║          APOLLO Policy Server v1.0.0                          ║")
	log.Println("║   Adaptive Policy Optimization for Low-Latency I/O            ║")
	log.Println("╠═══════════════════════════════════════════════════════════════╣")
	log.Println("║   Modules:                                                    ║")
	log.Println("║   - Node Resource Forecaster                                  ║")
	log.Println("║   - Scheduling Policy Engine                                  ║")
	log.Println("║   - Orchestration Policy Engine                               ║")
	log.Println("╚═══════════════════════════════════════════════════════════════╝")

	// Load configuration
	config := loadConfig()

	// Create server
	srv := NewAPOLLOServer(config)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start gRPC server in goroutine
	go func() {
		if err := srv.grpcServer.Start(); err != nil {
			log.Fatalf("gRPC server failed: %v", err)
		}
	}()

	// Start HTTP server in goroutine
	go func() {
		if err := srv.StartHTTP(ctx); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Print monitor summary every 30 seconds
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				srv.monitor.PrintSummary()
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Println("")
	log.Println("============================================")
	log.Printf("APOLLO Server is ready!")
	log.Printf("  gRPC: localhost:%d", config.GRPCPort)
	log.Printf("  HTTP: localhost:%d", config.HTTPPort)
	log.Println("============================================")
	log.Println("")

	// Wait for shutdown signal
	<-sigCh
	log.Println("")
	log.Println("Shutdown signal received, stopping server...")

	// Print final summary
	srv.monitor.PrintSummary()

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	log.Println("APOLLO Policy Server stopped")
}

// loadConfig loads configuration from environment
func loadConfig() Config {
	config := Config{
		GRPCPort:              getEnvInt("GRPC_PORT", 50051),
		HTTPPort:              getEnvInt("HTTP_PORT", 8080),
		SchedulerEndpoint:     getEnv("SCHEDULER_ENDPOINT", "ai-storage-scheduler:50053"),
		OrchestratorEndpoint:  getEnv("ORCHESTRATOR_ENDPOINT", "ai-storage-orchestrator:8080"),
		PolicyCacheTTLMinutes: getEnvInt("POLICY_CACHE_TTL_MINUTES", 5),
		ForecastModelType:     getEnv("FORECAST_MODEL_TYPE", "simple"),
		ForecastHorizonMin:    getEnvInt("FORECAST_HORIZON_MIN", 30),
		VerboseLogging:        getEnv("VERBOSE", "false") == "true",
	}

	log.Println("Configuration:")
	log.Printf("  gRPC Port:             %d", config.GRPCPort)
	log.Printf("  HTTP Port:             %d", config.HTTPPort)
	log.Printf("  Scheduler Endpoint:    %s", config.SchedulerEndpoint)
	log.Printf("  Orchestrator Endpoint: %s", config.OrchestratorEndpoint)
	log.Printf("  Verbose Logging:       %v", config.VerboseLogging)
	log.Println("")

	return config
}

// NewAPOLLOServer creates a new APOLLO server instance
func NewAPOLLOServer(config Config) *APOLLOServer {
	// Create monitor
	mon := monitor.NewMonitor(config.VerboseLogging)

	// Create gRPC server
	grpcSrv := grpcserver.NewServer(config.GRPCPort, mon)

	srv := &APOLLOServer{
		config:             config,
		grpcServer:         grpcSrv,
		monitor:            mon,
		workloadSignatures: make(map[string]*pb.WorkloadSignature),
		clusterInsights:    make(map[string]*pb.ClusterInsight),
	}

	// Set up gRPC handlers
	grpcSrv.SetWorkloadSignatureHandler(srv.handleWorkloadSignature)
	grpcSrv.SetClusterInsightHandler(srv.handleClusterInsight)
	grpcSrv.SetSchedulingPolicyProvider(srv.getSchedulingPolicy)
	grpcSrv.SetOrchestrationPolicyProvider(srv.getOrchestrationPolicy)

	return srv
}

// handleWorkloadSignature processes received workload signature
func (s *APOLLOServer) handleWorkloadSignature(sig *pb.WorkloadSignature) {
	key := fmt.Sprintf("%s/%s", sig.PodNamespace, sig.PodName)

	s.dataMu.Lock()
	s.workloadSignatures[key] = sig
	s.dataMu.Unlock()

	log.Printf("[Handler] Stored WorkloadSignature: %s (type: %s, stage: %s)",
		key, sig.WorkloadType.String(), sig.CurrentStage.String())
}

// handleClusterInsight processes received cluster insight
func (s *APOLLOServer) handleClusterInsight(insight *pb.ClusterInsight) {
	s.dataMu.Lock()
	s.clusterInsights[insight.NodeName] = insight
	s.dataMu.Unlock()

	log.Printf("[Handler] Stored ClusterInsight: %s (devices: storage=%d, gpu=%d, csd=%d)",
		insight.NodeName, len(insight.StorageDevices), len(insight.GpuDevices), len(insight.CsdDevices))
}

// getSchedulingPolicy returns scheduling policy for a pod
func (s *APOLLOServer) getSchedulingPolicy(podName, podNamespace string) *pb.SchedulingPolicy {
	key := fmt.Sprintf("%s/%s", podNamespace, podName)

	s.dataMu.RLock()
	sig, exists := s.workloadSignatures[key]
	s.dataMu.RUnlock()

	policy := &pb.SchedulingPolicy{
		RequestId:    fmt.Sprintf("sp-%s-%d", key, time.Now().Unix()),
		PodName:      podName,
		PodNamespace: podNamespace,
		Decision:     pb.SchedulingDecision_SCHEDULING_DECISION_ALLOW,
	}

	if !exists {
		policy.Reason = "No workload signature found, using default policy"
		return policy
	}

	// Generate policy based on workload signature
	policy.NodePreferences = s.calculateNodePreferences(sig)
	policy.Reason = fmt.Sprintf("Policy based on %s workload, %s stage", sig.WorkloadType.String(), sig.CurrentStage.String())

	// Set storage requirements if GPU workload
	if sig.IsGpuWorkload {
		policy.StorageRequirements = &pb.StorageRequirements{
			StorageClass: pb.StorageClass_STORAGE_CLASS_ULTRA_FAST,
			RequiresCsd:  true,
		}
	}

	return policy
}

// calculateNodePreferences calculates node preferences based on workload and cluster state
func (s *APOLLOServer) calculateNodePreferences(sig *pb.WorkloadSignature) []*pb.NodePreference {
	var preferences []*pb.NodePreference

	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	for nodeName, insight := range s.clusterInsights {
		score := int32(50) // Base score
		reason := "base score"

		// Prefer nodes with available resources
		if insight.NodeResources != nil {
			cpuAvail := insight.NodeResources.CpuAvailableCores
			if cpuAvail > 4 {
				score += 20
				reason = "high CPU availability"
			}
		}

		// GPU workloads prefer nodes with GPUs
		if sig.IsGpuWorkload && len(insight.GpuDevices) > 0 {
			score += 30
			reason = "GPU available"
		}

		// CSD preference for I/O heavy workloads
		if sig.IoPattern == pb.IOPattern_IO_PATTERN_READ_HEAVY || sig.IoPattern == pb.IOPattern_IO_PATTERN_WRITE_HEAVY {
			if len(insight.CsdDevices) > 0 {
				score += 20
				reason = "CSD available for I/O workload"
			}
		}

		preferences = append(preferences, &pb.NodePreference{
			NodeName: nodeName,
			Score:    score,
			Reason:   reason,
		})
	}

	return preferences
}

// getOrchestrationPolicy returns orchestration policy for a pod
func (s *APOLLOServer) getOrchestrationPolicy(podName, podNamespace string) *pb.OrchestrationPolicy {
	key := fmt.Sprintf("%s/%s", podNamespace, podName)

	s.dataMu.RLock()
	sig, exists := s.workloadSignatures[key]
	s.dataMu.RUnlock()

	policy := &pb.OrchestrationPolicy{
		RequestId: fmt.Sprintf("op-%s-%d", key, time.Now().Unix()),
		Action:    pb.OrchestrationAction_ORCHESTRATION_ACTION_UNSPECIFIED,
		Priority:  pb.PolicyPriority_POLICY_PRIORITY_MEDIUM,
	}

	if !exists {
		policy.Reason = "No workload signature found"
		return policy
	}

	// Check if migration is needed based on node resource state
	if sig.NodeName != "" {
		s.dataMu.RLock()
		insight, hasInsight := s.clusterInsights[sig.NodeName]
		s.dataMu.RUnlock()

		if hasInsight && insight.NodeResources != nil {
			// Check for resource pressure
			var cpuUsage, memUsage float64
			if insight.NodeResources.CpuAllocatableCores > 0 {
				cpuUsage = insight.NodeResources.CpuUsedCores / insight.NodeResources.CpuAllocatableCores * 100
			}
			if insight.NodeResources.MemoryAllocatableBytes > 0 {
				memUsage = float64(insight.NodeResources.MemoryUsedBytes) / float64(insight.NodeResources.MemoryAllocatableBytes) * 100
			}

			if cpuUsage > 90 || memUsage > 90 {
				policy.Action = pb.OrchestrationAction_ORCHESTRATION_ACTION_MIGRATE
				policy.Priority = pb.PolicyPriority_POLICY_PRIORITY_HIGH
				policy.Reason = fmt.Sprintf("Node resource pressure: CPU=%.1f%%, Memory=%.1f%%", cpuUsage, memUsage)

				// Find target node
				if target := s.findMigrationTarget(sig); target != nil {
					policy.Target = &pb.OrchestrationPolicy_Migration{Migration: target}
				}
			}
		}
	}

	return policy
}

// findMigrationTarget finds the best node for migration
func (s *APOLLOServer) findMigrationTarget(sig *pb.WorkloadSignature) *pb.MigrationTarget {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	var bestNode string
	var bestScore float64

	for nodeName, insight := range s.clusterInsights {
		if nodeName == sig.NodeName {
			continue // Skip current node
		}

		if insight.NodeResources == nil {
			continue
		}

		// Calculate score based on available resources
		score := insight.NodeResources.CpuAvailableCores * 10
		score += float64(insight.NodeResources.MemoryAvailableBytes) / (1024 * 1024 * 1024) // GB

		if score > bestScore {
			bestScore = score
			bestNode = nodeName
		}
	}

	if bestNode == "" {
		return nil
	}

	return &pb.MigrationTarget{
		PodName:        sig.PodName,
		PodNamespace:   sig.PodNamespace,
		SourceNode:     sig.NodeName,
		TargetNode:     bestNode,
		PreservePv:     true,
		TimeoutSeconds: 600,
	}
}

// StartHTTP starts the HTTP server
func (s *APOLLOServer) StartHTTP(ctx context.Context) error {
	mux := http.NewServeMux()

	// Health endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	// Status and monitoring
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/monitor", s.handleMonitor)
	mux.HandleFunc("/api/v1/monitor/workloads", s.handleMonitorWorkloads)
	mux.HandleFunc("/api/v1/monitor/insights", s.handleMonitorInsights)

	// Data endpoints (for debugging)
	mux.HandleFunc("/api/v1/data/workloads", s.handleDataWorkloads)
	mux.HandleFunc("/api/v1/data/insights", s.handleDataInsights)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.config.HTTPPort),
		Handler: mux,
	}

	log.Printf("Starting HTTP server on port %d", s.config.HTTPPort)
	return s.httpServer.ListenAndServe()
}

// HTTP Handlers
func (s *APOLLOServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func (s *APOLLOServer) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (s *APOLLOServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.dataMu.RLock()
	workloadCount := len(s.workloadSignatures)
	insightCount := len(s.clusterInsights)
	s.dataMu.RUnlock()

	status := map[string]interface{}{
		"server":  "APOLLO Policy Server",
		"version": "1.0.0",
		"uptime":  time.Since(startTime).String(),
		"data": map[string]int{
			"workload_signatures": workloadCount,
			"cluster_insights":    insightCount,
		},
		"monitor": s.monitor.GetStats(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *APOLLOServer) handleMonitor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.monitor.GetStats())
}

func (s *APOLLOServer) handleMonitorWorkloads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.monitor.GetRecentWorkloads())
}

func (s *APOLLOServer) handleMonitorInsights(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.monitor.GetRecentInsights())
}

func (s *APOLLOServer) handleDataWorkloads(w http.ResponseWriter, r *http.Request) {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.workloadSignatures)
}

func (s *APOLLOServer) handleDataInsights(w http.ResponseWriter, r *http.Request) {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.clusterInsights)
}

// Shutdown gracefully shuts down the server
func (s *APOLLOServer) Shutdown(ctx context.Context) error {
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		// Stop gRPC server
		s.grpcServer.Stop()

		// Shutdown HTTP server
		if s.httpServer != nil {
			if err := s.httpServer.Shutdown(ctx); err != nil {
				shutdownErr = fmt.Errorf("HTTP server shutdown error: %w", err)
			}
		}
	})
	return shutdownErr
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err == nil {
			return result
		}
	}
	return defaultValue
}
