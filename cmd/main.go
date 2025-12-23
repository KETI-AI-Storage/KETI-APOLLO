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

	"apollo/pkg/client"
	"apollo/pkg/server"
	"apollo/pkg/types"
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
}

// APOLLOServer is the main policy server
type APOLLOServer struct {
	config Config

	// Services
	insightService       *server.InsightService
	schedulingService    *server.SchedulingPolicyService
	orchestrationService *server.OrchestrationPolicyService
	forecastService      *server.ForecastService

	// Clients
	schedulerClient    *client.SchedulerClient
	orchestratorClient *client.OrchestratorClient

	// HTTP server
	httpServer *http.Server

	// Shutdown
	shutdownOnce sync.Once
}

func main() {
	log.Println("============================================")
	log.Println("APOLLO Policy Server")
	log.Println("AI Workload Scheduling & Orchestration Policy Engine")
	log.Println("============================================")

	// Load configuration
	config := loadConfig()

	// Create server
	srv := NewAPOLLOServer(config)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start server
	go func() {
		if err := srv.Start(ctx); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-sigCh
	log.Println("Shutdown signal received, stopping server...")

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
		SchedulerEndpoint:     getEnv("SCHEDULER_ENDPOINT", "http://ai-storage-scheduler:50053"),
		OrchestratorEndpoint:  getEnv("ORCHESTRATOR_ENDPOINT", "http://ai-storage-orchestrator:8080"),
		PolicyCacheTTLMinutes: getEnvInt("POLICY_CACHE_TTL_MINUTES", 5),
		ForecastModelType:     getEnv("FORECAST_MODEL_TYPE", "simple"),
		ForecastHorizonMin:    getEnvInt("FORECAST_HORIZON_MIN", 30),
	}

	log.Printf("Configuration loaded:")
	log.Printf("  GRPC Port: %d", config.GRPCPort)
	log.Printf("  HTTP Port: %d", config.HTTPPort)
	log.Printf("  Scheduler Endpoint: %s", config.SchedulerEndpoint)
	log.Printf("  Orchestrator Endpoint: %s", config.OrchestratorEndpoint)
	log.Printf("  Forecast Model: %s", config.ForecastModelType)

	return config
}

// NewAPOLLOServer creates a new APOLLO server instance
func NewAPOLLOServer(config Config) *APOLLOServer {
	// Create services
	forecastService := server.NewForecastService()
	forecastService.SetConfig(server.ForecastConfig{
		ModelType:             config.ForecastModelType,
		HistoryWindowMinutes:  60,
		PredictionHorizonMin:  config.ForecastHorizonMin,
		UpdateIntervalSeconds: 60,
		OverloadThreshold:     80.0,
		MinDataPoints:         10,
	})

	insightService := server.NewInsightService()
	schedulingService := server.NewSchedulingPolicyService(insightService)
	orchestrationService := server.NewOrchestrationPolicyService(insightService, forecastService)

	// Create clients
	schedulerClient := client.NewSchedulerClient(client.SchedulerClientConfig{
		Endpoint:   config.SchedulerEndpoint,
		TimeoutSec: 10,
		MaxRetries: 3,
	})

	orchestratorClient := client.NewOrchestratorClient(client.OrchestratorClientConfig{
		Endpoint:   config.OrchestratorEndpoint,
		TimeoutSec: 30,
		MaxRetries: 3,
	})

	return &APOLLOServer{
		config:               config,
		insightService:       insightService,
		schedulingService:    schedulingService,
		orchestrationService: orchestrationService,
		forecastService:      forecastService,
		schedulerClient:      schedulerClient,
		orchestratorClient:   orchestratorClient,
	}
}

// Start starts the APOLLO server
func (s *APOLLOServer) Start(ctx context.Context) error {
	// Setup insight callbacks
	s.setupCallbacks()

	// Start HTTP server
	mux := http.NewServeMux()
	s.registerHTTPHandlers(mux)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.config.HTTPPort),
		Handler: mux,
	}

	log.Printf("Starting HTTP server on port %d", s.config.HTTPPort)

	// Try to connect to external services (non-blocking)
	go s.connectToExternalServices(ctx)

	// Start HTTP server
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}

	return nil
}

// setupCallbacks sets up callbacks for insight events
func (s *APOLLOServer) setupCallbacks() {
	// When new workload signature is received
	s.insightService.SetWorkloadSignatureCallback(func(sig *types.WorkloadSignature) {
		log.Printf("[Callback] New workload signature for %s/%s", sig.PodNamespace, sig.PodName)

		// Record metrics for forecast service
		if insight := s.insightService.GetClusterInsight(sig.NodeName); insight != nil {
			ctx := context.Background()
			s.forecastService.RecordMetrics(ctx, sig.NodeName, insight)
		}
	})

	// When new cluster insight is received
	s.insightService.SetClusterInsightCallback(func(insight *types.ClusterInsight) {
		log.Printf("[Callback] New cluster insight from node %s", insight.NodeName)

		// Record metrics for forecast
		ctx := context.Background()
		s.forecastService.RecordMetrics(ctx, insight.NodeName, insight)
	})
}

// connectToExternalServices attempts to connect to scheduler and orchestrator
func (s *APOLLOServer) connectToExternalServices(ctx context.Context) {
	// Retry connection with backoff
	for i := 0; i < 5; i++ {
		if err := s.schedulerClient.Connect(ctx); err != nil {
			log.Printf("Failed to connect to scheduler (attempt %d): %v", i+1, err)
		} else {
			break
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Second)
	}

	for i := 0; i < 5; i++ {
		if err := s.orchestratorClient.Connect(ctx); err != nil {
			log.Printf("Failed to connect to orchestrator (attempt %d): %v", i+1, err)
		} else {
			break
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Second)
	}
}

// registerHTTPHandlers registers HTTP API handlers
func (s *APOLLOServer) registerHTTPHandlers(mux *http.ServeMux) {
	// Health endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	// Insight API - receive data from Insight Trace/Scope
	mux.HandleFunc("/api/v1/insights/workload", s.handleWorkloadSignature)
	mux.HandleFunc("/api/v1/insights/cluster", s.handleClusterInsight)

	// Policy API - provide policies to Scheduler/Orchestrator
	mux.HandleFunc("/api/v1/policies/scheduling", s.handleSchedulingPolicy)
	mux.HandleFunc("/api/v1/policies/orchestration", s.handleOrchestrationPolicy)

	// Forecast API
	mux.HandleFunc("/api/v1/forecast/", s.handleForecast)

	// Status and metrics
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/stats", s.handleStats)
}

// handleHealth handles health check
func (s *APOLLOServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

// handleReady handles readiness check
func (s *APOLLOServer) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// handleWorkloadSignature handles incoming workload signatures from Insight Trace
func (s *APOLLOServer) handleWorkloadSignature(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sig types.WorkloadSignature
	if err := json.NewDecoder(r.Body).Decode(&sig); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := s.insightService.ReceiveWorkloadSignature(ctx, &sig); err != nil {
		http.Error(w, fmt.Sprintf("Failed to process signature: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "accepted",
		"message": fmt.Sprintf("workload signature for %s/%s received", sig.PodNamespace, sig.PodName),
	})
}

// handleClusterInsight handles incoming cluster insights from Insight Scope
func (s *APOLLOServer) handleClusterInsight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var insight types.ClusterInsight
	if err := json.NewDecoder(r.Body).Decode(&insight); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := s.insightService.ReceiveClusterInsight(ctx, &insight); err != nil {
		http.Error(w, fmt.Sprintf("Failed to process insight: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "accepted",
		"message": fmt.Sprintf("cluster insight from %s received", insight.NodeName),
	})
}

// handleSchedulingPolicy handles scheduling policy requests from Scheduler
func (s *APOLLOServer) handleSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Get policy for a pod
		podNamespace := r.URL.Query().Get("namespace")
		podName := r.URL.Query().Get("name")

		if podNamespace == "" || podName == "" {
			http.Error(w, "Missing namespace or name parameter", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		req := &server.SchedulingPolicyRequest{
			PodName:      podName,
			PodNamespace: podNamespace,
		}

		policy, err := s.schedulingService.GetSchedulingPolicy(ctx, req)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get policy: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(policy)
		return
	}

	if r.Method == http.MethodPost {
		// Report scheduling result
		var result server.SchedulingResult
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		resp, err := s.schedulingService.ReportSchedulingResult(ctx, &result)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to report result: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleOrchestrationPolicy handles orchestration policy requests from Orchestrator
func (s *APOLLOServer) handleOrchestrationPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Get policy for a pod
		podNamespace := r.URL.Query().Get("namespace")
		podName := r.URL.Query().Get("name")
		currentNode := r.URL.Query().Get("node")

		if podNamespace == "" || podName == "" {
			http.Error(w, "Missing namespace or name parameter", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		req := &server.OrchestrationPolicyRequest{
			PodName:      podName,
			PodNamespace: podNamespace,
			CurrentNode:  currentNode,
		}

		policy, err := s.orchestrationService.GetOrchestrationPolicy(ctx, req)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get policy: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(policy)
		return
	}

	if r.Method == http.MethodPost {
		// Update migration status
		var update server.MigrationStatusUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		resp, err := s.orchestrationService.UpdateMigrationStatus(ctx, &update)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to update status: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleForecast handles forecast requests
func (s *APOLLOServer) handleForecast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	nodeName := r.URL.Query().Get("node")

	if nodeName == "" {
		// Return all predictions
		ctx := r.Context()
		predictions := s.forecastService.GenerateAllPredictions(ctx)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(predictions)
		return
	}

	// Return specific node prediction
	ctx := r.Context()
	prediction, err := s.forecastService.GeneratePrediction(ctx, nodeName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate prediction: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prediction)
}

// handleStatus handles status requests
func (s *APOLLOServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"server": "apollo-policy-server",
		"version": "1.0.0",
		"uptime": time.Since(startTime).String(),
		"services": map[string]interface{}{
			"insight": map[string]interface{}{
				"workload_signatures": s.insightService.GetWorkloadSignatureCount(),
				"cluster_insights":    s.insightService.GetClusterInsightCount(),
			},
			"scheduling": map[string]interface{}{
				"stats": s.schedulingService.GetStats(),
			},
			"orchestration": map[string]interface{}{
				"stats":             s.orchestrationService.GetStats(),
				"active_migrations": len(s.orchestrationService.GetActiveMigrations()),
			},
			"forecast": map[string]interface{}{
				"stats": s.forecastService.GetStats(),
			},
		},
		"clients": map[string]interface{}{
			"scheduler_connected":    s.schedulerClient.IsConnected(),
			"orchestrator_connected": s.orchestratorClient.IsConnected(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleStats handles stats requests
func (s *APOLLOServer) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]interface{}{
		"insight":       s.insightService.GetStats(),
		"scheduling":    s.schedulingService.GetStats(),
		"orchestration": s.orchestrationService.GetStats(),
		"forecast":      s.forecastService.GetStats(),
		"scheduler_client":    s.schedulerClient.GetStats(),
		"orchestrator_client": s.orchestratorClient.GetStats(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// Shutdown gracefully shuts down the server
func (s *APOLLOServer) Shutdown(ctx context.Context) error {
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		// Close clients
		s.schedulerClient.Close()
		s.orchestratorClient.Close()

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

var startTime = time.Now()
