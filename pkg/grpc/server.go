// ============================================
// APOLLO gRPC Server
// Insight Trace/Scope로부터 데이터 수신
// ============================================

package grpc

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	pb "apollo/api/proto"
	"apollo/pkg/monitor"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// Server implements all APOLLO gRPC services
type Server struct {
	pb.UnimplementedInsightServiceServer
	pb.UnimplementedSchedulingPolicyServiceServer
	pb.UnimplementedOrchestrationPolicyServiceServer
	pb.UnimplementedForecastServiceServer

	grpcServer *grpc.Server
	monitor    *monitor.Monitor
	port       int

	// Data handlers (callbacks to main server)
	onWorkloadSignature func(*pb.WorkloadSignature)
	onClusterInsight    func(*pb.ClusterInsight)

	// Policy providers
	schedulingPolicyProvider    func(string, string) *pb.SchedulingPolicy
	orchestrationPolicyProvider func(string, string) *pb.OrchestrationPolicy

	// Subscribers for streaming
	policySubscribers map[string]chan *pb.OrchestrationPolicy
	subscriberMu      sync.RWMutex
}

// NewServer creates a new gRPC server
func NewServer(port int, mon *monitor.Monitor) *Server {
	return &Server{
		port:              port,
		monitor:           mon,
		policySubscribers: make(map[string]chan *pb.OrchestrationPolicy),
	}
}

// SetWorkloadSignatureHandler sets the callback for workload signatures
func (s *Server) SetWorkloadSignatureHandler(handler func(*pb.WorkloadSignature)) {
	s.onWorkloadSignature = handler
}

// SetClusterInsightHandler sets the callback for cluster insights
func (s *Server) SetClusterInsightHandler(handler func(*pb.ClusterInsight)) {
	s.onClusterInsight = handler
}

// SetSchedulingPolicyProvider sets the policy provider function
func (s *Server) SetSchedulingPolicyProvider(provider func(string, string) *pb.SchedulingPolicy) {
	s.schedulingPolicyProvider = provider
}

// SetOrchestrationPolicyProvider sets the orchestration policy provider
func (s *Server) SetOrchestrationPolicyProvider(provider func(string, string) *pb.OrchestrationPolicy) {
	s.orchestrationPolicyProvider = provider
}

// Start starts the gRPC server
func (s *Server) Start() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", s.port, err)
	}

	s.grpcServer = grpc.NewServer(
		grpc.UnaryInterceptor(s.unaryInterceptor),
		grpc.StreamInterceptor(s.streamInterceptor),
	)

	// Register all services
	pb.RegisterInsightServiceServer(s.grpcServer, s)
	pb.RegisterSchedulingPolicyServiceServer(s.grpcServer, s)
	pb.RegisterOrchestrationPolicyServiceServer(s.grpcServer, s)
	pb.RegisterForecastServiceServer(s.grpcServer, s)

	// Enable reflection for debugging
	reflection.Register(s.grpcServer)

	log.Printf("============================================")
	log.Printf("APOLLO gRPC Server starting on port %d", s.port)
	log.Printf("============================================")
	log.Printf("Registered services:")
	log.Printf("  - InsightService (WorkloadSignature, ClusterInsight)")
	log.Printf("  - SchedulingPolicyService")
	log.Printf("  - OrchestrationPolicyService")
	log.Printf("  - ForecastService")
	log.Printf("============================================")

	return s.grpcServer.Serve(lis)
}

// Stop stops the gRPC server
func (s *Server) Stop() {
	if s.grpcServer != nil {
		log.Println("Stopping gRPC server...")
		s.grpcServer.GracefulStop()
	}
}

// ============================================
// Interceptors for logging
// ============================================

func (s *Server) unaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	log.Printf("[gRPC] Unary call: %s", info.FullMethod)
	return handler(ctx, req)
}

func (s *Server) streamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	log.Printf("[gRPC] Stream started: %s", info.FullMethod)
	err := handler(srv, ss)
	log.Printf("[gRPC] Stream ended: %s", info.FullMethod)
	return err
}

// ============================================
// InsightService Implementation
// ============================================

// ReportWorkloadSignature receives a single workload signature
func (s *Server) ReportWorkloadSignature(ctx context.Context, sig *pb.WorkloadSignature) (*pb.ReportResponse, error) {
	// Log via monitor
	s.monitor.LogWorkloadSignature(
		sig.PodName,
		sig.PodNamespace,
		sig.NodeName,
		sig.WorkloadType.String(),
		sig.CurrentStage.String(),
		sig.IoPattern.String(),
		sig.Framework,
		sig.IsGpuWorkload,
		sig.Confidence,
	)

	// Call handler if set
	if s.onWorkloadSignature != nil {
		s.onWorkloadSignature(sig)
	}

	return &pb.ReportResponse{
		Success:   true,
		Message:   fmt.Sprintf("WorkloadSignature for %s/%s received", sig.PodNamespace, sig.PodName),
		RequestId: fmt.Sprintf("ws-%s-%d", sig.PodName, sig.UpdatedAt.GetSeconds()),
	}, nil
}

// StreamWorkloadSignatures receives streaming workload signatures
func (s *Server) StreamWorkloadSignatures(stream pb.InsightService_StreamWorkloadSignaturesServer) error {
	var count int64

	for {
		sig, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.StreamResponse{
				MessagesReceived:  count,
				MessagesProcessed: count,
				Status:            "completed",
			})
		}
		if err != nil {
			return err
		}

		count++

		// Log via monitor
		s.monitor.LogWorkloadSignature(
			sig.PodName,
			sig.PodNamespace,
			sig.NodeName,
			sig.WorkloadType.String(),
			sig.CurrentStage.String(),
			sig.IoPattern.String(),
			sig.Framework,
			sig.IsGpuWorkload,
			sig.Confidence,
		)

		// Call handler if set
		if s.onWorkloadSignature != nil {
			s.onWorkloadSignature(sig)
		}
	}
}

// ReportClusterInsight receives a single cluster insight
func (s *Server) ReportClusterInsight(ctx context.Context, insight *pb.ClusterInsight) (*pb.ReportResponse, error) {
	// Calculate usage percentages
	var cpuUsage, memUsage float64
	if insight.NodeResources != nil {
		if insight.NodeResources.CpuAllocatableCores > 0 {
			cpuUsage = (insight.NodeResources.CpuUsedCores / insight.NodeResources.CpuAllocatableCores) * 100
		}
		if insight.NodeResources.MemoryAllocatableBytes > 0 {
			memUsage = float64(insight.NodeResources.MemoryUsedBytes) / float64(insight.NodeResources.MemoryAllocatableBytes) * 100
		}
	}

	// Log via monitor
	s.monitor.LogClusterInsight(
		insight.NodeName,
		cpuUsage,
		memUsage,
		len(insight.StorageDevices),
		len(insight.GpuDevices),
		len(insight.CsdDevices),
		len(insight.RunningWorkloads),
	)

	// Call handler if set
	if s.onClusterInsight != nil {
		s.onClusterInsight(insight)
	}

	return &pb.ReportResponse{
		Success:   true,
		Message:   fmt.Sprintf("ClusterInsight from %s received", insight.NodeName),
		RequestId: fmt.Sprintf("ci-%s-%d", insight.NodeName, insight.CollectedAt.GetSeconds()),
	}, nil
}

// StreamClusterInsights receives streaming cluster insights
func (s *Server) StreamClusterInsights(stream pb.InsightService_StreamClusterInsightsServer) error {
	var count int64

	for {
		insight, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.StreamResponse{
				MessagesReceived:  count,
				MessagesProcessed: count,
				Status:            "completed",
			})
		}
		if err != nil {
			return err
		}

		count++

		// Calculate usage
		var cpuUsage, memUsage float64
		if insight.NodeResources != nil {
			if insight.NodeResources.CpuAllocatableCores > 0 {
				cpuUsage = (insight.NodeResources.CpuUsedCores / insight.NodeResources.CpuAllocatableCores) * 100
			}
			if insight.NodeResources.MemoryAllocatableBytes > 0 {
				memUsage = float64(insight.NodeResources.MemoryUsedBytes) / float64(insight.NodeResources.MemoryAllocatableBytes) * 100
			}
		}

		// Log via monitor
		s.monitor.LogClusterInsight(
			insight.NodeName,
			cpuUsage,
			memUsage,
			len(insight.StorageDevices),
			len(insight.GpuDevices),
			len(insight.CsdDevices),
			len(insight.RunningWorkloads),
		)

		// Call handler if set
		if s.onClusterInsight != nil {
			s.onClusterInsight(insight)
		}
	}
}

// ============================================
// SchedulingPolicyService Implementation
// ============================================

// GetSchedulingPolicy returns scheduling policy for a pod
func (s *Server) GetSchedulingPolicy(ctx context.Context, req *pb.SchedulingPolicyRequest) (*pb.SchedulingPolicy, error) {
	s.monitor.LogSchedulingRequest(req.PodName, req.PodNamespace)

	// Get policy from provider if set
	if s.schedulingPolicyProvider != nil {
		return s.schedulingPolicyProvider(req.PodName, req.PodNamespace), nil
	}

	// Default policy
	return &pb.SchedulingPolicy{
		RequestId:    fmt.Sprintf("sp-%s-%s", req.PodNamespace, req.PodName),
		PodName:      req.PodName,
		PodNamespace: req.PodNamespace,
		Decision:     pb.SchedulingDecision_SCHEDULING_DECISION_ALLOW,
		Reason:       "No specific policy, allowing scheduling",
	}, nil
}

// ReportSchedulingResult receives scheduling result feedback
func (s *Server) ReportSchedulingResult(ctx context.Context, result *pb.SchedulingResult) (*pb.ReportResponse, error) {
	status := "success"
	if !result.Success {
		status = "failed: " + result.FailureReason
	}

	log.Printf("[gRPC] SchedulingResult: %s/%s -> %s (%s, %dms)",
		result.PodNamespace, result.PodName, result.ScheduledNode, status, result.SchedulingDurationMs)

	return &pb.ReportResponse{
		Success:   true,
		Message:   "Scheduling result recorded",
		RequestId: result.RequestId,
	}, nil
}

// ============================================
// OrchestrationPolicyService Implementation
// ============================================

// SubscribePolicies streams orchestration policies to subscribers
func (s *Server) SubscribePolicies(req *pb.PolicySubscription, stream pb.OrchestrationPolicyService_SubscribePoliciesServer) error {
	log.Printf("[gRPC] New policy subscriber: %s", req.SubscriberId)

	// Create channel for this subscriber
	policyChan := make(chan *pb.OrchestrationPolicy, 100)

	s.subscriberMu.Lock()
	s.policySubscribers[req.SubscriberId] = policyChan
	s.subscriberMu.Unlock()

	defer func() {
		s.subscriberMu.Lock()
		delete(s.policySubscribers, req.SubscriberId)
		close(policyChan)
		s.subscriberMu.Unlock()
		log.Printf("[gRPC] Policy subscriber disconnected: %s", req.SubscriberId)
	}()

	// Stream policies to subscriber
	for policy := range policyChan {
		if err := stream.Send(policy); err != nil {
			return err
		}
	}

	return nil
}

// BroadcastOrchestrationPolicy sends a policy to all subscribers
func (s *Server) BroadcastOrchestrationPolicy(policy *pb.OrchestrationPolicy) {
	s.subscriberMu.RLock()
	defer s.subscriberMu.RUnlock()

	for id, ch := range s.policySubscribers {
		select {
		case ch <- policy:
			log.Printf("[gRPC] Policy sent to subscriber: %s", id)
		default:
			log.Printf("[gRPC] Subscriber %s buffer full, skipping", id)
		}
	}
}

// GetOrchestrationPolicy returns orchestration policy for a workload
func (s *Server) GetOrchestrationPolicy(ctx context.Context, req *pb.OrchestrationPolicyRequest) (*pb.OrchestrationPolicy, error) {
	s.monitor.LogOrchestrationEvent("policy_request", req.PodName, req.PodNamespace, req.RequestedAction.String())

	// Get policy from provider if set
	if s.orchestrationPolicyProvider != nil {
		return s.orchestrationPolicyProvider(req.PodName, req.PodNamespace), nil
	}

	// Default policy
	return &pb.OrchestrationPolicy{
		RequestId: fmt.Sprintf("op-%s-%s", req.PodNamespace, req.PodName),
		Action:    pb.OrchestrationAction_ORCHESTRATION_ACTION_UNSPECIFIED,
		Priority:  pb.PolicyPriority_POLICY_PRIORITY_MEDIUM,
		Reason:    "No action required",
	}, nil
}

// ReportExecutionResult receives execution result feedback
func (s *Server) ReportExecutionResult(ctx context.Context, result *pb.ExecutionResult) (*pb.ReportResponse, error) {
	status := "success"
	if !result.Success {
		status = "failed: " + result.FailureReason
	}

	log.Printf("[gRPC] ExecutionResult: %s (%s, %dms)",
		result.Action.String(), status, result.ExecutionDurationMs)

	s.monitor.LogOrchestrationEvent("execution_result", "", "", fmt.Sprintf("%s: %s", result.Action.String(), status))

	return &pb.ReportResponse{
		Success:   true,
		Message:   "Execution result recorded",
		RequestId: result.RequestId,
	}, nil
}

// ============================================
// ForecastService Implementation
// ============================================

// GetResourcePrediction returns resource prediction for a node
func (s *Server) GetResourcePrediction(ctx context.Context, req *pb.PredictionRequest) (*pb.ResourcePrediction, error) {
	log.Printf("[gRPC] ResourcePrediction request for node: %s (horizon: %d min)", req.NodeName, req.HorizonMinutes)

	// Return placeholder prediction
	return &pb.ResourcePrediction{
		NodeName:                  req.NodeName,
		PredictionId:             fmt.Sprintf("pred-%s", req.NodeName),
		PredictionHorizonMinutes: req.HorizonMinutes,
		ConfidenceLevel:          req.ConfidenceLevel,
		ModelType:                "simple",
	}, nil
}

// GetClusterPrediction returns cluster-wide prediction
func (s *Server) GetClusterPrediction(ctx context.Context, req *pb.ClusterPredictionRequest) (*pb.ClusterPrediction, error) {
	log.Printf("[gRPC] ClusterPrediction request (horizon: %d min)", req.HorizonMinutes)

	return &pb.ClusterPrediction{
		NodePredictions: []*pb.ResourcePrediction{},
	}, nil
}
