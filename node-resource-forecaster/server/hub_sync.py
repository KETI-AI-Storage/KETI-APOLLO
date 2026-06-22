"""
Pull node resource history from Insight Hub into forecaster in-memory node_history.
Activated when INSIGHT_HUB_ADDR is set (e.g. insight-hub.apollo.svc.cluster.local:50056).
"""
from __future__ import annotations

import logging
import os
import sys
import threading
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

logger = logging.getLogger(__name__)

_proto_dir = os.path.join(os.path.dirname(__file__), "..", "proto")
if _proto_dir not in sys.path:
    sys.path.insert(0, _proto_dir)

try:
    import grpc
    import hub_pb2
    import hub_pb2_grpc
    HUB_AVAILABLE = True
except ImportError as e:
    HUB_AVAILABLE = False
    logger.warning("Insight Hub proto not available: %s", e)


class InsightHubSync:
    """Background pull from Insight Hub."""

    def __init__(
        self,
        hub_addr: str,
        node_history: Dict[str, List[Dict[str, Any]]],
        history_lock: threading.Lock,
        max_history_size: int,
        interval_sec: int = 30,
    ):
        self.hub_addr = hub_addr
        self.node_history = node_history
        self.history_lock = history_lock
        self.max_history_size = max_history_size
        self.interval_sec = max(5, interval_sec)
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def start(self) -> None:
        logger.info(
            "[hub→forecaster] InsightHubSync.start entered: INSIGHT_HUB_ADDR='%s', HUB_AVAILABLE=%s",
            self.hub_addr,
            HUB_AVAILABLE,
        )
        if not HUB_AVAILABLE or not self.hub_addr:
            logger.warning(
                "[hub→forecaster] InsightHubSync.start skipped: INSIGHT_HUB_ADDR='%s', HUB_AVAILABLE=%s",
                self.hub_addr,
                HUB_AVAILABLE,
            )
            return
        self._thread = threading.Thread(target=self._loop, name="insight-hub-sync", daemon=True)
        self._thread.start()
        logger.info(
            "[hub→forecaster] InsightHubSync.start thread started: INSIGHT_HUB_ADDR='%s', "
            "thread_name=%s, alive=%s, interval_sec=%s",
            self.hub_addr,
            self._thread.name,
            self._thread.is_alive(),
            self.interval_sec,
        )

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=5)

    def _loop(self) -> None:
        logger.info(
            "[hub→forecaster] _loop entered: INSIGHT_HUB_ADDR='%s', interval_sec=%s",
            self.hub_addr,
            self.interval_sec,
        )
        while not self._stop.is_set():
            try:
                logger.info("[hub→forecaster] _loop tick: starting _pull_once")
                self._pull_once()
            except Exception as e:
                logger.exception("[hub→forecaster] _loop error during _pull_once: %s", e)
            self._stop.wait(self.interval_sec)

    def _pull_once(self) -> None:
        logger.info(
            "[hub→forecaster] _pull_once entered: INSIGHT_HUB_ADDR='%s', HUB_AVAILABLE=%s",
            self.hub_addr,
            HUB_AVAILABLE,
        )
        channel = grpc.insecure_channel(self.hub_addr)
        try:
            try:
                grpc.channel_ready_future(channel).result(timeout=10)
            except Exception as e:
                logger.warning(
                    "[hub→forecaster] channel not ready: INSIGHT_HUB_ADDR='%s', reason=%s",
                    self.hub_addr,
                    e,
                )
                return

            stub = hub_pb2_grpc.InsightHubServiceStub(channel)
            nodes = stub.ListNodes(hub_pb2.ListNodesRequest())
            node_names = list(nodes.node_names)
            logger.info(
                "[hub→forecaster] ListNodes result: INSIGHT_HUB_ADDR='%s', node_count=%d",
                self.hub_addr,
                len(node_names),
            )
            if not node_names:
                logger.info("[hub→forecaster] no nodes returned from ListNodes")
                return

            total_samples = 0
            per_node_counts: Dict[str, int] = {}
            for node_name in node_names:
                resp = stub.GetNodeHistory(
                    hub_pb2.GetNodeHistoryRequest(
                        node_name=node_name, since_unix_ms=0, max_snapshots=20000
                    )
                )
                per_node_counts[node_name] = len(resp.snapshots)
                hist: List[Dict[str, Any]] = []
                for s in resp.snapshots:
                    ts = datetime.fromtimestamp(s.timestamp_unix_ms / 1000.0, tz=timezone.utc)
                    hist.append(
                        {
                            "timestamp": ts.replace(tzinfo=None),
                            "cpu": s.cpu_utilization,
                            "memory": s.memory_utilization,
                            "gpu": s.gpu_utilization,
                            "storage_io": s.storage_io_utilization,
                        }
                    )
                hist.sort(key=lambda x: x["timestamp"])
                if len(hist) > self.max_history_size:
                    hist = hist[-self.max_history_size :]

                with self.history_lock:
                    self.node_history[node_name] = hist

                total_samples += len(hist)

            logger.info(
                "[hub→forecaster] pulled from Insight Hub addr=%s nodes=%d total_samples=%d "
                "(GetNodeHistory → forecaster in-memory node_history)",
                self.hub_addr,
                len(node_names),
                total_samples,
            )
            logger.info(
                "[hub→forecaster] GetNodeHistory summary: INSIGHT_HUB_ADDR='%s', per_node=%s",
                self.hub_addr,
                per_node_counts,
            )
        finally:
            channel.close()


def start_hub_sync_if_configured(
    node_history: Dict[str, List[Dict[str, Any]]],
    history_lock: threading.Lock,
    max_history_size: int,
) -> Optional[InsightHubSync]:
    addr = os.environ.get("INSIGHT_HUB_ADDR", "").strip()
    logger.info(
        "[hub→forecaster] start_hub_sync_if_configured entered: INSIGHT_HUB_ADDR='%s', HUB_AVAILABLE=%s",
        addr,
        HUB_AVAILABLE,
    )
    if not addr or not HUB_AVAILABLE:
        logger.warning(
            "[hub→forecaster] sync disabled: INSIGHT_HUB_ADDR='%s', HUB_AVAILABLE=%s",
            addr,
            HUB_AVAILABLE,
        )
        return None
    interval = int(os.environ.get("INSIGHT_HUB_SYNC_INTERVAL_SEC", "30"))
    logger.info(
        "[hub→forecaster] creating sync: INSIGHT_HUB_ADDR='%s', interval_sec=%s",
        addr,
        interval,
    )
    sync = InsightHubSync(addr, node_history, history_lock, max_history_size, interval_sec=interval)
    sync.start()
    logger.info("[hub→forecaster] start_hub_sync_if_configured completed: thread_start_requested=true")
    return sync
