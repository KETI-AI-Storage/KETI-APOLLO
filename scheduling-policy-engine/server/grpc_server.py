"""
============================================
Scheduling Policy Engine gRPC Server
Scheduler 인터페이스(SchedulingPolicyService)에 맞춤
============================================

실제 클러스터/워크로드 데이터 기반 동적 Feature 생성
- Kubernetes API에서 노드/Pod 상태 조회
- Pod spec에서 리소스 요청 추출
- Argo Workflow/Kubeflow 메타데이터 파싱
"""

import grpc
from concurrent import futures
import logging
import uuid
import time
import torch
import numpy as np
from typing import Dict, Optional, List, Tuple
import threading
import os
import re
import hashlib

# Kubernetes client
from kubernetes import client, config as k8s_config
from kubernetes.client.rest import ApiException

# Add parent directory to path for imports
import sys
sys.path.insert(0, '/app')

# Proto imports (generated at /app/)
import apollo_pb2
import apollo_pb2_grpc

try:
    import forecaster_pb2
    import forecaster_pb2_grpc
    FORECASTER_PROTO_AVAILABLE = True
except ImportError:
    FORECASTER_PROTO_AVAILABLE = False
    forecaster_pb2 = None
    forecaster_pb2_grpc = None

from models.ppo import SchedulingPolicyPPO, PPOConfig, PPOTrainer, calculate_reward

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


class ClusterStateManager:
    """실시간 클러스터 상태 관리자"""

    def __init__(self, refresh_interval: int = 30):
        self.refresh_interval = refresh_interval
        self.last_refresh = 0
        self.node_states: Dict[str, Dict] = {}
        self.cluster_metrics: Dict = {}
        self.lock = threading.Lock()

        # Kubernetes client 초기화
        try:
            k8s_config.load_incluster_config()
            logger.info("Loaded in-cluster Kubernetes config")
        except:
            try:
                k8s_config.load_kube_config()
                logger.info("Loaded local Kubernetes config")
            except Exception as e:
                logger.warning(f"Failed to load Kubernetes config: {e}")

        self.core_api = client.CoreV1Api()
        self.custom_api = client.CustomObjectsApi()

    def get_cluster_state(self) -> Tuple[Dict[str, Dict], Dict]:
        """클러스터 상태 조회 (캐시 사용)"""
        current_time = time.time()

        with self.lock:
            if current_time - self.last_refresh > self.refresh_interval:
                self._refresh_cluster_state()
                self.last_refresh = current_time

            return self.node_states.copy(), self.cluster_metrics.copy()

    def _refresh_cluster_state(self):
        """Kubernetes API에서 클러스터 상태 갱신"""
        try:
            # 노드 목록 조회
            nodes = self.core_api.list_node()

            total_cpu_capacity = 0
            total_cpu_used = 0
            total_mem_capacity = 0
            total_mem_used = 0
            total_gpu_capacity = 0
            total_gpu_used = 0

            for node in nodes.items:
                node_name = node.metadata.name

                # 노드 리소스 capacity
                capacity = node.status.capacity or {}
                allocatable = node.status.allocatable or {}

                cpu_capacity = self._parse_cpu(capacity.get('cpu', '0'))
                mem_capacity = self._parse_memory(capacity.get('memory', '0'))
                gpu_capacity = int(capacity.get('nvidia.com/gpu', 0))

                cpu_allocatable = self._parse_cpu(allocatable.get('cpu', '0'))
                mem_allocatable = self._parse_memory(allocatable.get('memory', '0'))

                # 노드 labels로 타입 판단
                labels = node.metadata.labels or {}
                node_type = 'compute'
                if 'gpu' in node_name.lower() or gpu_capacity > 0:
                    node_type = 'gpu'
                elif 'csd' in node_name.lower() or 'storage' in node_name.lower():
                    node_type = 'storage'
                elif labels.get('node-role.kubernetes.io/control-plane') is not None:
                    node_type = 'control-plane'

                # 노드 conditions
                conditions = {c.type: c.status for c in (node.status.conditions or [])}
                is_ready = conditions.get('Ready') == 'True'

                # 스토리지 티어 (labels에서)
                storage_tier = labels.get('storage-tier', 'standard')

                self.node_states[node_name] = {
                    'cpu_capacity': cpu_capacity,
                    'cpu_allocatable': cpu_allocatable,
                    'mem_capacity': mem_capacity,
                    'mem_allocatable': mem_allocatable,
                    'gpu_capacity': gpu_capacity,
                    'node_type': node_type,
                    'storage_tier': storage_tier,
                    'is_ready': is_ready,
                    'labels': labels,
                }

                if is_ready:
                    total_cpu_capacity += cpu_capacity
                    total_mem_capacity += mem_capacity
                    total_gpu_capacity += gpu_capacity

            # Pod에서 실제 사용량 계산
            pods = self.core_api.list_pod_for_all_namespaces(field_selector='status.phase=Running')
            node_cpu_used = {}
            node_mem_used = {}

            for pod in pods.items:
                node_name = pod.spec.node_name
                if not node_name:
                    continue

                for container in (pod.spec.containers or []):
                    requests = container.resources.requests or {} if container.resources else {}
                    cpu_req = self._parse_cpu(requests.get('cpu', '0'))
                    mem_req = self._parse_memory(requests.get('memory', '0'))

                    node_cpu_used[node_name] = node_cpu_used.get(node_name, 0) + cpu_req
                    node_mem_used[node_name] = node_mem_used.get(node_name, 0) + mem_req

                    total_cpu_used += cpu_req
                    total_mem_used += mem_req

            # 노드별 사용률 업데이트
            for node_name, state in self.node_states.items():
                state['cpu_used'] = node_cpu_used.get(node_name, 0)
                state['mem_used'] = node_mem_used.get(node_name, 0)
                state['cpu_utilization'] = state['cpu_used'] / max(state['cpu_allocatable'], 0.001)
                state['mem_utilization'] = state['mem_used'] / max(state['mem_allocatable'], 0.001)

            # 클러스터 전체 메트릭
            self.cluster_metrics = {
                'total_nodes': len([n for n in self.node_states.values() if n['is_ready']]),
                'cpu_utilization': total_cpu_used / max(total_cpu_capacity, 0.001),
                'mem_utilization': total_mem_used / max(total_mem_capacity, 0.001),
                'gpu_available': total_gpu_capacity,
                'gpu_nodes': len([n for n in self.node_states.values() if n['gpu_capacity'] > 0]),
                'storage_nodes': len([n for n in self.node_states.values() if n['node_type'] == 'storage']),
            }

            logger.debug(f"Cluster state refreshed: {len(self.node_states)} nodes, "
                        f"CPU util={self.cluster_metrics['cpu_utilization']:.2%}")

        except ApiException as e:
            logger.error(f"Kubernetes API error: {e}")
        except Exception as e:
            logger.error(f"Error refreshing cluster state: {e}")

    def _parse_cpu(self, cpu_str: str) -> float:
        """CPU 문자열 파싱 (cores 단위로 변환)"""
        if not cpu_str:
            return 0.0
        cpu_str = str(cpu_str)
        if cpu_str.endswith('m'):
            return float(cpu_str[:-1]) / 1000
        elif cpu_str.endswith('n'):
            return float(cpu_str[:-1]) / 1e9
        return float(cpu_str)

    def _parse_memory(self, mem_str: str) -> float:
        """Memory 문자열 파싱 (GB 단위로 변환)"""
        if not mem_str:
            return 0.0
        mem_str = str(mem_str)
        multipliers = {'Ki': 1024, 'Mi': 1024**2, 'Gi': 1024**3, 'Ti': 1024**4,
                      'K': 1000, 'M': 1000**2, 'G': 1000**3, 'T': 1000**4}
        for suffix, mult in multipliers.items():
            if mem_str.endswith(suffix):
                return float(mem_str[:-len(suffix)]) * mult / (1024**3)
        return float(mem_str) / (1024**3)


class KueueStateManager:
    """Kueue 큐 상태 관리자"""

    def __init__(self):
        try:
            k8s_config.load_incluster_config()
        except:
            try:
                k8s_config.load_kube_config()
            except:
                pass
        self.custom_api = client.CustomObjectsApi()
        self.queue_states: Dict[str, Dict] = {}
        # ClusterQueue 쿼터 캐시: clusterqueue_name → quota_used (0.0~1.0)
        self._cq_quota_cache: Dict[str, float] = {}
        self._cq_last_refresh = 0.0
        self.last_refresh = 0
        self.lock = threading.Lock()

    def get_queue_state(self, queue_name: str = 'ai-storage-queue') -> Dict:
        """큐 상태 조회"""
        current_time = time.time()

        with self.lock:
            if current_time - self.last_refresh > 10:  # 10초마다 갱신
                self._refresh_clusterqueue_quota()
                self._refresh_queue_state()
                self.last_refresh = current_time

            return self.queue_states.get(queue_name, {
                'pending': 0, 'admitted': 0, 'active': 0, 'quota_used': 0.0
            })

    def _refresh_clusterqueue_quota(self):
        """ClusterQueue의 실제 쿼터 사용률을 계산해 캐시합니다.

        Kueue ClusterQueue status 구조:
          status.flavorsUsage[].resources[].total  (현재 사용량)
          spec.resourceGroups[].flavors[].resources[].nominalQuota  (할당 총량)

        cpu/memory/gpu 리소스를 기준으로 가중 평균 사용률을 계산합니다.
        """
        try:
            cqs = self.custom_api.list_cluster_custom_object(
                group='kueue.x-k8s.io',
                version='v1beta1',
                plural='clusterqueues'
            )
        except Exception as e:
            logger.debug(f"[KueueManager] ClusterQueue fetch failed: {e}")
            return

        for cq in cqs.get('items', []):
            name = cq['metadata']['name']
            spec = cq.get('spec', {})
            status = cq.get('status', {})

            # --- 총 쿼터(nominalQuota) 수집 ---
            nominal: Dict[str, float] = {}
            for rg in spec.get('resourceGroups', []):
                for flavor in rg.get('flavors', []):
                    for res in flavor.get('resources', []):
                        res_name = res.get('name', '')
                        quota_str = str(res.get('nominalQuota', '0'))
                        val = self._parse_resource_quantity(res_name, quota_str)
                        nominal[res_name] = nominal.get(res_name, 0.0) + val

            # --- 현재 사용량(flavorsUsage) 수집 ---
            used: Dict[str, float] = {}
            for flavor_usage in status.get('flavorsUsage', []):
                for res in flavor_usage.get('resources', []):
                    res_name = res.get('name', '')
                    total_str = str(res.get('total', '0'))
                    val = self._parse_resource_quantity(res_name, total_str)
                    used[res_name] = used.get(res_name, 0.0) + val

            # --- 가중 평균 쿼터 사용률 계산 ---
            # cpu(가중치 3), memory(가중치 2), gpu(가중치 5) 순으로 반영
            weights = {'cpu': 3.0, 'memory': 2.0, 'nvidia.com/gpu': 5.0}
            total_weight = 0.0
            weighted_usage = 0.0

            for res_name, weight in weights.items():
                nom = nominal.get(res_name, 0.0)
                if nom <= 0:
                    continue
                ratio = min(used.get(res_name, 0.0) / nom, 1.0)
                weighted_usage += ratio * weight
                total_weight += weight

            quota_used = (weighted_usage / total_weight) if total_weight > 0 else 0.0
            self._cq_quota_cache[name] = quota_used
            logger.debug(f"[KueueManager] ClusterQueue {name}: quota_used={quota_used:.2%} "
                         f"(cpu={used.get('cpu',0):.1f}/{nominal.get('cpu',0):.1f}, "
                         f"mem={used.get('memory',0):.1f}/{nominal.get('memory',0):.1f}G, "
                         f"gpu={used.get('nvidia.com/gpu',0):.0f}/{nominal.get('nvidia.com/gpu',0):.0f})")

    def _parse_resource_quantity(self, resource_name: str, value_str: str) -> float:
        """Kubernetes resource quantity를 float으로 변환합니다.
        cpu → cores, memory → GB, gpu → count
        """
        value_str = str(value_str).strip()
        if not value_str or value_str == '0':
            return 0.0
        try:
            if resource_name == 'cpu' or resource_name.startswith('cpu'):
                if value_str.endswith('m'):
                    return float(value_str[:-1]) / 1000.0
                return float(value_str)
            elif resource_name == 'memory' or resource_name.startswith('memory'):
                multipliers = [('Ti', 1024**4), ('Gi', 1024**3), ('Mi', 1024**2),
                               ('Ki', 1024), ('T', 1000**4), ('G', 1000**3),
                               ('M', 1000**2), ('K', 1000)]
                for suffix, mult in multipliers:
                    if value_str.endswith(suffix):
                        return float(value_str[:-len(suffix)]) * mult / (1024**3)  # → GB
                return float(value_str) / (1024**3)
            else:
                # gpu, custom resources
                return float(value_str)
        except (ValueError, TypeError):
            return 0.0

    def _refresh_queue_state(self):
        """Kueue LocalQueue 상태 조회 (quota_used는 ClusterQueue 캐시에서 가져옴)"""
        try:
            queues = self.custom_api.list_cluster_custom_object(
                group='kueue.x-k8s.io',
                version='v1beta1',
                plural='localqueues'
            )

            for queue in queues.get('items', []):
                name = queue['metadata']['name']
                status = queue.get('status', {})
                spec = queue.get('spec', {})

                # LocalQueue가 속한 ClusterQueue 이름으로 쿼터 사용률 조회
                cluster_queue_name = spec.get('clusterQueue', '')
                quota_used = self._cq_quota_cache.get(cluster_queue_name, 0.0)

                self.queue_states[name] = {
                    'pending': status.get('pendingWorkloads', 0),
                    'admitted': status.get('admittedWorkloads', 0),
                    'active': status.get('reservingWorkloads', 0),
                    'quota_used': quota_used,
                }
                logger.debug(f"[KueueManager] LocalQueue {name} (cq={cluster_queue_name}): "
                             f"quota_used={quota_used:.2%}")

        except ApiException as e:
            if e.status != 404:
                logger.debug(f"Kueue API not available: {e}")
        except Exception as e:
            logger.debug(f"Error fetching Kueue state: {e}")


class ForecasterClient:
    """Forecaster gRPC 클라이언트 - GetLatestClusterMetrics 호출"""

    def __init__(self, endpoint: Optional[str] = None):
        self.endpoint = endpoint or os.environ.get(
            'FORECASTER_ENDPOINT',
            'node-resource-forecaster.apollo.svc.cluster.local:50055'
        )
        self._channel = None
        self._stub = None
        self._connected = False

    def _ensure_connected(self) -> bool:
        if not FORECASTER_PROTO_AVAILABLE:
            return False
        if self._connected and self._channel:
            return True
        try:
            self._channel = grpc.insecure_channel(self.endpoint)
            self._stub = forecaster_pb2_grpc.NodeResourceForecasterServiceStub(self._channel)
            self._connected = True
            logger.debug(f"[ForecasterClient] Connected to {self.endpoint}")
            return True
        except Exception as e:
            logger.debug(f"[ForecasterClient] Connection failed: {e}")
            return False

    def get_latest_cluster_metrics(self) -> Optional[Dict]:
        """GetLatestClusterMetrics RPC 호출, 실패 시 None 반환"""
        if not self._ensure_connected():
            return None
        try:
            req = forecaster_pb2.GetLatestClusterMetricsRequest()
            resp = self._stub.GetLatestClusterMetrics(req, timeout=5.0)
            node_metrics_dict = {}
            for k, v in resp.node_metrics.items():
                node_metrics_dict[k] = {
                    'cpu_utilization': v.cpu_utilization,
                    'memory_utilization': v.memory_utilization,
                    'gpu_utilization': v.gpu_utilization,
                    'storage_io_utilization': v.storage_io_utilization,
                }
            return {
                'avg_cpu_utilization': resp.avg_cpu_utilization,
                'avg_memory_utilization': resp.avg_memory_utilization,
                'avg_gpu_utilization': resp.avg_gpu_utilization,
                'avg_storage_io_utilization': resp.avg_storage_io_utilization,
                'pending_pods_count': resp.pending_pods_count,
                'node_imbalance': resp.node_imbalance,
                'node_metrics': node_metrics_dict,
            }
        except Exception as e:
            logger.debug(f"[ForecasterClient] GetLatestClusterMetrics failed: {e}")
            return None


class SchedulingPolicyServicer(apollo_pb2_grpc.SchedulingPolicyServiceServicer):
    """
    SchedulingPolicyService 구현
    AI-Storage Scheduler가 기대하는 gRPC 인터페이스

    실제 클러스터/워크로드 데이터 기반 동적 정책 생성
    """

    def __init__(self, model_path: Optional[str] = None):
        # PPO 모델 초기화
        self.config = PPOConfig()
        self.model = SchedulingPolicyPPO(self.config)
        self.trainer = PPOTrainer(self.model, self.config)

        # 실시간 상태 관리자 초기화
        self.cluster_manager = ClusterStateManager(refresh_interval=30)
        self.kueue_manager = KueueStateManager()
        self.forecaster_client = ForecasterClient()

        # Kubernetes API
        try:
            k8s_config.load_incluster_config()
        except:
            try:
                k8s_config.load_kube_config()
            except:
                pass
        self.core_api = client.CoreV1Api()

        # 모델 로드
        if model_path and os.path.exists(model_path):
            try:
                self.trainer.load(model_path)
                logger.info(f"Model loaded from {model_path}")
            except Exception as e:
                logger.warning(f"Failed to load model: {e}, using random initialization")

        # 정책 캐시 (피드백용)
        self.policy_cache: Dict[str, Dict] = {}
        self.cache_lock = threading.Lock()

        # 통계
        self.request_count = 0
        self.total_reward = 0.0
        self.training_episodes = 0

        # 클러스터 정보 (외부에서 주입)
        self.cluster_nodes: List[str] = []

        logger.info("Scheduling Policy Engine initialized")

    def GetSchedulingPolicy(self, request, context):
        """
        스케줄링 정책 조회
        Scheduler의 SchedulingPolicyRequest를 받아 SchedulingPolicy 반환
        """
        request_id = str(uuid.uuid4())
        start_time = time.time()

        pod_name = request.pod_name
        pod_namespace = request.pod_namespace
        pod_labels = dict(request.pod_labels)
        pod_annotations = dict(request.pod_annotations)

        logger.info(f"[PolicyEngine] GetSchedulingPolicy: {pod_namespace}/{pod_name}")

        try:
            # Forecaster에서 metrics_before용 스냅샷 1회 조회
            metrics_before_full = None
            metrics_before = None
            try:
                metrics_before_full = self.forecaster_client.get_latest_cluster_metrics()
                if metrics_before_full:
                    metrics_before = {
                        'cpu_utilization': float(metrics_before_full.get('avg_cpu_utilization', 0.0)),
                        'gpu_utilization': float(metrics_before_full.get('avg_gpu_utilization', 0.0)),
                        'pending_pods': float(metrics_before_full.get('pending_pods_count', 0.0)),
                        'node_imbalance': float(metrics_before_full.get('node_imbalance', 0.0)),
                    }
                    logger.debug(
                        "[RewardDebug] metrics_before(raw): cpu=%.3f gpu=%.3f pending=%.1f imb=%.3f",
                        metrics_before['cpu_utilization'],
                        metrics_before['gpu_utilization'],
                        metrics_before['pending_pods'],
                        metrics_before['node_imbalance'],
                    )
                else:
                    logger.debug("[RewardDebug] metrics_before is None (Forecaster returned empty snapshot)")
            except Exception as e:
                logger.debug(f"[RewardDebug] Failed to fetch metrics_before from Forecaster: {e}")

            # Pod 메타데이터 + 실제 Pod spec에서 워크로드 특성 추출
            workload_info = self._extract_workload_info(
                pod_labels, pod_annotations, pod_name, pod_namespace
            )

            # 실제 클러스터/워크로드 데이터 기반 모델 입력 생성
            # Forecaster 스냅샷(metrics_before_full)을 그대로 사용해 state tensor 생성
            state_tensor = self._create_state_tensor(workload_info, metrics_snapshot=metrics_before_full)

            # PPO 모델로 정책 가중치 추론
            with torch.no_grad():
                action, log_prob, value = self.model.get_action(
                    state_tensor['workload'],
                    state_tensor['pipeline'],
                    state_tensor['cluster'],
                    deterministic=True
                )

            weights = action.squeeze().numpy()
            confidence = float(value.item())

            # NodePreferences 생성 (가중치 기반)
            node_preferences = self._compute_node_preferences(
                weights, workload_info, pod_labels
            )

            # WorkloadSignature 생성
            workload_signature = self._create_workload_signature(workload_info, weights)

            # StorageRequirements 생성
            storage_requirements = self._create_storage_requirements(workload_info)

            # 캐시 저장 (피드백용) - 보상 계산에 필요한 컨텍스트 포함
            with self.cache_lock:
                self.policy_cache[request_id] = {
                    'state': state_tensor,
                    'action': action,
                    'log_prob': log_prob,
                    'value': value.item(),
                    'timestamp': time.time(),
                    # 보상 계산용 추가 컨텍스트
                    'workload_info': workload_info,
                    'weights': weights.tolist(),
                    'node_preferences': [(p.node_name, p.score) for p in node_preferences],
                    'confidence': confidence,
                    'metrics_before': metrics_before,
                }

            self.request_count += 1
            latency = (time.time() - start_time) * 1000

            # PluginWeights 생성 - 스케줄러의 scoring 가중치로 사용됨
            plugin_weights = apollo_pb2.PluginWeights(
                data_locality_aware=float(weights[0]),
                storage_tier_aware=float(weights[1]),
                io_pattern_based=float(weights[2]),
                kueue_aware=float(weights[3]),
                pipeline_stage_aware=float(weights[4])
            )

            # 응답 생성
            response = apollo_pb2.SchedulingPolicy(
                request_id=request_id,
                pod_name=pod_name,
                pod_namespace=pod_namespace,
                decision=apollo_pb2.SCHEDULING_DECISION_ALLOW,
                node_preferences=node_preferences,
                storage_requirements=storage_requirements,
                workload_signature=workload_signature,
                plugin_weights=plugin_weights,
                priority=workload_info.get('priority', 0),
                allow_preemption=workload_info.get('preemptible', False),
                reason=f"Policy weights: DLA={weights[0]:.2f}, STA={weights[1]:.2f}, "
                       f"IOPB={weights[2]:.2f}, KA={weights[3]:.2f}, PSA={weights[4]:.2f}"
            )

            logger.info(f"[PolicyEngine] Policy generated: request_id={request_id}, "
                       f"weights={weights.tolist()[:5]}, confidence={confidence:.3f}, "
                       f"latency={latency:.2f}ms")

            return response

        except Exception as e:
            logger.error(f"[PolicyEngine] Error generating policy: {e}", exc_info=True)
            # Fallback: 기본 정책 반환
            return apollo_pb2.SchedulingPolicy(
                request_id=request_id,
                pod_name=pod_name,
                pod_namespace=pod_namespace,
                decision=apollo_pb2.SCHEDULING_DECISION_ALLOW,
                reason=f"Fallback policy due to error: {str(e)}"
            )

    def ReportSchedulingResult(self, request, context):
        """
        스케줄링 결과 피드백
        RL 모델 학습에 활용
        """
        request_id = request.request_id
        success = request.success
        scheduled_node = request.scheduled_node
        duration_ms = request.scheduling_duration_ms

        logger.info(f"[PolicyEngine] SchedulingResult: request_id={request_id}, "
                   f"success={success}, node={scheduled_node}, duration={duration_ms}ms")

        try:
            # 캐시에서 정책 조회
            with self.cache_lock:
                cached = self.policy_cache.pop(request_id, None)

            if cached:
                metrics_before = cached.get('metrics_before')

                # Forecaster에서 metrics_after 조회 (delta reward용)
                metrics_after = None
                try:
                    metrics_after_full = self.forecaster_client.get_latest_cluster_metrics()
                    if metrics_after_full:
                        metrics_after = {
                            'cpu_utilization': float(metrics_after_full.get('avg_cpu_utilization', 0.0)),
                            'gpu_utilization': float(metrics_after_full.get('avg_gpu_utilization', 0.0)),
                            'pending_pods': float(metrics_after_full.get('pending_pods_count', 0.0)),
                            'node_imbalance': float(metrics_after_full.get('node_imbalance', 0.0)),
                        }
                        logger.debug(
                            "[RewardDebug] metrics_after(raw): cpu=%.3f gpu=%.3f pending=%.1f imb=%.3f",
                            metrics_after['cpu_utilization'],
                            metrics_after['gpu_utilization'],
                            metrics_after['pending_pods'],
                            metrics_after['node_imbalance'],
                        )
                    else:
                        logger.debug("[RewardDebug] metrics_after is None (Forecaster returned empty snapshot)")
                except Exception as e:
                    logger.debug(f"[RewardDebug] Failed to fetch metrics_after from Forecaster: {e}")

                # 개선된 보상 계산 (컨텍스트 활용)
                reward = self._calculate_improved_reward(
                    success=success,
                    duration_ms=duration_ms,
                    scheduled_node=scheduled_node,
                    cached_context=cached,
                    failure_reason=request.failure_reason,
                    metrics_before=metrics_before,
                    metrics_after=metrics_after,
                )

                # Experience 저장
                self.trainer.store_transition(
                    state=cached['state'],
                    action=cached['action'],
                    reward=reward,
                    value=cached['value'],
                    log_prob=cached['log_prob'],
                    done=True
                )

                self.total_reward += reward
                self.training_episodes += 1

                # 주기적 학습 (32 에피소드마다)
                if self.training_episodes % 32 == 0:
                    metrics = self.trainer.update()
                    logger.info(f"[PolicyEngine] Training update: {metrics}")

                logger.info(f"[PolicyEngine] Feedback processed: reward={reward:.4f}")

                return apollo_pb2.ReportResponse(
                    success=True,
                    message=f"Feedback accepted, reward={reward:.4f}",
                    request_id=request_id
                )
            else:
                return apollo_pb2.ReportResponse(
                    success=False,
                    message="Request ID not found in cache",
                    request_id=request_id
                )

        except Exception as e:
            logger.error(f"[PolicyEngine] Error processing feedback: {e}")
            return apollo_pb2.ReportResponse(
                success=False,
                message=str(e),
                request_id=request_id
            )

    def _extract_workload_info(self, labels: Dict, annotations: Dict,
                                pod_name: str = '', pod_namespace: str = '') -> Dict:
        """
        Pod 레이블/어노테이션 + 실제 Pod spec에서 워크로드 정보 추출

        실제 데이터 소스:
        - Pod labels/annotations: 워크로드 타입, IO 패턴
        - Pod spec: CPU/Memory/GPU 요청량
        - Argo Workflow annotations: 파이프라인 위치
        - Kubeflow labels: 분산 학습 정보
        """
        info = {
            'workload_type': 'unknown',
            'pipeline_stage': 'unknown',
            'io_pattern': 'balanced',
            'framework': '',
            'requires_gpu': False,
            'requires_csd': False,
            'data_size_gb': 0.0,
            'priority': 0,
            'preemptible': True,
            'dataset_name': '',
            'data_locations': [],
            # 실제 리소스 요청 (Pod spec에서)
            'cpu_request': 0.0,
            'mem_request_gb': 0.0,
            'gpu_request': 0,
            # 파이프라인 정보 (Argo Workflow)
            'pipeline_name': '',
            'pipeline_step_index': 0,
            'pipeline_total_steps': 1,
            'pipeline_dependencies': 0,
            # 분산 학습 정보 (Kubeflow)
            'is_distributed': False,
            'replica_type': '',  # master/worker
            'replica_index': 0,
            'total_replicas': 1,
            # Kueue 정보
            'queue_name': '',
        }

        # === 1. 레이블에서 기본 정보 추출 ===
        # 워크로드 타입 추론
        all_text = ' '.join(list(labels.values()) + list(annotations.values())).lower()

        if any(kw in all_text for kw in ['train', 'training', 'fit']):
            info['workload_type'] = 'training'
            info['pipeline_stage'] = 'training'
        elif any(kw in all_text for kw in ['preprocess', 'etl', 'transform', 'augment']):
            info['workload_type'] = 'preprocessing'
            info['pipeline_stage'] = 'preprocessing'
        elif any(kw in all_text for kw in ['infer', 'predict', 'serve', 'serving']):
            info['workload_type'] = 'inference'
            info['pipeline_stage'] = 'inference'
        elif any(kw in all_text for kw in ['eval', 'valid', 'test']):
            info['workload_type'] = 'evaluation'
            info['pipeline_stage'] = 'evaluation'

        # 프레임워크 감지
        if 'pytorch' in all_text or 'torch' in all_text:
            info['framework'] = 'pytorch'
        elif 'tensorflow' in all_text or 'tf-' in all_text:
            info['framework'] = 'tensorflow'
        elif 'jax' in all_text:
            info['framework'] = 'jax'

        # === 2. 명시적 어노테이션 (우선순위 높음) ===
        annotation_mappings = {
            'apollo.ai-storage/workload-type': 'workload_type',
            'apollo.ai-storage/io-pattern': 'io_pattern',
            'apollo.ai-storage/dataset-name': 'dataset_name',
            'apollo.ai-storage/data-size-gb': ('data_size_gb', float),
            'apollo.ai-storage/requires-gpu': ('requires_gpu', lambda x: x.lower() == 'true'),
            'apollo.ai-storage/requires-csd': ('requires_csd', lambda x: x.lower() == 'true'),
            'apollo.ai-storage/data-locations': ('data_locations', lambda x: x.split(',')),
            # Insight-Trace에서 수집한 실제 I/O 메트릭
            'insight-trace.keti/io-read-ratio': ('io_read_ratio', float),
            'insight-trace.keti/io-write-ratio': ('io_write_ratio', float),
            'insight-trace.keti/io-iops': ('io_iops', float),
            'insight-trace.keti/io-throughput-mbps': ('io_throughput', float),
        }

        for anno_key, target in annotation_mappings.items():
            if anno_key in annotations:
                value = annotations[anno_key]
                if isinstance(target, tuple):
                    field, converter = target
                    try:
                        info[field] = converter(value)
                    except:
                        pass
                else:
                    info[target] = value

        # === 3. Argo Workflow 파이프라인 정보 ===
        if 'workflows.argoproj.io/node-name' in annotations:
            # 예: "my-workflow/step-2"
            node_name = annotations['workflows.argoproj.io/node-name']
            info['pipeline_name'] = node_name.split('/')[0] if '/' in node_name else node_name

            # 스텝 인덱스 추출 시도
            step_match = re.search(r'step[_-]?(\d+)', node_name.lower())
            if step_match:
                info['pipeline_step_index'] = int(step_match.group(1))

        if 'workflows.argoproj.io/template' in annotations:
            info['pipeline_stage'] = annotations['workflows.argoproj.io/template']

        # === 4. Kubeflow 분산학습 정보 ===
        if 'training.kubeflow.org/job-name' in labels:
            info['is_distributed'] = True
            info['pipeline_name'] = labels['training.kubeflow.org/job-name']

        if 'training.kubeflow.org/replica-type' in labels:
            info['replica_type'] = labels['training.kubeflow.org/replica-type']

        if 'training.kubeflow.org/replica-index' in labels:
            try:
                info['replica_index'] = int(labels['training.kubeflow.org/replica-index'])
            except:
                pass

        # Gang scheduling 정보
        if 'gang-group' in labels:
            info['pipeline_name'] = labels['gang-group']
            info['is_distributed'] = True

        # === 5. Kueue 큐 정보 ===
        if 'kueue.x-k8s.io/queue-name' in labels:
            info['queue_name'] = labels['kueue.x-k8s.io/queue-name']

        # === 6. 실제 Pod spec에서 리소스 정보 추출 ===
        if pod_name and pod_namespace:
            try:
                pod = self.core_api.read_namespaced_pod(pod_name, pod_namespace)

                for container in (pod.spec.containers or []):
                    requests = container.resources.requests or {} if container.resources else {}
                    limits = container.resources.limits or {} if container.resources else {}

                    # CPU (cores)
                    cpu_str = requests.get('cpu') or limits.get('cpu') or '0'
                    info['cpu_request'] += self._parse_cpu(cpu_str)

                    # Memory (GB)
                    mem_str = requests.get('memory') or limits.get('memory') or '0'
                    info['mem_request_gb'] += self._parse_memory(mem_str)

                    # GPU
                    gpu_str = requests.get('nvidia.com/gpu') or limits.get('nvidia.com/gpu') or '0'
                    info['gpu_request'] += int(gpu_str)

                if info['gpu_request'] > 0:
                    info['requires_gpu'] = True

            except ApiException as e:
                logger.debug(f"Could not fetch Pod spec: {e}")
            except Exception as e:
                logger.debug(f"Error extracting Pod resources: {e}")

        return info

    def _parse_cpu(self, cpu_str: str) -> float:
        """CPU 문자열 파싱"""
        if not cpu_str:
            return 0.0
        cpu_str = str(cpu_str)
        if cpu_str.endswith('m'):
            return float(cpu_str[:-1]) / 1000
        return float(cpu_str)

    def _parse_memory(self, mem_str: str) -> float:
        """Memory 문자열 파싱 (GB)"""
        if not mem_str:
            return 0.0
        mem_str = str(mem_str)
        multipliers = {'Ki': 1024, 'Mi': 1024**2, 'Gi': 1024**3,
                      'K': 1000, 'M': 1000**2, 'G': 1000**3}
        for suffix, mult in multipliers.items():
            if mem_str.endswith(suffix):
                return float(mem_str[:-len(suffix)]) * mult / (1024**3)
        return float(mem_str) / (1024**3)

    def _create_state_tensor(self, workload_info: Dict, metrics_snapshot: Optional[Dict] = None) -> Dict:
        """
        실제 클러스터/워크로드 데이터 기반 모델 입력 텐서 생성

        하드코딩 제거 - 모든 값이 실제 데이터에서 계산됨
        """
        # === 1. 워크로드 텐서 (실제 워크로드 특성) ===
        wt_map = {'training': 0, 'inference': 1, 'preprocessing': 2,
                  'evaluation': 3, 'data_loading': 4, 'unknown': 5}
        preprocess_map = {'augmentation': 0, 'filtering': 1, 'transformation': 2,
                         'tokenization': 3, 'normalization': 4, 'unknown': 5}
        io_map = {'read_heavy': 0, 'write_heavy': 1, 'balanced': 2,
                  'sequential': 3, 'random': 4}

        wt_idx = wt_map.get(workload_info['workload_type'], 5)
        io_idx = io_map.get(workload_info['io_pattern'], 2)

        # I/O 패턴 벡터 (Insight-Trace 데이터 또는 추론)
        io_read_ratio = workload_info.get('io_read_ratio', 0.0)
        io_write_ratio = workload_info.get('io_write_ratio', 0.0)

        # Insight-Trace 데이터가 없으면 워크로드 타입에서 추론
        if io_read_ratio == 0.0 and io_write_ratio == 0.0:
            io_patterns = {
                'read_heavy': (0.8, 0.2),
                'write_heavy': (0.2, 0.8),
                'balanced': (0.5, 0.5),
                'sequential': (0.6, 0.4),
                'random': (0.5, 0.5),
            }
            io_read_ratio, io_write_ratio = io_patterns.get(
                workload_info['io_pattern'], (0.5, 0.5))

        # 순차/랜덤 비율 (타입에서 추론)
        seq_ratio = 0.7 if workload_info['io_pattern'] == 'sequential' else 0.3
        rand_ratio = 0.7 if workload_info['io_pattern'] == 'random' else 0.3

        # IOPS/Throughput (실제 값 또는 워크로드 타입 기반 추정)
        iops_normalized = min(1.0, workload_info.get('io_iops', 0) / 100000)  # 100K IOPS 기준
        throughput_normalized = min(1.0, workload_info.get('io_throughput', 0) / 1000)  # 1GB/s 기준

        # 워크로드 타입별 기본 I/O 강도
        if iops_normalized == 0 and throughput_normalized == 0:
            io_intensity_map = {
                'training': 0.7,
                'inference': 0.3,
                'preprocessing': 0.8,
                'evaluation': 0.4,
                'unknown': 0.5,
            }
            io_intensity = io_intensity_map.get(workload_info['workload_type'], 0.5)
        else:
            io_intensity = max(iops_normalized, throughput_normalized)

        # 컴퓨트 프로파일 (실제 리소스 요청에서 계산)
        cpu_req = workload_info.get('cpu_request', 0.0)
        mem_req = workload_info.get('mem_request_gb', 0.0)
        gpu_req = workload_info.get('gpu_request', 0)

        # 정규화 (일반적인 AI 워크로드 기준)
        cpu_normalized = min(1.0, cpu_req / 16.0)  # 16 cores 기준
        mem_normalized = min(1.0, mem_req / 64.0)  # 64GB 기준
        gpu_normalized = min(1.0, gpu_req / 8.0)   # 8 GPU 기준

        # 리소스 강도 (요청량 기반)
        cpu_intensity = 0.3 + cpu_normalized * 0.7  # 0.3 ~ 1.0
        mem_intensity = 0.3 + mem_normalized * 0.7
        gpu_intensity = gpu_normalized if gpu_req > 0 else 0.0

        workload_tensor = {
            'workload_type': torch.tensor([wt_idx], dtype=torch.long),
            'preprocessing_type': torch.tensor([
                preprocess_map.get(workload_info.get('preprocessing_type', 'unknown'), 5)
            ], dtype=torch.long),
            'io_pattern': torch.tensor([[
                io_read_ratio,
                io_write_ratio,
                seq_ratio,
                rand_ratio,
                iops_normalized,
                throughput_normalized,
                io_intensity
            ]], dtype=torch.float32),
            'compute_profile': torch.tensor([[
                cpu_intensity,
                mem_intensity,
                gpu_intensity,
                cpu_normalized,
                mem_normalized,
                gpu_normalized
            ]], dtype=torch.float32),
            'flags': torch.tensor([[
                float(workload_info['requires_gpu']),
                float(workload_info['requires_csd']),
                min(1.0, workload_info['data_size_gb'] / 100.0)  # 100GB 기준 정규화
            ]], dtype=torch.float32)
        }

        # === 2. 파이프라인 텐서 (Argo Workflow / Kubeflow 데이터) ===
        step_idx = workload_info.get('pipeline_step_index', 0)
        total_steps = workload_info.get('pipeline_total_steps', 1)
        total_steps = max(total_steps, 1)

        # 파이프라인 진행률
        progress = step_idx / total_steps

        # DAG 구조 (분산 학습 정보에서)
        is_distributed = workload_info.get('is_distributed', False)
        total_replicas = workload_info.get('total_replicas', 1)
        dependencies = workload_info.get('pipeline_dependencies', 0)

        # 데이터 지역성
        has_data_locations = len(workload_info.get('data_locations', [])) > 0

        pipeline_tensor = {
            'position': torch.tensor([[
                step_idx / max(total_steps, 1),  # current position (0~1)
                1.0 / max(total_steps, 1),       # step fraction
                progress                          # progress (0~1)
            ]], dtype=torch.float32),
            'dag_structure': torch.tensor([[
                min(1.0, dependencies / 5.0),       # num_prev (5개 기준)
                min(1.0, (total_steps - step_idx - 1) / 5.0),  # num_next
                min(1.0, step_idx / 10.0),          # depth (10 기준)
                min(1.0, total_replicas / 8.0) if is_distributed else 0.0  # fan_out
            ]], dtype=torch.float32),
            'locality': torch.tensor([[
                1.0 if has_data_locations else 0.0,  # has_prev_node data
                0.5 if is_distributed else 1.0       # same_node_ratio (분산이면 낮음)
            ]], dtype=torch.float32)
        }

        # === 3. 클러스터 텐서 (K8s + insight-scope/forecaster 메트릭) ===
        node_states, cluster_metrics = self.cluster_manager.get_cluster_state()

        # Forecaster에서 insight-scope 기반 최신 메트릭 조회
        # metrics_snapshot이 제공되면 동일 스냅샷을 사용 (GetSchedulingPolicy에서 1회 조회)
        forecaster_metrics = metrics_snapshot or self.forecaster_client.get_latest_cluster_metrics()
        forecaster_node = {}
        if forecaster_metrics:
            cluster_metrics['cpu_utilization'] = forecaster_metrics['avg_cpu_utilization']
            cluster_metrics['mem_utilization'] = forecaster_metrics['avg_memory_utilization']
            cluster_metrics['gpu_utilization'] = forecaster_metrics['avg_gpu_utilization']
            # storage_io_utilization: insight-scope avg_storage_io_utilization (disk I/O util, 0~1)
            cluster_metrics['storage_io_utilization'] = forecaster_metrics['avg_storage_io_utilization']
            cluster_metrics['pending_pods'] = forecaster_metrics['pending_pods_count']
            cluster_metrics['node_imbalance'] = forecaster_metrics['node_imbalance']
            forecaster_node = forecaster_metrics.get('node_metrics', {})
            if metrics_snapshot is not None:
                logger.debug(
                    "[RewardDebug] state cluster_metrics(norm): cpu=%.3f gpu=%.3f pending=%.3f imb=%.3f",
                    cluster_metrics.get('cpu_utilization', 0.5),
                    cluster_metrics.get('gpu_utilization', 0.0),
                    min(1.0, cluster_metrics.get('pending_pods', 0) / 100.0),
                    cluster_metrics.get('node_imbalance', 0.0),
                )
        else:
            cluster_metrics.setdefault('gpu_utilization', 0.0)
            cluster_metrics.setdefault('storage_io_utilization', 0.0)
            cluster_metrics.setdefault('pending_pods', 0)
            cluster_metrics.setdefault('node_imbalance', 0.0)

        # 노드 상태 텐서 생성 (최대 32개 노드)
        max_nodes = 32
        nodes_tensor = torch.zeros(1, max_nodes, 10, dtype=torch.float32)
        node_mask = torch.zeros(1, max_nodes, dtype=torch.bool)

        node_type_map = {'compute': 0, 'gpu': 1, 'storage': 2, 'control-plane': 3}
        storage_tier_map = {'nvme': 0, 'ssd': 1, 'hdd': 2, 'standard': 1}

        for i, (node_name, state) in enumerate(list(node_states.items())[:max_nodes]):
            if not state.get('is_ready', False):
                continue

            node_mask[0, i] = True
            # forecaster 노드 메트릭이 있으면 cpu/mem/gpu/storage_io 덮어쓰기
            fn = forecaster_node.get(node_name, {})
            cpu_util = fn.get('cpu_utilization', state.get('cpu_utilization', 0.5)) if isinstance(fn, dict) else state.get('cpu_utilization', 0.5)
            mem_util = fn.get('memory_utilization', state.get('mem_utilization', 0.5)) if isinstance(fn, dict) else state.get('mem_utilization', 0.5)
            gpu_util = fn.get('gpu_utilization', 0.0) if isinstance(fn, dict) else 0.0

            gpu_val = min(1.0, gpu_util) if gpu_util > 0 else min(1.0, state.get('gpu_capacity', 0) / 8.0)
            nodes_tensor[0, i] = torch.tensor([
                cpu_util,
                mem_util,
                gpu_val,  # GPU utilization (forecaster) or capacity (K8s)
                node_type_map.get(state.get('node_type', 'compute'), 0) / 3.0,
                storage_tier_map.get(state.get('storage_tier', 'standard'), 1) / 2.0,
                min(1.0, state.get('cpu_allocatable', 0) / 64.0),
                min(1.0, state.get('mem_allocatable', 0) / 256.0),
                1.0 if 'gpu' in node_name.lower() else 0.0,
                1.0 if 'csd' in node_name.lower() or 'storage' in node_name.lower() else 0.0,
                1.0 - cpu_util
            ], dtype=torch.float32)

        # Kueue 큐 상태
        queue_name = workload_info.get('queue_name', 'ai-storage-queue')
        queue_state = self.kueue_manager.get_queue_state(queue_name)

        # cluster_metrics: 8차원 (기존 4 + insight-scope 4)
        cluster_tensor = {
            'nodes': nodes_tensor,
            'node_mask': node_mask,
            'cluster_metrics': torch.tensor([[
                cluster_metrics.get('cpu_utilization', 0.5),
                cluster_metrics.get('mem_utilization', 0.5),
                min(1.0, cluster_metrics.get('total_nodes', 1) / 32.0),
                min(1.0, cluster_metrics.get('gpu_available', 0) / 16.0),
                cluster_metrics.get('gpu_utilization', 0.0),
                min(1.0, cluster_metrics.get('pending_pods', 0) / 100.0),
                min(1.0, cluster_metrics.get('node_imbalance', 0.0)),  # variance 0~1
                cluster_metrics.get('storage_io_utilization', 0.0),
            ]], dtype=torch.float32),
            'queue_status': torch.tensor([[
                min(1.0, queue_state.get('pending', 0) / 100.0),
                min(1.0, queue_state.get('admitted', 0) / 50.0),
                min(1.0, queue_state.get('active', 0) / 20.0),
                queue_state.get('quota_used', 0.5)
            ]], dtype=torch.float32)
        }

        return {
            'workload': workload_tensor,
            'pipeline': pipeline_tensor,
            'cluster': cluster_tensor
        }

    def _compute_node_preferences(self, weights: np.ndarray,
                                   workload_info: Dict,
                                   labels: Dict) -> List[apollo_pb2.NodePreference]:
        """가중치 기반 노드 선호도 계산"""
        preferences = []

        # 데이터 위치 노드 선호
        data_locations = workload_info.get('data_locations', [])
        for node in data_locations:
            # DataLocalityAware 가중치 적용
            score = int(weights[0] * 100)
            preferences.append(apollo_pb2.NodePreference(
                node_name=node.strip(),
                score=score,
                reason=f"Data locality: DataLocalityAware weight={weights[0]:.2f}"
            ))

        return preferences

    def _create_workload_signature(self, workload_info: Dict,
                                    weights: np.ndarray) -> apollo_pb2.WorkloadSignature:
        """WorkloadSignature 생성"""
        # 워크로드 타입 매핑
        wt_map = {
            'training': apollo_pb2.WORKLOAD_TYPE_GENERIC,
            'inference': apollo_pb2.WORKLOAD_TYPE_GENERIC,
            'preprocessing': apollo_pb2.WORKLOAD_TYPE_GENERIC,
            'image': apollo_pb2.WORKLOAD_TYPE_IMAGE,
            'text': apollo_pb2.WORKLOAD_TYPE_TEXT,
        }

        # 파이프라인 스테이지 매핑
        ps_map = {
            'preprocessing': apollo_pb2.PIPELINE_STAGE_PREPROCESSING,
            'training': apollo_pb2.PIPELINE_STAGE_TRAINING,
            'inference': apollo_pb2.PIPELINE_STAGE_INFERENCE,
            'data_loading': apollo_pb2.PIPELINE_STAGE_DATA_LOADING,
        }

        # IO 패턴 매핑
        io_map = {
            'read_heavy': apollo_pb2.IO_PATTERN_READ_HEAVY,
            'write_heavy': apollo_pb2.IO_PATTERN_WRITE_HEAVY,
            'balanced': apollo_pb2.IO_PATTERN_BALANCED,
            'sequential': apollo_pb2.IO_PATTERN_SEQUENTIAL,
            'random': apollo_pb2.IO_PATTERN_RANDOM,
        }

        return apollo_pb2.WorkloadSignature(
            workload_type=wt_map.get(workload_info['workload_type'], apollo_pb2.WORKLOAD_TYPE_UNKNOWN),
            current_stage=ps_map.get(workload_info['pipeline_stage'], apollo_pb2.PIPELINE_STAGE_UNKNOWN),
            io_pattern=io_map.get(workload_info['io_pattern'], apollo_pb2.IO_PATTERN_UNKNOWN),
            confidence=float(np.max(weights)),
            framework=workload_info.get('framework', ''),
            is_gpu_workload=workload_info.get('requires_gpu', False),
            is_distributed=False,
            is_pipeline=workload_info['pipeline_stage'] != 'unknown',
            pipeline_step=workload_info['pipeline_stage'],
            dataset_name=workload_info.get('dataset_name', ''),
            data_locations=workload_info.get('data_locations', []),
        )

    def _create_storage_requirements(self, workload_info: Dict) -> apollo_pb2.StorageRequirements:
        """StorageRequirements 생성"""
        io_map = {
            'read_heavy': apollo_pb2.IO_PATTERN_READ_HEAVY,
            'write_heavy': apollo_pb2.IO_PATTERN_WRITE_HEAVY,
            'balanced': apollo_pb2.IO_PATTERN_BALANCED,
            'sequential': apollo_pb2.IO_PATTERN_SEQUENTIAL,
            'random': apollo_pb2.IO_PATTERN_RANDOM,
        }

        storage_class = apollo_pb2.STORAGE_CLASS_STANDARD
        if workload_info.get('requires_csd'):
            storage_class = apollo_pb2.STORAGE_CLASS_CSD
        elif workload_info.get('io_pattern') == 'random':
            storage_class = apollo_pb2.STORAGE_CLASS_FAST

        return apollo_pb2.StorageRequirements(
            storage_class=storage_class,
            size_bytes=int(workload_info.get('data_size_gb', 0) * 1024 * 1024 * 1024),
            min_iops=10000 if workload_info.get('io_pattern') == 'random' else 1000,
            min_throughput_mbps=500 if workload_info.get('io_pattern') == 'sequential' else 100,
            expected_io_pattern=io_map.get(workload_info['io_pattern'], apollo_pb2.IO_PATTERN_BALANCED),
            requires_csd=workload_info.get('requires_csd', False)
        )

    def _calculate_improved_reward(self, success: bool, duration_ms: int,
                                     scheduled_node: str, cached_context: Dict,
                                     failure_reason: str,
                                     metrics_before: Optional[Dict] = None,
                                     metrics_after: Optional[Dict] = None) -> float:
        """
        개선된 보상 계산 - 스케줄링 품질을 반영

        보상 구성요소:
        1. 기본 성공/실패 보상 (-1.0 ~ 1.0)
        2. 데이터 로컬리티 보상 (0 ~ 0.5)
        3. 워크로드 적합성 보상 (0 ~ 0.3)
        4. 정책 일관성 보상 (0 ~ 0.2)
        5. 스케줄링 속도 보상 (-0.2 ~ 0.2)
        """
        if not success:
            # 실패 유형에 따른 차등 페널티
            if 'resource' in failure_reason.lower():
                return -0.8  # 리소스 부족 - 예측 실패
            elif 'affinity' in failure_reason.lower():
                return -0.5  # 어피니티 불일치 - 정책 개선 필요
            else:
                return -1.0  # 기타 실패

        reward = 0.0
        workload_info = cached_context.get('workload_info', {})
        node_preferences = cached_context.get('node_preferences', [])
        weights = cached_context.get('weights', [0.2] * 5)

        # 1. 기본 성공 보상 (0.5)
        reward += 0.5

        # 2. 데이터 로컬리티 보상 (0 ~ 0.5)
        data_locations = workload_info.get('data_locations', [])
        if data_locations:
            if scheduled_node in data_locations:
                reward += 0.5  # 데이터가 있는 노드에 스케줄링 - 최고!
                logger.info(f"[Reward] Data locality bonus: +0.5 (node {scheduled_node} in data_locations)")
            else:
                reward += 0.1  # 데이터 위치 고려했지만 다른 노드
        else:
            reward += 0.25  # 데이터 위치 정보 없음 - 중립

        # 3. 워크로드 적합성 보상 (0 ~ 0.3)
        workload_type = workload_info.get('workload_type', 'unknown')
        io_pattern = workload_info.get('io_pattern', 'balanced')
        requires_gpu = workload_info.get('requires_gpu', False)
        requires_csd = workload_info.get('requires_csd', False)

        # GPU 워크로드가 GPU 노드에 배치되었는지 (노드 이름으로 휴리스틱 판단)
        if requires_gpu:
            if 'gpu' in scheduled_node.lower():
                reward += 0.3
                logger.info(f"[Reward] GPU matching bonus: +0.3")
            else:
                reward += 0.0  # GPU 필요한데 GPU 노드 아님
        elif requires_csd:
            if 'csd' in scheduled_node.lower() or 'storage' in scheduled_node.lower():
                reward += 0.3
                logger.info(f"[Reward] CSD matching bonus: +0.3")
            else:
                reward += 0.1
        else:
            reward += 0.15  # 특별 요구사항 없음 - 중립

        # 4. 정책 일관성 보상 (0 ~ 0.2)
        # APOLLO가 추천한 노드와 실제 스케줄링된 노드 비교
        if node_preferences:
            pref_dict = {name: score for name, score in node_preferences}
            if scheduled_node in pref_dict:
                pref_score = pref_dict[scheduled_node]
                # 높은 점수의 노드에 배치될수록 좋음
                max_score = max(score for _, score in node_preferences) if node_preferences else 100
                if max_score > 0:
                    consistency_bonus = 0.2 * (pref_score / max_score)
                    reward += consistency_bonus
                    logger.info(f"[Reward] Policy consistency bonus: +{consistency_bonus:.3f}")
        else:
            reward += 0.1  # 선호도 정보 없음 - 중립

        # 5. 스케줄링 속도 보상 (-0.2 ~ 0.2)
        if duration_ms < 50:
            reward += 0.2
        elif duration_ms < 100:
            reward += 0.1
        elif duration_ms < 500:
            reward += 0.0
        elif duration_ms < 2000:
            reward -= 0.1
        else:
            reward -= 0.2

        # 6. delta 기반 클러스터 메트릭 보상 (insight-scope/forecaster)
        reward_delta = self._calculate_delta_reward(metrics_before, metrics_after)
        reward += reward_delta

        # 최종 보상 클리핑
        final_reward = max(-1.0, min(2.0, reward))

        logger.info(f"[Reward] Final reward: {final_reward:.4f} "
                   f"(base=0.5, locality={0.5 if scheduled_node in data_locations else 0.1 if data_locations else 0.25:.2f}, "
                   f"workload_fit, policy_consistency, speed)")

        return final_reward

    def _calculate_delta_reward(self,
                                 metrics_before: Optional[Dict],
                                 metrics_after: Optional[Dict]) -> float:
        """
        insight-scope 기반 클러스터 메트릭 delta 보상 계산

        delta_cpu       = cpu_after - cpu_before
        delta_gpu       = gpu_after - gpu_before
        delta_pending   = pending_before - pending_after
        delta_imbalance = imbalance_before - imbalance_after

        reward_delta =
          w1 * delta_cpu
        + w2 * delta_gpu
        + w3 * delta_pending
        + w4 * delta_imbalance
        """
        if not metrics_before or not metrics_after:
            return 0.0

        try:
            cpu_b = float(metrics_before.get('cpu_utilization', 0.0))
            cpu_a = float(metrics_after.get('cpu_utilization', 0.0))
            gpu_b = float(metrics_before.get('gpu_utilization', 0.0))
            gpu_a = float(metrics_after.get('gpu_utilization', 0.0))
            p_b = float(metrics_before.get('pending_pods', 0.0))
            p_a = float(metrics_after.get('pending_pods', 0.0))
            imb_b = float(metrics_before.get('node_imbalance', 0.0))
            imb_a = float(metrics_after.get('node_imbalance', 0.0))

            delta_cpu = cpu_a - cpu_b
            delta_gpu = gpu_a - gpu_b
            delta_pending = p_b - p_a
            delta_imbalance = imb_b - imb_a

            # 가중치: delta 보상은 기존 보상 대비 소규모로 유지
            w1 = 0.2  # cpu
            w2 = 0.2  # gpu
            w3 = 0.2  # pending
            w4 = 0.2  # imbalance

            reward_delta = (
                w1 * delta_cpu +
                w2 * delta_gpu +
                w3 * delta_pending +
                w4 * delta_imbalance
            )

            # delta 보상을 [-0.5, 0.5] 범위로 제한
            return max(-0.5, min(0.5, reward_delta))

        except Exception as e:
            logger.debug(f"[Reward] delta reward calculation failed: {e}")
            return 0.0

    def _calculate_scheduling_reward(self, success: bool, duration_ms: int,
                                      failure_reason: str) -> float:
        """레거시 보상 계산 (fallback용)"""
        if not success:
            return -1.0
        reward = 1.0
        if duration_ms < 100:
            reward += 0.5
        elif duration_ms < 500:
            reward += 0.2
        elif duration_ms > 5000:
            reward -= 0.3
        return max(-1.0, min(2.0, reward))


def serve(port: int = 50054, model_path: Optional[str] = None):
    """gRPC 서버 시작"""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    servicer = SchedulingPolicyServicer(model_path)
    apollo_pb2_grpc.add_SchedulingPolicyServiceServicer_to_server(servicer, server)

    server.add_insecure_port(f'[::]:{port}')
    server.start()

    logger.info(f"Scheduling Policy Engine server started on port {port}")

    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        logger.info("Shutting down server...")
        server.stop(grace=5)


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=50054)
    parser.add_argument('--model', type=str, default=None)
    args = parser.parse_args()

    serve(port=args.port, model_path=args.model)
