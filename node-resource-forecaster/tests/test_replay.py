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
    assert np.array_equal(a.occupancy["memory"], b.occupancy["memory"])
    assert np.array_equal(a.occupancy["gpu"], b.occupancy["gpu"])
    assert np.array_equal(a.queue_pending, b.queue_pending)
    assert np.array_equal(a.queue_admitted, b.queue_admitted)


def test_nan_scheduled_time_never_scheduled():
    """A pod with NaN scheduled_time is never scheduled: stays pending at every step,
    contributes 0 to occupancy throughout."""
    nodes = _nodes()
    pods = pd.DataFrame([
        {"cpu_milli": 500, "memory_mib": 1000, "num_gpu": 1,
         "creation_time": 0, "scheduled_time": float("nan"), "deletion_time": float("nan")},
    ])
    r = replay_trace(nodes, pods, step_seconds=60)
    # Occupancy must be zero at every step — pod never runs.
    assert np.all(r.occupancy["cpu"] == 0.0), "cpu occupancy should be 0 for never-scheduled pod"
    assert np.all(r.occupancy["memory"] == 0.0), "memory occupancy should be 0 for never-scheduled pod"
    assert np.all(r.occupancy["gpu"] == 0.0), "gpu occupancy should be 0 for never-scheduled pod"
    # Pod is pending at every step (created at t=0, never scheduled).
    assert np.all(r.queue_pending >= 1), "never-scheduled pod must appear in queue_pending at every step"


def test_nan_deletion_time_runs_forever():
    """A pod with NaN deletion_time runs indefinitely once scheduled:
    must be counted in occupancy for every step at/after its scheduled_time."""
    nodes = _nodes()
    # Pod scheduled at t=60, deletion_time=NaN (never deleted).
    pods = pd.DataFrame([
        {"cpu_milli": 500, "memory_mib": 1000, "num_gpu": 1,
         "creation_time": 0, "scheduled_time": 60, "deletion_time": float("nan")},
    ])
    r = replay_trace(nodes, pods, step_seconds=60)
    i60 = r.times.index(60)
    # At t=60 and every subsequent step, the pod is running.
    for i in range(i60, len(r.times)):
        assert abs(r.occupancy["cpu"][i] - 0.5) < 1e-9, (
            f"cpu occupancy should be 0.5 at t={r.times[i]}, got {r.occupancy['cpu'][i]}"
        )
        assert abs(r.occupancy["memory"][i] - 0.5) < 1e-9, (
            f"memory occupancy should be 0.5 at t={r.times[i]}, got {r.occupancy['memory'][i]}"
        )
        assert abs(r.occupancy["gpu"][i] - 0.5) < 1e-9, (
            f"gpu occupancy should be 0.5 at t={r.times[i]}, got {r.occupancy['gpu'][i]}"
        )
    # Before scheduling (t=0 through t<60), occupancy must be 0.
    for i in range(i60):
        assert r.occupancy["cpu"][i] == 0.0, (
            f"cpu occupancy should be 0.0 before scheduled_time at t={r.times[i]}"
        )


def test_max_steps_caps_series_length():
    """max_steps=100 on a span of 600000s (10001 uncapped steps) must produce <= 100 steps,
    and all arrays must have the same length as times."""
    nodes = _nodes()
    pods = pd.DataFrame([
        {"cpu_milli": 500, "memory_mib": 1000, "num_gpu": 1,
         "creation_time": 0, "scheduled_time": 60, "deletion_time": 600000},
    ])
    r = replay_trace(nodes, pods, step_seconds=60, max_steps=100)
    assert len(r.times) <= 100, f"Expected <= 100 steps, got {len(r.times)}"
    assert len(r.occupancy["cpu"]) == len(r.times)
    assert len(r.occupancy["memory"]) == len(r.times)
    assert len(r.occupancy["gpu"]) == len(r.times)
    assert len(r.queue_pending) == len(r.times)
    assert len(r.queue_admitted) == len(r.times)


def test_max_steps_none_unchanged():
    """max_steps=None (default) must produce identical results to not passing max_steps."""
    a = replay_trace(_nodes(), _pods(), step_seconds=60)
    b = replay_trace(_nodes(), _pods(), step_seconds=60, max_steps=None)
    assert a.times == b.times
    assert np.array_equal(a.occupancy["cpu"], b.occupancy["cpu"])


def test_step_boundary_times_and_half_open_interval():
    """Using the single-pod fixture (cpu 500/1000, scheduled@60, deleted@180):
    - times must be exactly [0, 60, 120, 180]
    - at t=180 the pod is excluded (half-open [scheduled, deletion)) so occupancy drops to 0.
    """
    r = replay_trace(_nodes(), _pods(), step_seconds=60)
    assert r.times == [0, 60, 120, 180], f"Expected times [0,60,120,180], got {r.times}"
    i180 = r.times.index(180)
    assert r.occupancy["cpu"][i180] == 0.0, (
        f"cpu occupancy at t=180 (deletion boundary) should be 0.0, got {r.occupancy['cpu'][i180]}"
    )
    assert r.occupancy["memory"][i180] == 0.0, (
        f"memory occupancy at t=180 should be 0.0, got {r.occupancy['memory'][i180]}"
    )
    assert r.occupancy["gpu"][i180] == 0.0, (
        f"gpu occupancy at t=180 should be 0.0, got {r.occupancy['gpu'][i180]}"
    )
