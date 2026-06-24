/*
Policy Generator - Forecaster 예측 기반 OrchestrationPolicy 자동 생성
*/

package generator

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/forecaster"
)

// PolicyGenerator 정책 자동 생성기
type PolicyGenerator struct {
	client           client.Client
	kubeClient       kubernetes.Interface // Pod 노드별 목록은 API 서버 fieldSelector로만 조회 (캐시 필드 인덱스 불필요)
	forecasterClient *forecaster.Client
	namespace        string

	// 설정
	checkInterval         time.Duration
	policyTTL             time.Duration
	autoExecute           bool
	disableDuplicateCheck bool

	// 상태
	running bool
	stopCh  chan struct{}
	mu      sync.Mutex

	// 중복 정책 방지
	recentPolicies map[string]time.Time
	policyMu       sync.RWMutex
}

// Config 정책 생성기 설정
type Config struct {
	Namespace             string
	CheckInterval         time.Duration
	PolicyTTL             time.Duration
	AutoExecute           bool
	ForecasterURL         string
	DisableDuplicateCheck bool
}

// DefaultConfig 기본 설정
func DefaultConfig() Config {
	return Config{
		Namespace:             "default",
		CheckInterval:         60 * time.Second,
		PolicyTTL:             10 * time.Minute,
		AutoExecute:           false,
		ForecasterURL:         "http://node-resource-forecaster.apollo.svc.cluster.local:8080",
		DisableDuplicateCheck: false, // false: 중복정책생성 방지, true: 중복정책생성 허용(debug 용)
	}
}

// NewPolicyGenerator 새로운 정책 생성기 생성
func NewPolicyGenerator(k8sClient client.Client, kubeClient kubernetes.Interface, forecasterClient *forecaster.Client, config Config) *PolicyGenerator {
	return &PolicyGenerator{
		client:                k8sClient,
		kubeClient:            kubeClient,
		forecasterClient:      forecasterClient,
		namespace:             config.Namespace,
		checkInterval:         config.CheckInterval,
		policyTTL:             config.PolicyTTL,
		autoExecute:           config.AutoExecute,
		disableDuplicateCheck: config.DisableDuplicateCheck,
		stopCh:                make(chan struct{}),
		recentPolicies:        make(map[string]time.Time),
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
	log.Printf("║   Disable Duplicate Check: %v                              ║", g.disableDuplicateCheck)
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

	// 마이그레이션 목적지 후보 노드 이름 (control-plane 제외).
	// #13b: control-plane은 NoSchedule taint라 옮긴 Pod가 못 떠 마이그가 실패한다.
	// (분석 대상 nodes 루프는 control-plane 포함 그대로 — 목적지 선택만 제한.)
	var allNodeNames []string
	for i := range nodes {
		if isControlPlaneNode(&nodes[i]) {
			continue
		}
		allNodeNames = append(allNodeNames, nodes[i].Name)
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

	// WHY: 현재 운영 환경은 control-plane 노드도 실제 워크로드를 받는다.
	//      control-plane/master 라벨만으로 제외하면, ai-storage-master의
	//      예측/정책 생성이 누락되어 특정 노드(gpu-worker-server01)만 분석된다.
	//      따라서 스케줄 가능한 Ready 노드를 분석 대상으로 사용한다.
	var targetNodes []corev1.Node
	for _, node := range nodeList.Items {
		ready := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		targetNodes = append(targetNodes, node)
	}

	return targetNodes, nil
}

// isControlPlaneNode는 control-plane/master 노드인지 판별한다(마이그 목적지로 부적합 — NoSchedule taint).
func isControlPlaneNode(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for k := range node.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}

// workloadNamespaces는 오케스트레이션 대상으로 허용할 사용자 워크로드 네임스페이스 집합(#10).
// WORKLOAD_NAMESPACES env(콤마구분)로 설정, 기본 "ai-storage-workloads".
var workloadNamespaces = func() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("WORKLOAD_NAMESPACES"))
	if raw == "" {
		raw = "ai-storage-workloads"
	}
	m := make(map[string]bool)
	for _, n := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(n); s != "" {
			m[s] = true
		}
	}
	return m
}()

// isWorkloadNamespace는 ns가 사용자 워크로드 화이트리스트에 있는지 반환한다(#10).
func isWorkloadNamespace(ns string) bool {
	return workloadNamespaces[ns]
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

	// 노드에서 실행 중인 워크로드 찾기 (스케일링용 Deployment 이름 + 마이그레이션용 Pod 이름)
	deploymentName, podName, workloadNamespace := g.findTargetWorkload(ctx, nodeName)

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
		if !g.disableDuplicateCheck && g.isDuplicatePolicy(policyKey) {
			log.Printf("[PolicyGenerator] Skipping duplicate policy: %s", policyKey)
			continue
		}

		targetWorkload := g.resolveTargetWorkloadForRecommendation(rec, deploymentName, podName)
		if targetWorkload == "" {
			log.Printf("[PolicyGenerator] Skipping %s/%s for node %s: empty targetWorkload (deployment=%q pod=%q)",
				rec.PolicyType, rec.ResourceType, nodeName, deploymentName, podName)
			continue
		}

		// 정책 생성
		policy := g.createPolicyFromRecommendation(nodeName, rec, targetWorkload, workloadNamespace, migrationTargetNode)
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
		log.Printf("[PolicyGenerator]   Workload: %s/%s", workloadNamespace, targetWorkload)
		log.Printf("[PolicyGenerator]   Reason: %s", policy.Spec.Reason)
		log.Printf("[PolicyGenerator] ========================================")
	}
}

// migrationUnsuitableReason은 Pod가 마이그레이션 대상으로 부적합하면 그 사유를, 적합하면 빈 문자열을 반환한다.
//
// BUG-E: orchestrator는 마이그 완료 표시를 위해 대상 Pod 안에서 mkdir/touch를 exec한다.
// argo workflow-controller 같은 플랫폼/인프라 컨트롤러는 distroless 이미지라 shell이 없어 exec이 영원히 실패한다.
// 따라서 인프라 네임스페이스·컨트롤러/오퍼레이터성 Pod·distroless로 추정되는 이미지는 대상에서 제외한다.
func migrationUnsuitableReason(pod *corev1.Pod) string {
	// 이미 마이그레이션 산출물(orchestrator가 만든 optimized pod)인 Pod는 재마이그 금지.
	// 재마이그하면 orchestrator가 checkpoint-volume을 중복 추가해 optimized pod 생성이
	// "Duplicate value: checkpoint-volume"으로 실패한다. (E2E 하네스가 잡은 버그)
	if pod.Labels["migration.ai-storage/job"] == "true" {
		return "already a migration product (migration.ai-storage/job=true)"
	}

	// 플랫폼/인프라 네임스페이스(kube-* 는 호출부에서 이미 제외)
	switch pod.Namespace {
	case "argo", "argocd", "monitoring", "apollo", "keti":
		return fmt.Sprintf("infra namespace %q", pod.Namespace)
	}

	// 컨트롤러/오퍼레이터성 워크로드 휴리스틱(이름·app 라벨)
	name := strings.ToLower(pod.Name)
	appLabel := strings.ToLower(pod.Labels["app"])
	for _, kw := range []string{"workflow-controller", "-controller", "-operator", "controller-manager"} {
		if strings.Contains(name, kw) || strings.Contains(appLabel, kw) {
			return fmt.Sprintf("controller/operator workload (matched %q)", kw)
		}
	}

	// distroless 추정 이미지(shell/mkdir 없음) 제외
	for _, c := range pod.Spec.Containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "distroless") || strings.Contains(img, "workflow-controller") {
			return fmt.Sprintf("distroless-like image %q", c.Image)
		}
	}

	return ""
}

// findTargetWorkload 노드에서 실행 중인 대상 워크로드를 찾는다.
//
// 반환값은 (Deployment 이름, Pod 이름, 네임스페이스)이다. 스케일링·프로비저닝 등은 오케스트레이터가
// Deployment 리소스 이름을 기대하므로 ReplicaSet→Deployment 소유 체인을 통해 이름을 해석한다.
// controller-runtime client.List는 기본적으로 캐시를 쓰며, spec.nodeName 필드 인덱스가 없으면
// MatchingFieldsSelector 조차 "Index ... does not exist"로 실패한다. Pod 목록만 client-go로 API 서버에 직접 질의한다.
func (g *PolicyGenerator) findTargetWorkload(ctx context.Context, nodeName string) (deploymentName, podName, namespace string) {
	if g.kubeClient == nil {
		log.Printf("[PolicyGenerator] kubeClient nil, cannot list pods on node %s", nodeName)
		return "", "", ""
	}
	list, err := g.kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		log.Printf("[PolicyGenerator] Failed to list pods on node %s: %v", nodeName, err)
		return "", "", ""
	}

	var chosenPod *corev1.Pod
	var chosenDep string
	for i := range list.Items {
		pod := &list.Items[i]

		// #10 사용자 워크로드 네임스페이스만 오케스트레이션 대상으로 삼는다.
		// 인프라/플랫폼 ns(kubeflow의 cache-deployer 등)는 제외 — 사용자 잡이 없는 노드에서
		// 플랫폼 부품이 잘못 타깃되어 헛정책(scaling/provisioning)을 양산하던 문제 차단.
		if !isWorkloadNamespace(pod.Namespace) {
			continue
		}

		// 시스템 네임스페이스 제외
		if pod.Namespace == "kube-system" || pod.Namespace == "kube-public" || pod.Namespace == "kube-node-lease" {
			continue
		}

		// 마이그레이션 부적합 워크로드 제외(플랫폼/인프라 컨트롤러, distroless 등 — BUG-E)
		if reason := migrationUnsuitableReason(pod); reason != "" {
			log.Printf("[PolicyGenerator] Skipping unsuitable pod %s/%s as migration target: %s", pod.Namespace, pod.Name, reason)
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

		dep := g.resolveDeploymentName(ctx, pod)
		if dep != "" {
			chosenPod = pod
			chosenDep = dep
			break
		}
		if chosenPod == nil {
			chosenPod = pod
			chosenDep = ""
		}
	}

	if chosenPod != nil {
		log.Printf("[PolicyGenerator] Found target workload on node %s: deployment=%q pod=%s/%s",
			nodeName, chosenDep, chosenPod.Namespace, chosenPod.Name)
		return chosenDep, chosenPod.Name, chosenPod.Namespace
	}

	log.Printf("[PolicyGenerator] No suitable workload found on node %s", nodeName)
	return "", "", ""
}

// resolveDeploymentName은 Pod의 상위 ReplicaSet을 거쳐 Deployment 이름을 반환한다. 없으면 빈 문자열.
//
// controller-runtime client.Get은 내부 캐시·Informer 동기화에 의존하며, ReplicaSet은 클러스터 스코프 list/watch
// 권한이 없으면 informer가 sync되지 않아 Timeout이 난다. namespaced 단건 Get은 client-go로 API 서버에 직접 요청한다.
func (g *PolicyGenerator) resolveDeploymentName(ctx context.Context, pod *corev1.Pod) string {
	if g.kubeClient == nil {
		return ""
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "ReplicaSet" || ref.Name == "" {
			continue
		}
		rs, err := g.kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			log.Printf("[PolicyGenerator] Failed to get ReplicaSet %s/%s: %v", pod.Namespace, ref.Name, err)
			continue
		}
		for _, o := range rs.OwnerReferences {
			if o.Kind == "Deployment" && o.Name != "" {
				return o.Name
			}
		}
	}
	return ""
}

// resolveTargetWorkloadForRecommendation은 정책 유형에 맞는 targetWorkload 한 줄을 만든다.
//
// 마이그레이션은 오케스트레이터 API가 Pod 이름을 요구하고, 스케일링·프로비저닝 등은 Deployment 이름이 필요하다.
func (g *PolicyGenerator) resolveTargetWorkloadForRecommendation(rec forecaster.PolicyRecommendation, deploymentName, podName string) string {
	switch rec.PolicyType {
	case "migration":
		return podName
	default:
		return deploymentName
	}
}

// createPolicyFromRecommendation 권장 사항으로부터 정책 생성
func (g *PolicyGenerator) createPolicyFromRecommendation(nodeName string, rec forecaster.PolicyRecommendation, targetWorkload, workloadNamespace, migrationTargetNode string) *apollov1.OrchestrationPolicy {
	policyTypePart := sanitizeRFC1123NamePart(rec.PolicyType)
	nodeNamePart := sanitizeRFC1123NamePart(nodeName)
	resourceTypePart := sanitizeRFC1123NamePart(rec.ResourceType)
	timestampPart := fmt.Sprintf("%d", time.Now().Unix())
	policyName := fmt.Sprintf("%s-%s-%s-%s", policyTypePart, nodeNamePart, resourceTypePart, timestampPart)

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
		sourceNode = nodeName            // 리소스 부족 노드 (출발지)
		targetNode = migrationTargetNode // 여유 있는 노드 (목적지)
	} else {
		targetNode = nodeName // 다른 정책은 대상 노드로 설정
	}

	policy := &apollov1.OrchestrationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: g.namespace,
			Labels: map[string]string{
				"app":           "orchestration-policy-engine",
				"policy-type":   string(policyType),
				"target-node":   nodeName,
				"resource-type": string(resourceType),
				"generated-by":  "policy-generator",
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
			TargetWorkload:  targetWorkload,
			TargetNamespace: workloadNamespace,
			Reason:          rec.Reason,
			Horizon:         rec.HorizonMinutes,
			AutoExecute:     g.autoExecute,
			ExecutionMode:   defaultExecutionModeForGeneratedPolicy(policyType),
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

func defaultExecutionModeForGeneratedPolicy(policyType apollov1.PolicyType) apollov1.ExecutionMode {
	switch policyType {
	case apollov1.PolicyTypeScaling:
		return apollov1.ExecutionModeContinuous
	default:
		return apollov1.ExecutionModeImmediate
	}
}

var invalidDNS1123NamePart = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeRFC1123NamePart(v string) string {
	s := strings.ToLower(v)
	s = strings.ReplaceAll(s, "_", "-")
	s = invalidDNS1123NamePart.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "x"
	}
	return s
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
	if g.disableDuplicateCheck {
		return false
	}

	g.policyMu.RLock()
	defer g.policyMu.RUnlock()

	if createdAt, exists := g.recentPolicies[key]; exists {
		ttl := g.policyTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		// TTL 기간 내에 생성된 동일 정책이 있으면 중복
		return time.Since(createdAt) < ttl
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
