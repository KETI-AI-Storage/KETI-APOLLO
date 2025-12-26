# APOLLO - Adaptive Policy Optimization for Low-Latency I/O

KETI AI Storage System의 정책 최적화 엔진

## Architecture

```
                    ┌─────────────────────────────────────────────────────────────┐
                    │                         APOLLO                              │
                    │  ┌─────────────────┐ ┌──────────────────┐ ┌──────────────┐ │
                    │  │ Node Resource   │ │ Scheduling Policy│ │Orchestration │ │
Insight Trace ────────>│   Forecaster    │ │     Engine       │ │Policy Engine │ │
Insight Scope ────────>│                 │ │                  │ │              │ │
                    │  └────────┬────────┘ └────────┬─────────┘ └──────┬───────┘ │
                    └───────────│───────────────────│──────────────────│─────────┘
                                │                   │                  │
                                v                   v                  v
                    ┌───────────────────┐ ┌─────────────────┐ ┌────────────────┐
                    │  Cluster State    │ │  AI-Storage     │ │  AI-Storage    │
                    │  Prediction       │ │  Scheduler      │ │  Orchestrator  │
                    └───────────────────┘ └─────────────────┘ └────────────────┘
```

## Modules

### 1. Node Resource Forecaster
노드 자원 사용량 예측 (LSTM/Prophet/ARIMA 기반)
- 클러스터 리소스 사용량 예측
- 이상 탐지 (Anomaly Detection)
- 과부하 사전 경고

### 2. Scheduling Policy Engine
AI 워크로드 스케줄링 정책 결정
- 워크로드 특성 기반 노드 선택
- CSD/GPU 자원 고려
- AI-Storage Scheduler와 gRPC 연동

### 3. Orchestration Policy Engine
오케스트레이션 정책 결정
- Pod 마이그레이션 결정
- 오토스케일링 트리거
- 스토리지 프로비저닝
- AI-Storage Orchestrator와 gRPC 연동

## Data Flow

```
[Insight Trace] ─────> WorkloadSignature ─────> APOLLO
    (Sidecar)           (gRPC Stream)           │
                                                v
[Insight Scope] ─────> ClusterInsight ───────> Policy Engines
   (DaemonSet)          (gRPC Stream)           │
                                                v
                                    ┌───────────┴───────────┐
                                    v                       v
                            SchedulingPolicy        OrchestrationPolicy
                                    │                       │
                                    v                       v
                            AI-Storage Scheduler    AI-Storage Orchestrator
```

## Project Structure

```
apollo/
├── api/
│   └── proto/
│       └── apollo.proto        # gRPC 프로토콜 정의
├── cmd/
│   └── main.go                 # 메인 엔트리포인트
├── pkg/
│   ├── forecaster/             # Node Resource Forecaster
│   │   └── forecast_service.go
│   ├── scheduling/             # Scheduling Policy Engine
│   │   ├── scheduling_service.go
│   │   └── scheduler_client.go
│   ├── orchestration/          # Orchestration Policy Engine
│   │   ├── orchestration_service.go
│   │   └── orchestrator_client.go
│   ├── insight/                # Insight 데이터 수신
│   │   └── insight_service.go
│   └── types/
│       └── types.go            # 공통 타입 정의
├── deployments/                # Kubernetes 배포 매니페스트
└── scripts/
    ├── build.sh
    ├── deploy.sh
    └── generate-proto.sh
```

## gRPC Services

### InsightService (데이터 수집)
```protobuf
service InsightService {
  rpc ReportWorkloadSignature(WorkloadSignature) returns (ReportResponse);
  rpc StreamWorkloadSignatures(stream WorkloadSignature) returns (StreamResponse);
  rpc ReportClusterInsight(ClusterInsight) returns (ReportResponse);
  rpc StreamClusterInsights(stream ClusterInsight) returns (StreamResponse);
}
```

### SchedulingPolicyService
```protobuf
service SchedulingPolicyService {
  rpc GetSchedulingPolicy(SchedulingPolicyRequest) returns (SchedulingPolicy);
  rpc ReportSchedulingResult(SchedulingResult) returns (ReportResponse);
}
```

### OrchestrationPolicyService
```protobuf
service OrchestrationPolicyService {
  rpc SubscribePolicies(PolicySubscription) returns (stream OrchestrationPolicy);
  rpc GetOrchestrationPolicy(OrchestrationPolicyRequest) returns (OrchestrationPolicy);
  rpc ReportExecutionResult(ExecutionResult) returns (ReportResponse);
}
```

## Build & Deploy

```bash
# Build
./scripts/build.sh

# Deploy to Kubernetes
./scripts/deploy.sh

# Generate protobuf
./scripts/generate-proto.sh
```

## Related Projects

- [AI-Storage-Scheduler](https://github.com/KETI-AI-Storage/ai-storage-scheduler) - Custom K8s Scheduler
- [AI-Storage-Orchestrator](https://github.com/KETI-AI-Storage/ai-storage-orchestrator) - Pod Migration Controller
- [Insight-Trace](https://github.com/KETI-AI-Storage/insight-trace) - Workload Signature Collector (Sidecar)
- [Insight-Scope](https://github.com/KETI-AI-Storage/insight-scope) - Cluster State Collector (DaemonSet)

## Research

This project is supported by IITP (Institute of Information & Communications Technology Planning & Evaluation), funded by Korea government (MSIT):
- Grant No. RS-2024-00461572
- Project: "Development of High-efficiency Parallel Storage SW Technology Optimized for AI Computational Accelerators"
