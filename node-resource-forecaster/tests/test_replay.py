import numpy as np
import pandas as pd
from training.data.replay import replay_trace, ReplayResult


def _nodes():
    # Cluster capacity: 1000 cpu_milli, 2000 memory_mib, 2 gpu
    return pd.DataFrame([
        {"cpu_milli": 1000, "memory_mib": 2000, "gpu": 2},
    ])


def _pods():
    # One pod uses half cpu/mem and 1 gpu, scheduled at t=60, deleted at t=180.
    return pd.DataFrame([
        {"cpu_milli": 500, "memory_mib": 1000, "num_gpu": 1,
         "creation_time": 0, "scheduled_time": 60, "deletion_time": 180},
    ])


def test_occupancy_reflects_running_pod():
    r = replay_trace(_nodes(), _pods(), step_seconds=60)
    assert isinstance(r, ReplayResult)
    # times: 0, 60, 120, 180 (>= min creation, <= max deletion)
    assert r.times[0] == 0
    # At t=0 the pod is created but not scheduled -> 0 occupancy, 1 pending.
    assert r.occupancy["cpu"][0] == 0.0
    assert r.queue_pending[0] == 1
    # At t=60 it becomes running -> cpu occ = 500/1000 = 0.5, gpu = 1/2 = 0.5.
    i60 = r.times.index(60)
    assert abs(r.occupancy["cpu"][i60] - 0.5) < 1e-9
    assert abs(r.occupancy["gpu"][i60] - 0.5) < 1e-9
    assert r.queue_admitted[i60] == 1


def test_deterministic():
    a = replay_trace(_nodes(), _pods(), step_seconds=60)
    b = replay_trace(_nodes(), _pods(), step_seconds=60)
    assert a.times == b.times
    assert np.array_equal(a.occupancy["cpu"], b.occupancy["cpu"])
