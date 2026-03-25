/*
Policy Generator - Forecaster 예측 기반 OrchestrationPolicy 자동 생성
*/

package generator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/forecaster"
)

// PolicyGenerator 정책 자동 생성기
type PolicyGenerator struct {
	client           client.Client
	forecasterClient *forecaster.Client
	namespace        string

	// 설정
	checkInterval    time.Duration
	policyTTL        time.Duration
	autoExecute      bool

	// 상태
	running     bool
	stopCh      chan struct{}
	mu          sync.Mutex

	// 중복 정책 방지
	recentPolicies map[string]time.Time
	policyMu       sync.RWMutex
}

// Config 정책 생성기 설정
type Config struct {
	Namespace        string
	CheckInterval    time.Duration
	PolicyTTL        time.Duration
	AutoExecute      bool
	ForecasterURL    string
}

// DefaultConfig 기본 설정
func DefaultConfig() Config {
	return Config{
		Namespace:        "default",
		CheckInterval:    60 * time.Second,
		PolicyTTL:        10 * time.Minute,
		AutoExecute:      false,
		ForecasterURL:    "http://node-resource-forecaster.apollo.svc.cluster.local:8080",
	}
}

// NewPolicyGenerator 새로운 정책 생성기 생성
func NewPolicyGenerator(k8sClient client.Client, forecasterClient *forecaster.Client, config Config) *PolicyGenerator {
	return &PolicyGenerator{
		client:           k8sClient,
		forecasterClient: forecasterClient,
		namespace:        config.Namespace,
		checkInterval:    config.CheckInterval,
		policyTTL:        config.PolicyTTL,
		autoExecute:      config.AutoExecute,
		stopCh:           make(chan struct{}),
		recentPolicies:   make(map[string]time.Time),
	}
}

// Start 정책 생성기 시작
func (g *PolicyGenerator) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return fmt.Errorf("policy generator already running")
	}
	g.running = true
	g.mu.Unlock()

	log.Println("╔═══════════════════════════════════════════════════════════════╗")
	log.Println("║          Policy Generator Started                             ║")
	log.Printf("║   Check Interval: %v                                    ║", g.checkInterval)
	log.Printf("║   Auto Execute: %v                                          ║", g.autoExecute)
	log.Println("╚═══════════════════════════════════════════════════════════════╝")

	ticker := time.NewTicker(g.checkInterval)
	defer ticker.Stop()

	// 시작 시 즉시 한 번 실행
	g.runPolicyGeneration(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("[PolicyGenerator] Context cancelled, stopping")
			return ctx.Err()
		case <-g.stopCh:
			log.Println("[PolicyGenerator] Stop signal received")
			return nil
		case <-ticker.C:
			g.runPolicyGeneration(ctx)
		}
	}
}

// Stop 정책 생성기 중지
func (g *PolicyGenerator) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running {
		close(g.stopCh)
		g.running = false
		log.Println("[PolicyGenerator] Stopped")
	}
}

// runPolicyGeneration 정책 생성 실행
func (g *PolicyGenerator) runPolicyGeneration(ctx context.Context) {
	log.Println("============================================")
	log.Println("[PolicyGenerator] Running policy generation cycle")

	// 1. 노드 목록 조회
	nodes, err := g.getNodes(ctx)
	if err != nil {
		log.Printf("[PolicyGenerator] Failed to get nodes: %v", err)
		return
	}

	log.Printf("[PolicyGenerator] Found %d nodes", len(nodes))

	// 노드 이름 목록 생성 (마이그레이션 대상 노드 선택에 사용)
	var allNodeNames []string
	for _, node := range nodes {
		allNodeNames = append(allNodeNames, node.Name)
	}

	// 2. 각 노드에 대해 예측 분석 및 정책 생성
	for _, node := range nodes {
		g.analyzeAndCreatePolicy(ctx, node.Name, allNodeNames)
	}

	// 3. 오래된 정책 기록 정리
	g.cleanupOldPolicyRecords()

	log.Println("[PolicyGenerator] Policy generation cycle completed")
	log.Println("============================================")
}

// getNodes 노드 목록 조회
func (g *PolicyGenerator) getNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := g.client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Worker 노드만 필터링 (Control Plane 제외)
	var workerNodes []corev1.Node
	for _, node := range nodeList.Items {
		isControlPlane := false
		for key := range node.Labels {
			if key == "node-role.kubernetes.io/control-plane" || key == "node-role.kubernetes.io/master" {
				isControlPlane = true
				break
			}
		}
		if !isControlPlane {
			workerNodes = append(workerNodes, node)
		}
	}

	return workerNodes, nil
}

// analyzeAndCreatePolicy 노드 분석 및 정책 생성
func (g *PolicyGenerator) analyzeAndCreatePolicy(ctx context.Context, nodeName string, allNodeNames []string) {
	log.Printf("[PolicyGenerator] Analyzing node: %s", nodeName)

	// Forecaster에서 예측 분석 및 권장 사항 조회
	recommendations, err := g.forecasterClient.AnalyzeAndRecommend(ctx, nodeName)
	if err != nil {
		log.Printf("[PolicyGenerator] Failed to analyze node %s: %v", nodeName, err)
		return
	}

	if len(recommendations) == 0 {
		log.Printf("[PolicyGenerator] No policy recommendations for node %s", nodeName)
		return
	}

	log.Printf("[PolicyGenerator] Got %d recommendations for node %s", len(recommendations), nodeName)

	// 노드에서 실행 중인 워크로드 찾기
	workloadName, workloadNamespace := g.findTargetWorkload(ctx, nodeName)

	// 마이그레이션 대상 노드 선택 (현재 노드 제외)
	var migrationTargetNode string
	for _, n := range allNodeNames {
		if n != nodeName {
			migrationTargetNode = n
			break
		}
	}

	// 권장 사항별로 정책 생성
	for _, rec := range recommendations {
		// 중복 정책 확인
		policyKey := fmt.Sprintf("%s-%s-%s", nodeName, rec.PolicyType, rec.ResourceType)
		if g.isDuplicatePolicy(policyKey) {
			log.Printf("[PolicyGenerator] Skipping duplicate policy: %s", policyKey)
			continue
		}

		// 정책 생성
		policy := g.createPolicyFromRecommendation(nodeName, rec, workloadName, workloadNamespace, migrationTargetNode)
		if err := g.client.Create(ctx, policy); err != nil {
			log.Printf("[PolicyGenerator] Failed to create policy: %v", err)
			continue
		}

		// 정책 기록
		g.recordPolicy(policyKey)

		log.Printf("[PolicyGenerator] ========================================")
		log.Printf("[PolicyGenerator] Created policy: %s", policy.Name)
		log.Printf("[PolicyGenerator]   Type: %s", policy.Spec.PolicyType)
		log.Printf("[PolicyGenerator]   Urgency: %s", policy.Spec.Urgency)
		log.Printf("[PolicyGenerator]   Probability: %d%%", policy.Spec.Probability)
		log.Printf("[PolicyGenerator]   Resource: %s", policy.Spec.ResourceType)
		log.Printf("[PolicyGenerator]   Workload: %s/%s", workloadNamespace, workloadName)
		log.Printf("[PolicyGenerator]   Reason: %s", policy.Spec.Reason)
		log.Printf("[PolicyGenerator] ========================================")
	}
}

// findTargetWorkload 노드에서 실행 중인 대상 워크로드 찾기
func (g *PolicyGenerator) findTargetWorkload(ctx context.Context, nodeName string) (string, string) {
	// 노드에서 실행 중인 Pod 목록 조회
	podList := &corev1.PodList{}
	if err := g.client.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		log.Printf("[PolicyGenerator] Failed to list pods on node %s: %v", nodeName, err)
		// 기본값 반환 - 노드의 모든 워크로드 대상
		return "all-workloads", "default"
	}

	// 실행 중인 Pod 중 시스템 Pod 제외하고 가장 오래된 Pod 선택
	var targetPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]

		// 시스템 네임스페이스 제외
		if pod.Namespace == "kube-system" || pod.Namespace == "kube-public" || pod.Namespace == "kube-node-lease" {
			continue
		}

		// Running 상태의 Pod만
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// DaemonSet Pod 제외 (monitoring 등)
		if pod.OwnerReferences != nil {
			isDaemonSet := false
			for _, ref := range pod.OwnerReferences {
				if ref.Kind == "DaemonSet" {
					isDaemonSet = true
					break
				}
			}
			if isDaemonSet {
				continue
			}
		}

		// 첫 번째 적합한 Pod 선택 (또는 가장 리소스를 많이 요청하는 Pod)
		if targetPod == nil {
			targetPod = pod
		}
	}

	if targetPod != nil {
		log.Printf("[PolicyGenerator] Found target workload: %s/%s on node %s",
			targetPod.Namespace, targetPod.Name, nodeName)
		return targetPod.Name, targetPod.Namespace
	}

	// 적합한 Pod이 없으면 기본값
	log.Printf("[PolicyGenerator] No suitable workload found on node %s, using default", nodeName)
	return "node-workloads", "default"
}

// createPolicyFromRecommendation 권장 사항으로부터 정책 생성
func (g *PolicyGenerator) createPolicyFromRecommendation(nodeName string, rec forecaster.PolicyRecommendation, workloadName, workloadNamespace, migrationTargetNode string) *apollov1.OrchestrationPolicy {
	// RFC 1123 규칙: 소문자, 숫자, '-'만 허용
	policyName := strings.ToLower(fmt.Sprintf("%s-%s-%s-%d", rec.PolicyType, nodeName, rec.ResourceType, time.Now().Unix()))

	// PolicyType 변환
	var policyType apollov1.PolicyType
	switch rec.PolicyType {
	case "migration":
		policyType = apollov1.PolicyTypeMigration
	case "scaling":
		policyType = apollov1.PolicyTypeScaling
	case "provisioning":
		policyType = apollov1.PolicyTypeProvisioning
	case "caching":
		policyType = apollov1.PolicyTypeCaching
	case "loadbalance":
		policyType = apollov1.PolicyTypeLoadbalance
	case "preemption":
		policyType = apollov1.PolicyTypePreemption
	default:
		policyType = apollov1.PolicyTypeMigration
	}

	// Urgency 변환
	var urgency apollov1.Urgency
	switch rec.Urgency {
	case "LOW":
		urgency = apollov1.UrgencyLow
	case "MEDIUM":
		urgency = apollov1.UrgencyMedium
	case "HIGH":
		urgency = apollov1.UrgencyHigh
	case "CRITICAL":
		urgency = apollov1.UrgencyCritical
	default:
		urgency = apollov1.UrgencyMedium
	}

	// ResourceType 변환
	var resourceType apollov1.ResourceType
	switch rec.ResourceType {
	case "CPU":
		resourceType = apollov1.ResourceTypeCPU
	case "MEMORY":
		resourceType = apollov1.ResourceTypeMemory
	case "GPU":
		resourceType = apollov1.ResourceTypeGPU
	case "STORAGE_IO":
		resourceType = apollov1.ResourceTypeStorageIO
	default:
		resourceType = apollov1.ResourceTypeCPU
	}

	// 마이그레이션 정책의 경우 SourceNode와 TargetNode 설정
	var sourceNode, targetNode string
	if policyType == apollov1.PolicyTypeMigration {
		sourceNode = nodeName                // 리소스 부족 노드 (출발지)
		targetNode = migrationTargetNode     // 여유 있는 노드 (목적지)
	} else {
		targetNode = nodeName                // 다른 정책은 대상 노드로 설정
	}

	policy := &apollov1.OrchestrationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: g.namespace,
			Labels: map[string]string{
				"app":                    "orchestration-policy-engine",
				"policy-type":            string(policyType),
				"target-node":            nodeName,
				"resource-type":          string(resourceType),
				"generated-by":           "policy-generator",
			},
			Annotations: map[string]string{
				"apollo.keti.re.kr/predicted-utilization": fmt.Sprintf("%.2f", rec.PredictedUtilization),
				"apollo.keti.re.kr/threshold":             fmt.Sprintf("%.2f", rec.Threshold),
				"apollo.keti.re.kr/horizon-minutes":       fmt.Sprintf("%d", rec.HorizonMinutes),
			},
		},
		Spec: apollov1.OrchestrationPolicySpec{
			PolicyType:      policyType,
			Probability:     rec.Probability,
			Urgency:         urgency,
			ResourceType:    resourceType,
			SourceNode:      sourceNode,
			TargetNode:      targetNode,
			TargetWorkload:  workloadName,
			TargetNamespace: workloadNamespace,
			Reason:          rec.Reason,
			Horizon:         rec.HorizonMinutes,
			AutoExecute:     g.autoExecute,
			PriorityScore:   g.calculatePriorityScore(rec),
			Parameters: map[string]string{
				"predicted_utilization": fmt.Sprintf("%.2f", rec.PredictedUtilization),
				"threshold":             fmt.Sprintf("%.2f", rec.Threshold),
				"generated_at":          time.Now().Format(time.RFC3339),
			},
		},
	}

	return policy
}

// calculatePriorityScore 우선순위 점수 계산
func (g *PolicyGenerator) calculatePriorityScore(rec forecaster.PolicyRecommendation) int32 {
	score := int32(50) // 기본 점수

	// Urgency에 따른 가중치
	switch rec.Urgency {
	case "CRITICAL":
		score += 40
	case "HIGH":
		score += 25
	case "MEDIUM":
		score += 10
	}

	// 확률에 따른 가중치
	score += rec.Probability / 10

	// Horizon에 따른 가중치 (급할수록 높음)
	if rec.HorizonMinutes <= 5 {
		score += 15
	} else if rec.HorizonMinutes <= 10 {
		score += 10
	} else if rec.HorizonMinutes <= 30 {
		score += 5
	}

	// 최대 100으로 제한
	if score > 100 {
		score = 100
	}

	return score
}

// isDuplicatePolicy 중복 정책 확인
func (g *PolicyGenerator) isDuplicatePolicy(key string) bool {
	g.policyMu.RLock()
	defer g.policyMu.RUnlock()

	if createdAt, exists := g.recentPolicies[key]; exists {
		// 5분 이내에 생성된 동일 정책이 있으면 중복
		return time.Since(createdAt) < 5*time.Minute
	}
	return false
}

// recordPolicy 정책 기록
func (g *PolicyGenerator) recordPolicy(key string) {
	g.policyMu.Lock()
	defer g.policyMu.Unlock()
	g.recentPolicies[key] = time.Now()
}

// cleanupOldPolicyRecords 오래된 정책 기록 정리
func (g *PolicyGenerator) cleanupOldPolicyRecords() {
	g.policyMu.Lock()
	defer g.policyMu.Unlock()

	for key, createdAt := range g.recentPolicies {
		if time.Since(createdAt) > g.policyTTL {
			delete(g.recentPolicies, key)
		}
	}
}

// SetAutoExecute AutoExecute 설정 변경
func (g *PolicyGenerator) SetAutoExecute(autoExecute bool) {
	g.autoExecute = autoExecute
	log.Printf("[PolicyGenerator] AutoExecute set to: %v", autoExecute)
}

// SetCheckInterval 체크 간격 설정 변경
func (g *PolicyGenerator) SetCheckInterval(interval time.Duration) {
	g.checkInterval = interval
	log.Printf("[PolicyGenerator] Check interval set to: %v", interval)
}
