"""Download + cache the Alibaba GPU-2023 (openb) trace CSVs.

Public CSVs from github.com/alibaba/clusterdata. Idempotent: re-uses cache.
"""
import os
import urllib.request
import pandas as pd

TRACE_BASE_URL = (
    "https://raw.githubusercontent.com/alibaba/clusterdata/master/"
    "cluster-trace-gpu-v2023/csv/"
)
NODE_FILE = "openb_node_list_gpu_node.csv"
POD_FILE = "openb_pod_list_default.csv"


def _download(url: str, dst: str) -> None:
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    urllib.request.urlretrieve(url, dst)  # noqa: S310 (trusted public dataset URL)


def ensure_trace(cache_dir: str) -> tuple:
    os.makedirs(cache_dir, exist_ok=True)
    out = []
    for fname in (NODE_FILE, POD_FILE):
        dst = os.path.join(cache_dir, fname)
        if not os.path.exists(dst) or os.path.getsize(dst) == 0:
            _download(TRACE_BASE_URL + fname, dst)
        out.append(dst)
    return out[0], out[1]


def load_trace(cache_dir: str) -> tuple:
    node_path, pod_path = ensure_trace(cache_dir)
    return pd.read_csv(node_path), pd.read_csv(pod_path)
