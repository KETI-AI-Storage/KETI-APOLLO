"""Replay the GPU-2023 scheduling trace into allocation-occupancy + queue series.

The trace has no utilization samples; we reconstruct, per step, the cluster
allocation occupancy = sum(requests of running pods) / sum(node capacity).
A pod is "running" on [scheduled_time, deletion_time). This is the same kind of
signal the runtime forecaster sees (requests/capacity), so train==serve.
"""
from dataclasses import dataclass
import numpy as np
import pandas as pd


@dataclass
class ReplayResult:
    times: list
    occupancy: dict          # {"cpu"|"memory"|"gpu": np.ndarray (n_steps,)}
    queue_pending: np.ndarray
    queue_admitted: np.ndarray


def _cap(nodes_df):
    return {
        "cpu": float(nodes_df["cpu_milli"].sum()) or 1.0,
        "memory": float(nodes_df["memory_mib"].sum()) or 1.0,
        "gpu": float(nodes_df["gpu"].sum()) or 1.0,
    }


def replay_trace(nodes_df: pd.DataFrame, pods_df: pd.DataFrame,
                 step_seconds: int = 60, gpu_capacity_per_unit: float = 1.0) -> ReplayResult:
    cap = _cap(nodes_df)

    created = pods_df["creation_time"].fillna(0).astype(float).to_numpy()
    scheduled = pods_df["scheduled_time"].astype(float).to_numpy()  # may be NaN
    deleted = pods_df["deletion_time"].astype(float).to_numpy()     # may be NaN

    t_start = int(np.nanmin(np.concatenate([created, scheduled[~np.isnan(scheduled)] if np.any(~np.isnan(scheduled)) else created])))
    finite_del = deleted[~np.isnan(deleted)]
    t_end = int(np.nanmax(finite_del)) if finite_del.size else int(np.nanmax(created)) + step_seconds
    if t_end <= t_start:
        t_end = t_start + step_seconds

    times = list(range(t_start, t_end + 1, step_seconds))
    n = len(times)
    occ = {k: np.zeros(n, dtype=np.float64) for k in ("cpu", "memory", "gpu")}
    pending = np.zeros(n, dtype=np.int64)
    admitted = np.zeros(n, dtype=np.int64)

    cpu_req = pods_df["cpu_milli"].fillna(0).astype(float).to_numpy()
    mem_req = pods_df["memory_mib"].fillna(0).astype(float).to_numpy()
    gpu_req = pods_df["num_gpu"].fillna(0).astype(float).to_numpy() * gpu_capacity_per_unit

    for i, t in enumerate(times):
        running = (~np.isnan(scheduled)) & (scheduled <= t) & (np.isnan(deleted) | (deleted > t))
        is_pending = (created <= t) & (np.isnan(scheduled) | (scheduled > t))
        just_admitted = (~np.isnan(scheduled)) & (scheduled > t - step_seconds) & (scheduled <= t)

        occ["cpu"][i] = min(cpu_req[running].sum() / cap["cpu"], 1.0)
        occ["memory"][i] = min(mem_req[running].sum() / cap["memory"], 1.0)
        occ["gpu"][i] = min(gpu_req[running].sum() / cap["gpu"], 1.0)
        pending[i] = int(is_pending.sum())
        admitted[i] = int(just_admitted.sum())

    return ReplayResult(times=times, occupancy=occ, queue_pending=pending, queue_admitted=admitted)
