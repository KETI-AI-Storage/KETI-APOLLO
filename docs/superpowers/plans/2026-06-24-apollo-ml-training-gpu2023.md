# APOLLO Real ML Training & Loading (GPU-2023) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Train APOLLO's LSTM-Attention forecaster and 4 of 7 LightGBM policy heads on the Alibaba GPU-2023 trace, bake the artifacts into the forecaster image, and load them at runtime — replacing the never-trained / random / threshold-fallback state with real, honestly-scoped models.

**Architecture:** A new offline `training/` package replays the GPU-2023 scheduling trace into per-node **allocation-occupancy** time series + queue metrics, trains the existing (untouched) `LSTMAttentionForecaster` self-supervised on those series, then trains the existing `MultiLightGBMPolicyEngine`'s 4 data-supported heads (node_health, autoscale, migration, provisioning) on forward-looking trace-derived labels — reusing the engine's own `_build_features` so training and serving features cannot drift. The 3 storage/IO heads (caching, storage_tiering, load_balancing) stay rule-based. A multi-stage Dockerfile trains at build time and copies artifacts into the runtime image; the server loads them and logs which heads are model-backed vs rule-backed.

**Tech Stack:** Python 3, PyTorch (existing LSTM-Attention), LightGBM (existing 7-head engine), pandas/numpy (trace replay), scikit-learn (metrics), pytest (tests).

## Global Constraints

- **Scope (verbatim from spec §3):** Train LSTM-Attention forecaster + 4 LightGBM heads (`node_health`, `autoscale`, `migration`, `provisioning`). Keep 3 heads rule-based (`caching`, `storage_tiering`, `load_balancing`). **PPO / scheduling-policy-engine: NO training, NO isolation guard this round.** **No synthetic data.** No live-usage/storage-IO/cache telemetry plumbing (Year-3).
- **Data source:** Alibaba `cluster-trace-gpu-v2023` (`openb`) public CSVs from `https://github.com/alibaba/clusterdata`. It is a scheduling-request trace (no utilization time-series, no storage/IO).
- **Occupancy-consistent:** forecaster forecasts **allocation occupancy** (`requests/capacity`, range 0–1), the same signal kind the runtime sees. Name it "allocation occupancy forecasting" everywhere; never claim true-utilization.
- **Train == serve feature contract:** the 25-feature LightGBM vector MUST be produced by the engine's own `MultiLightGBMPolicyEngine._build_features`; do not re-implement it.
- **input_dim consistency:** the trained LSTM artifact and the serving model MUST be constructed from the SAME `LSTMAttentionConfig` (occupancy config, `input_dim=4`, `output_dim=4`, horizons `[15,30,60,120]`, `sequence_length=60`). Use the shared factory from Task 4 on both sides.
- **Existing model code is NOT modified** except to add the shared config module (Task 4) and the server wiring/logging (Task 11). `models/multi_lightgbm.py` and `models/lstm_attention.py` training/load methods are reused as-is.
- **Determinism:** `train.py` sets seeds (`numpy`, `torch`, `random`) so artifacts are reproducible from source.
- **Commit convention (this repo, `apollo`):** author `evergyeol95 <evergyeol@gmail.com>`, conventional-commit messages (`feat:`/`fix:`/`test:`/`docs:`/`chore:`), **no Claude trailer**. Branch: `main`.
- **Artifacts:** `lstm_attention.pt` + `{node_health,autoscale,migration,provisioning}_model.txt`. Built into image; `MODEL_PATH`/`--policy-models` override preserved. Never commit binary artifacts (gitignore them).
- **Environment:** training/tests need `torch`, `lightgbm`, `pandas`, `scikit-learn`, `pytest` (not in base `requirements.txt`). Install `requirements-train.txt` (Task 1) before running anything. Run on a host with pip network access (build box).
- **Paths:** all paths below are relative to `apollo/node-resource-forecaster/` unless stated.

---

### Task 1: Training package skeleton + training dependencies

**Files:**
- Create: `node-resource-forecaster/training/__init__.py`
- Create: `node-resource-forecaster/training/data/__init__.py`
- Create: `node-resource-forecaster/requirements-train.txt`
- Create: `node-resource-forecaster/tests/__init__.py`
- Create: `node-resource-forecaster/tests/conftest.py`
- Create: `node-resource-forecaster/pytest.ini`
- Modify: `node-resource-forecaster/.gitignore` (create if absent)

**Interfaces:**
- Produces: importable `training` and `training.data` packages; `tests/` discoverable by pytest; `conftest.py` puts the component root on `sys.path` so `import models...` and `import training...` work from tests.

- [ ] **Step 1: Create the package + test scaffolding files**

`training/__init__.py`:
```python
"""Offline training pipeline for the APOLLO node-resource-forecaster.

Trains the LSTM-Attention forecaster and the 4 data-supported LightGBM
policy heads on the Alibaba GPU-2023 trace (allocation-occupancy signal).
Not imported by the serving path.
"""
```

`training/data/__init__.py`:
```python
"""GPU-2023 trace acquisition and replay."""
```

`tests/__init__.py`:
```python
```

`tests/conftest.py`:
```python
import os
import sys

# Make the component root importable so tests can `import models` and `import training`.
COMPONENT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if COMPONENT_ROOT not in sys.path:
    sys.path.insert(0, COMPONENT_ROOT)
```

`pytest.ini`:
```ini
[pytest]
testpaths = tests
python_files = test_*.py
addopts = -q
```

`requirements-train.txt`:
```text
# Training/test-only dependencies (NOT needed by the serving base image's runtime,
# but installed in the Dockerfile train stage). torch/lightgbm match the Dockerfile.
torch>=2.0.0
lightgbm>=4.0.0
pandas>=2.0.0
scikit-learn>=1.3.0
numpy>=1.24.0
pytest>=7.4.0
```

`.gitignore` (append or create):
```text
# Trained model artifacts (baked into image at build, never committed)
artifacts/
*.pt
*_model.txt
# Trace cache
training/data/cache/
__pycache__/
*.pyc
.venv/
```

- [ ] **Step 2: Install training dependencies**

Run:
```bash
cd node-resource-forecaster
python3 -m pip install -r requirements-train.txt
```
Expected: installs torch, lightgbm, pandas, scikit-learn, pytest without error.

- [ ] **Step 3: Verify package imports and pytest discovers an empty suite**

Run:
```bash
cd node-resource-forecaster && python3 -c "import training, training.data; print('ok')" && python3 -m pytest
```
Expected: prints `ok`; pytest reports `no tests ran` (exit code 5) — acceptable here.

- [ ] **Step 4: Commit**

```bash
git add training/__init__.py training/data/__init__.py tests/__init__.py tests/conftest.py pytest.ini requirements-train.txt .gitignore
git commit -m "chore(forecaster): scaffold training package + train/test deps"
```

---

### Task 2: GPU-2023 trace replay → occupancy + queue series

**Files:**
- Create: `node-resource-forecaster/training/data/replay.py`
- Test: `node-resource-forecaster/tests/test_replay.py`

**Interfaces:**
- Consumes: pandas DataFrames `nodes_df` (columns `cpu_milli`, `memory_mib`, `gpu`) and `pods_df` (columns `cpu_milli`, `memory_mib`, `num_gpu`, `creation_time`, `scheduled_time`, `deletion_time`; times in seconds, may be NaN for never-scheduled/never-deleted).
- Produces:
  - `ReplayResult` dataclass with:
    - `times: list[int]` — step timestamps (seconds, relative to trace start).
    - `occupancy: dict[str, np.ndarray]` — keys `"cpu"`,`"memory"`,`"gpu"`; each array shape `(n_steps,)`, cluster-wide occupancy ratio in `[0,1]` (sum of running pod requests / sum of node capacity).
    - `queue_pending: np.ndarray` shape `(n_steps,)` — count of pods created-but-not-yet-scheduled at each step.
    - `queue_admitted: np.ndarray` shape `(n_steps,)` — count newly scheduled within the step.
  - `replay_trace(nodes_df, pods_df, step_seconds=60, gpu_capacity_per_unit=1.0) -> ReplayResult`

- [ ] **Step 1: Write the failing test**

`tests/test_replay.py`:
```python
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_replay.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.data.replay'`.

- [ ] **Step 3: Write minimal implementation**

`training/data/replay.py`:
```python
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_replay.py -v`
Expected: PASS (2 passed).

- [ ] **Step 5: Commit**

```bash
git add training/data/replay.py tests/test_replay.py
git commit -m "feat(forecaster): replay GPU-2023 trace into occupancy + queue series"
```

---

### Task 3: GPU-2023 trace acquisition (download + cache)

**Files:**
- Create: `node-resource-forecaster/training/data/acquire.py`
- Test: `node-resource-forecaster/tests/test_acquire.py`

**Interfaces:**
- Produces:
  - `TRACE_BASE_URL` (str), `NODE_FILE` = `"openb_node_list_gpu_node.csv"`, `POD_FILE` = `"openb_pod_list_default.csv"`.
  - `ensure_trace(cache_dir: str) -> tuple[str, str]` — returns `(node_csv_path, pod_csv_path)`; downloads only if missing; idempotent.
  - `load_trace(cache_dir: str) -> tuple[pandas.DataFrame, pandas.DataFrame]` — `ensure_trace` then read CSVs as `(nodes_df, pods_df)`.

- [ ] **Step 1: Write the failing test**

`tests/test_acquire.py`:
```python
import os
from training.data import acquire


def test_ensure_trace_skips_download_when_cached(tmp_path, monkeypatch):
    # Pre-create cached files; download must NOT be called.
    node = tmp_path / acquire.NODE_FILE
    pod = tmp_path / acquire.POD_FILE
    node.write_text("cpu_milli,memory_mib,gpu\n1000,2000,2\n")
    pod.write_text("cpu_milli,memory_mib,num_gpu,creation_time,scheduled_time,deletion_time\n500,1000,1,0,60,180\n")

    called = {"n": 0}
    monkeypatch.setattr(acquire, "_download", lambda url, dst: called.__setitem__("n", called["n"] + 1))

    n_path, p_path = acquire.ensure_trace(str(tmp_path))
    assert os.path.exists(n_path) and os.path.exists(p_path)
    assert called["n"] == 0  # no download because cached


def test_load_trace_returns_dataframes(tmp_path, monkeypatch):
    (tmp_path / acquire.NODE_FILE).write_text("cpu_milli,memory_mib,gpu\n1000,2000,2\n")
    (tmp_path / acquire.POD_FILE).write_text(
        "cpu_milli,memory_mib,num_gpu,creation_time,scheduled_time,deletion_time\n500,1000,1,0,60,180\n")
    monkeypatch.setattr(acquire, "_download", lambda url, dst: (_ for _ in ()).throw(AssertionError("should not download")))
    nodes_df, pods_df = acquire.load_trace(str(tmp_path))
    assert list(nodes_df.columns)[:3] == ["cpu_milli", "memory_mib", "gpu"]
    assert int(pods_df.iloc[0]["num_gpu"]) == 1
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_acquire.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.data.acquire'`.

- [ ] **Step 3: Write minimal implementation**

`training/data/acquire.py`:
```python
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
```

> NOTE: the exact CSV sub-path (`/csv/`) on the upstream repo may differ. If `ensure_trace`
> 404s at execution time, fetch the real raw path from
> `https://github.com/alibaba/clusterdata/tree/master/cluster-trace-gpu-v2023` and update
> `TRACE_BASE_URL`/file names. The cache-skip logic (the tested behavior) is unaffected.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_acquire.py -v`
Expected: PASS (2 passed).

- [ ] **Step 5: Commit**

```bash
git add training/data/acquire.py tests/test_acquire.py
git commit -m "feat(forecaster): cache-aware GPU-2023 trace download"
```

---

### Task 4: Shared occupancy forecaster config

**Files:**
- Create: `node-resource-forecaster/models/forecaster_config.py`
- Test: `node-resource-forecaster/tests/test_forecaster_config.py`

**Interfaces:**
- Consumes: `models.lstm_attention.LSTMAttentionConfig`.
- Produces:
  - `OCCUPANCY_CHANNELS = ["cpu", "memory", "gpu", "pending"]`
  - `OCCUPANCY_INPUT_DIM = 4`
  - `make_occupancy_forecaster_config() -> LSTMAttentionConfig` (input_dim=4, output_dim=4, sequence_length=60, horizons [15,30,60,120]).
  - This factory is the SINGLE SOURCE for both training (Task 7) and serving (Task 11).

- [ ] **Step 1: Write the failing test**

`tests/test_forecaster_config.py`:
```python
from models.forecaster_config import (
    make_occupancy_forecaster_config, OCCUPANCY_INPUT_DIM, OCCUPANCY_CHANNELS,
)


def test_occupancy_config_dims():
    cfg = make_occupancy_forecaster_config()
    assert cfg.input_dim == OCCUPANCY_INPUT_DIM == 4
    assert cfg.output_dim == 4
    assert cfg.sequence_length == 60
    assert cfg.forecast_horizons == [15, 30, 60, 120]
    assert OCCUPANCY_CHANNELS == ["cpu", "memory", "gpu", "pending"]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_forecaster_config.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'models.forecaster_config'`.

- [ ] **Step 3: Write minimal implementation**

`models/forecaster_config.py`:
```python
"""Shared LSTM-Attention config for the allocation-occupancy forecaster.

SINGLE SOURCE for input_dim/output_dim so the trained artifact and the serving
model are constructed identically (else state_dict load shape-mismatches).
"""
from models.lstm_attention import LSTMAttentionConfig

OCCUPANCY_CHANNELS = ["cpu", "memory", "gpu", "pending"]
OCCUPANCY_INPUT_DIM = 4


def make_occupancy_forecaster_config() -> LSTMAttentionConfig:
    return LSTMAttentionConfig(
        input_dim=OCCUPANCY_INPUT_DIM,
        output_dim=4,
        sequence_length=60,
        forecast_horizons=[15, 30, 60, 120],
    )
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_forecaster_config.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add models/forecaster_config.py tests/test_forecaster_config.py
git commit -m "feat(forecaster): shared occupancy LSTM config (single source for input_dim)"
```

---

### Task 5: Feature/sequence assembly (occupancy → LSTM sequences; replay row → engine state dicts)

**Files:**
- Create: `node-resource-forecaster/training/features.py`
- Test: `node-resource-forecaster/tests/test_features.py`

**Interfaces:**
- Consumes: `ReplayResult` (Task 2); `OCCUPANCY_CHANNELS` (Task 4).
- Produces:
  - `PENDING_NORM = 50.0` (divisor mapping pending-count → [0,1] pressure, clipped).
  - `build_lstm_sequences(replay, seq_len=60) -> tuple[np.ndarray, dict[int, np.ndarray]]`:
    returns `X` shape `(n_samples, seq_len, 4)` and `Y` = `{h: array (n_samples, 4)}` for each horizon h in [15,30,60,120]. Channels order = `[cpu, memory, gpu, pending_norm]`. A sample at index i uses window `[i, i+seq_len)`; targets are the channel values at `i+seq_len+h` (samples without a full future horizon are dropped).
  - `current_state_at(replay, idx) -> dict` with keys `cpu_util, memory_util, gpu_util, pending_pods` (the engine's `current_state` contract).
  - `node_info_at(replay, idx, node_type="gpu") -> dict` with `node_name, node_type, queue_status={pending, admitted}`.
  - `lstm_pred_to_engine_dict(model_predict_out) -> dict[int, dict]` — adapts `LSTMAttentionForecaster.predict` output keys (`predicted_cpu`...) which already match the engine's expected `lstm_predictions` contract; returns as-is (identity) so callers have one obvious entry point.

- [ ] **Step 1: Write the failing test**

`tests/test_features.py`:
```python
import numpy as np
from training.data.replay import ReplayResult
from training.features import (
    build_lstm_sequences, current_state_at, node_info_at, PENDING_NORM,
)


def _replay(n=200):
    t = list(range(0, n * 60, 60))
    occ = {
        "cpu": np.linspace(0.1, 0.9, n),
        "memory": np.linspace(0.2, 0.6, n),
        "gpu": np.full(n, 0.3),
    }
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=np.arange(n) % 10,
                        queue_admitted=np.ones(n, dtype=int))


def test_sequence_shapes_and_channels():
    r = _replay(200)
    X, Y = build_lstm_sequences(r, seq_len=60)
    assert X.shape[1:] == (60, 4)
    assert set(Y.keys()) == {15, 30, 60, 120}
    assert Y[120].shape[0] == X.shape[0]
    assert Y[15].shape[1] == 4
    # channel 0 of last input row equals cpu occupancy at that step
    assert abs(X[0, -1, 0] - r.occupancy["cpu"][59]) < 1e-6
    # pending channel normalized into [0,1]
    assert X[:, :, 3].max() <= 1.0


def test_current_state_contract():
    r = _replay(70)
    s = current_state_at(r, 10)
    assert set(s) >= {"cpu_util", "memory_util", "gpu_util", "pending_pods"}
    info = node_info_at(r, 10)
    assert info["queue_status"]["pending"] == int(r.queue_pending[10])
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_features.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.features'`.

- [ ] **Step 3: Write minimal implementation**

`training/features.py`:
```python
"""Turn a ReplayResult into LSTM training sequences and into the engine's
`current_state` / `node_info` dicts. The 25-feature LightGBM vector is built by
the engine's own `_build_features` (Task 7) — NOT here — so train==serve.
"""
import numpy as np

HORIZONS = [15, 30, 60, 120]
PENDING_NORM = 50.0  # pending count -> [0,1] pressure (clipped)


def _channels(replay):
    pending_norm = np.clip(replay.queue_pending / PENDING_NORM, 0.0, 1.0)
    return np.stack([
        replay.occupancy["cpu"],
        replay.occupancy["memory"],
        replay.occupancy["gpu"],
        pending_norm,
    ], axis=1)  # (n_steps, 4)


def build_lstm_sequences(replay, seq_len: int = 60):
    chans = _channels(replay)            # (T, 4)
    T = chans.shape[0]
    max_h = max(HORIZONS)
    X, Y = [], {h: [] for h in HORIZONS}
    last = T - seq_len - max_h
    for i in range(0, max(last, 0)):
        X.append(chans[i:i + seq_len])
        base = i + seq_len
        for h in HORIZONS:
            Y[h].append(chans[base + h])
    X = np.asarray(X, dtype=np.float32)
    Y = {h: np.asarray(v, dtype=np.float32) for h, v in Y.items()}
    return X, Y


def current_state_at(replay, idx: int) -> dict:
    return {
        "cpu_util": float(replay.occupancy["cpu"][idx]),
        "memory_util": float(replay.occupancy["memory"][idx]),
        "gpu_util": float(replay.occupancy["gpu"][idx]),
        "pending_pods": float(min(replay.queue_pending[idx] / PENDING_NORM, 1.0)),
    }


def node_info_at(replay, idx: int, node_type: str = "gpu") -> dict:
    return {
        "node_name": "trace-cluster",
        "node_type": node_type,
        "queue_status": {
            "pending": int(replay.queue_pending[idx]),
            "admitted": int(replay.queue_admitted[idx]),
        },
    }


def lstm_pred_to_engine_dict(model_predict_out: dict) -> dict:
    # LSTMAttentionForecaster.predict already returns {h: {predicted_cpu, ...}},
    # which is exactly the engine's lstm_predictions contract.
    return model_predict_out
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_features.py -v`
Expected: PASS (2 passed).

- [ ] **Step 5: Commit**

```bash
git add training/features.py tests/test_features.py
git commit -m "feat(forecaster): occupancy->LSTM sequences + engine state-dict adapters"
```

---

### Task 6: Forward-looking labels for the 4 trained heads

**Files:**
- Create: `node-resource-forecaster/training/labels.py`
- Test: `node-resource-forecaster/tests/test_labels.py`

**Interfaces:**
- Consumes: `ReplayResult` (Task 2).
- Produces:
  - `STRESSED, CRITICAL = 0.70, 0.85` (occupancy bands).
  - `derive_labels(replay, label_horizon=30) -> dict[str, np.ndarray]` returning arrays aligned with `build_lstm_sequences` samples (same count, same i→i+seq_len+label_horizon indexing) for the 4 heads:
    - `"node_health"`: 3-class (0=NORMAL,1=STRESSED,2=CRITICAL) from FUTURE max(cpu,mem) occupancy at `+label_horizon`.
    - `"autoscale"`: 1 if FUTURE pending pressure ≥ 0.5 OR future max(cpu,mem) ≥ STRESSED.
    - `"migration"`: 1 if future max(cpu,mem) occupancy ≥ CRITICAL.
    - `"provisioning"`: 1 if future gpu occupancy ≥ STRESSED.
  - The label index uses `seq_len` so it lines up 1:1 with `build_lstm_sequences(replay, seq_len)` rows. `seq_len` is a parameter (default 60).

- [ ] **Step 1: Write the failing test**

`tests/test_labels.py`:
```python
import numpy as np
from training.data.replay import ReplayResult
from training.labels import derive_labels, STRESSED, CRITICAL


def _replay_rising(n=200):
    t = list(range(0, n * 60, 60))
    occ = {"cpu": np.linspace(0.1, 0.99, n),
           "memory": np.linspace(0.1, 0.5, n),
           "gpu": np.linspace(0.1, 0.95, n)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=np.zeros(n, dtype=int),
                        queue_admitted=np.zeros(n, dtype=int))


def test_label_shapes_align_and_classes_present():
    r = _replay_rising(200)
    y = derive_labels(r, label_horizon=30)
    assert set(y) == {"node_health", "autoscale", "migration", "provisioning"}
    n = len(y["node_health"])
    assert all(len(y[k]) == n for k in y)
    # node_health is multiclass with values in {0,1,2}
    assert set(np.unique(y["node_health"])).issubset({0, 1, 2})
    # rising cpu -> at least one CRITICAL migration label and one critical node_health
    assert y["migration"].max() == 1
    assert y["node_health"].max() == 2
    # provisioning fires where future gpu >= STRESSED
    assert y["provisioning"].max() == 1
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_labels.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.labels'`.

- [ ] **Step 3: Write minimal implementation**

`training/labels.py`:
```python
"""Forward-looking labels for the 4 data-supported LightGBM heads.

Labels come from the TRUE FUTURE of the replay (not current-instant thresholds),
so the model must learn to anticipate from current + forecast features.
Indices align 1:1 with training.features.build_lstm_sequences rows.
"""
import numpy as np

STRESSED, CRITICAL = 0.70, 0.85
HORIZONS_MAX = 120


def derive_labels(replay, label_horizon: int = 30, seq_len: int = 60) -> dict:
    cpu = replay.occupancy["cpu"]
    mem = replay.occupancy["memory"]
    gpu = replay.occupancy["gpu"]
    pend = np.clip(replay.queue_pending / 50.0, 0.0, 1.0)
    T = len(cpu)
    last = T - seq_len - HORIZONS_MAX
    node_health, autoscale, migration, provisioning = [], [], [], []
    for i in range(0, max(last, 0)):
        f = i + seq_len + label_horizon          # future index
        fmax = max(cpu[f], mem[f])
        if fmax >= CRITICAL:
            node_health.append(2)
        elif fmax >= STRESSED:
            node_health.append(1)
        else:
            node_health.append(0)
        autoscale.append(1 if (pend[f] >= 0.5 or fmax >= STRESSED) else 0)
        migration.append(1 if fmax >= CRITICAL else 0)
        provisioning.append(1 if gpu[f] >= STRESSED else 0)
    return {
        "node_health": np.asarray(node_health, dtype=np.int64),
        "autoscale": np.asarray(autoscale, dtype=np.int64),
        "migration": np.asarray(migration, dtype=np.int64),
        "provisioning": np.asarray(provisioning, dtype=np.int64),
    }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_labels.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add training/labels.py tests/test_labels.py
git commit -m "feat(forecaster): forward-looking labels for 4 LightGBM heads"
```

---

### Task 7: Train the LSTM-Attention forecaster (occupancy, self-supervised)

**Files:**
- Create: `node-resource-forecaster/training/train_lstm.py`
- Test: `node-resource-forecaster/tests/test_train_lstm.py`

**Interfaces:**
- Consumes: `make_occupancy_forecaster_config` (Task 4); `build_lstm_sequences` (Task 5); `models.lstm_attention.create_lstm_attention_forecaster`, `LSTMAttentionTrainer.train_epoch/save/load`, `model.predict`.
- Produces:
  - `make_batches(X, Y, batch_size) -> list[tuple[Tensor, dict[int, Tensor]]]` — batches in the exact shape `train_epoch` consumes (`batch_x`, `{h: batch_y}`).
  - `train_lstm(X, Y, epochs=5, batch_size=32, seed=0) -> (model, trainer)`.
  - `save_lstm(trainer, out_path)` (thin wrapper over `trainer.save`).

- [ ] **Step 1: Write the failing test**

`tests/test_train_lstm.py`:
```python
import numpy as np
import torch
from training.train_lstm import train_lstm, save_lstm, make_batches
from models.forecaster_config import make_occupancy_forecaster_config
from models.lstm_attention import create_lstm_attention_forecaster


def _data(n=40):
    X = np.random.RandomState(0).rand(n, 60, 4).astype("float32")
    Y = {h: np.random.RandomState(h).rand(n, 4).astype("float32") for h in (15, 30, 60, 120)}
    return X, Y


def test_make_batches_shapes():
    X, Y = _data(40)
    batches = make_batches(X, Y, batch_size=16)
    bx, by = batches[0]
    assert bx.shape == (16, 60, 4)
    assert set(by.keys()) == {15, 30, 60, 120}
    assert by[15].shape == (16, 4)


def test_train_and_load_roundtrip(tmp_path):
    X, Y = _data(40)
    model, trainer = train_lstm(X, Y, epochs=1, batch_size=16, seed=0)
    out = tmp_path / "lstm_attention.pt"
    save_lstm(trainer, str(out))
    assert out.exists()
    # reload into a fresh model built from the SAME shared config
    _, fresh_trainer = create_lstm_attention_forecaster(make_occupancy_forecaster_config())
    fresh_trainer.load(str(out))
    pred = fresh_trainer.model.predict(X[0])
    assert 15 in pred and "predicted_cpu" in pred[15]
    assert np.isfinite(pred[15]["predicted_cpu"])
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_lstm.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.train_lstm'`.

- [ ] **Step 3: Write minimal implementation**

`training/train_lstm.py`:
```python
"""Train the existing LSTM-Attention forecaster on occupancy sequences.

Self-supervised: predict future occupancy channels from a 60-step window.
Reuses LSTMAttentionTrainer unchanged; builds the dict-batch format it expects.
"""
import random
import numpy as np
import torch

from models.forecaster_config import make_occupancy_forecaster_config
from models.lstm_attention import create_lstm_attention_forecaster


def make_batches(X, Y, batch_size: int):
    n = X.shape[0]
    batches = []
    for s in range(0, n, batch_size):
        e = min(s + batch_size, n)
        bx = torch.from_numpy(np.asarray(X[s:e], dtype="float32"))
        by = {h: torch.from_numpy(np.asarray(Y[h][s:e], dtype="float32")) for h in Y}
        batches.append((bx, by))
    return batches


def train_lstm(X, Y, epochs: int = 5, batch_size: int = 32, seed: int = 0):
    random.seed(seed); np.random.seed(seed); torch.manual_seed(seed)
    cfg = make_occupancy_forecaster_config()
    cfg.batch_size = batch_size
    model, trainer = create_lstm_attention_forecaster(cfg)
    batches = make_batches(X, Y, batch_size)
    for ep in range(epochs):
        loss = trainer.train_epoch(batches)
        print(f"[train_lstm] epoch {ep+1}/{epochs} loss={loss:.5f}")
    return model, trainer


def save_lstm(trainer, out_path: str):
    trainer.save(out_path)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_lstm.py -v`
Expected: PASS (2 passed). (CPU training of 1 epoch on 40 samples is fast.)

- [ ] **Step 5: Commit**

```bash
git add training/train_lstm.py tests/test_train_lstm.py
git commit -m "feat(forecaster): train LSTM-Attention on occupancy sequences"
```

---

### Task 8: Train the 4 LightGBM heads (engine `_build_features` reuse)

**Files:**
- Create: `node-resource-forecaster/training/train_lightgbm.py`
- Test: `node-resource-forecaster/tests/test_train_lightgbm.py`

**Interfaces:**
- Consumes: `current_state_at`, `node_info_at` (Task 5); `derive_labels` (Task 6); trained LSTM `model.predict` (Task 7); `models.multi_lightgbm.create_policy_engine`, `MultiLightGBMPolicyEngine._build_features/train_models/save_models`.
- Produces:
  - `TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]`.
  - `build_lightgbm_matrix(replay, lstm_model, seq_len=60) -> np.ndarray` shape `(n_samples, 25)`, rows aligned 1:1 with labels: for sample i, use `current_state_at(i+seq_len)`, the LSTM prediction from window `[i,i+seq_len)`, and `node_info_at(i+seq_len)`, fed through `engine._build_features` (reused → train==serve).
  - `train_lightgbm(replay, lstm_model, out_dir, seq_len=60) -> MultiLightGBMPolicyEngine` — builds X, labels for the 4 heads only, calls `engine.train_models`, `engine.save_models(out_dir)`.

- [ ] **Step 1: Write the failing test**

`tests/test_train_lightgbm.py`:
```python
import os
import numpy as np
from training.data.replay import ReplayResult
from training.train_lightgbm import train_lightgbm, build_lightgbm_matrix, TRAINED_HEADS
from training.train_lstm import train_lstm
from training.features import build_lstm_sequences


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    rng = np.random.RandomState(1)
    occ = {"cpu": np.clip(np.linspace(0.1, 0.95, n) + rng.rand(n) * 0.05, 0, 1),
           "memory": np.clip(np.linspace(0.1, 0.7, n), 0, 1),
           "gpu": np.clip(np.linspace(0.1, 0.9, n), 0, 1)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=(rng.rand(n) * 30).astype(int),
                        queue_admitted=np.ones(n, dtype=int))


def test_matrix_is_25_wide_and_train_writes_4_models(tmp_path):
    r = _replay(300)
    X, Y = build_lstm_sequences(r, seq_len=60)
    model, _ = train_lstm(X, Y, epochs=1, batch_size=32, seed=0)
    M = build_lightgbm_matrix(r, model, seq_len=60)
    assert M.shape[1] == 25
    engine = train_lightgbm(r, model, str(tmp_path), seq_len=60)
    for head in TRAINED_HEADS:
        assert os.path.exists(tmp_path / f"{head}_model.txt")
    # the 3 rule heads must NOT be trained/written
    for head in ("caching", "storage_tiering", "load_balancing"):
        assert not os.path.exists(tmp_path / f"{head}_model.txt")
        assert head not in engine.models
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_lightgbm.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.train_lightgbm'`.

- [ ] **Step 3: Write minimal implementation**

`training/train_lightgbm.py`:
```python
"""Train the 4 data-supported LightGBM heads.

Features are built by the engine's OWN _build_features (train==serve). The LSTM
predictions used as features are produced by the trained LSTM, matching serving.
Only the 4 supported heads are passed to train_models; the other 3 stay
rule-based (train_models skips tasks absent from the dicts).
"""
import numpy as np

from training.features import current_state_at, node_info_at, build_lstm_sequences
from training.labels import derive_labels
from models.multi_lightgbm import create_policy_engine

TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]


def _windows(replay, seq_len):
    # same channel stack + windowing as build_lstm_sequences, to get input windows
    X, _ = build_lstm_sequences(replay, seq_len=seq_len)
    return X  # (n_samples, seq_len, 4)


def build_lightgbm_matrix(replay, lstm_model, seq_len: int = 60) -> np.ndarray:
    engine = create_policy_engine(None)  # rule mode; we only use _build_features
    windows = _windows(replay, seq_len)
    rows = []
    for i in range(windows.shape[0]):
        idx = i + seq_len
        preds = lstm_model.predict(windows[i])           # {h: {predicted_cpu,...}}
        state = current_state_at(replay, idx)
        info = node_info_at(replay, idx)
        feat = engine._build_features(state, preds, info)  # (1, 25)
        rows.append(feat[0])
    return np.asarray(rows, dtype=np.float32)


def train_lightgbm(replay, lstm_model, out_dir: str, seq_len: int = 60):
    X = build_lightgbm_matrix(replay, lstm_model, seq_len=seq_len)
    labels = derive_labels(replay, label_horizon=30, seq_len=seq_len)
    n = min(X.shape[0], *(len(v) for v in labels.values()))
    X = X[:n]
    training_data = {h: X for h in TRAINED_HEADS}
    labels = {h: labels[h][:n] for h in TRAINED_HEADS}
    engine = create_policy_engine(None)
    engine.train_models(training_data, labels)
    engine.save_models(out_dir)
    return engine
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_lightgbm.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add training/train_lightgbm.py tests/test_train_lightgbm.py
git commit -m "feat(forecaster): train 4 LightGBM heads via engine _build_features"
```

---

### Task 9: Evaluation vs baselines + report

**Files:**
- Create: `node-resource-forecaster/training/evaluate.py`
- Test: `node-resource-forecaster/tests/test_evaluate.py`

**Interfaces:**
- Consumes: trained LSTM `model.predict`; `build_lstm_sequences`; `build_lightgbm_matrix` + `derive_labels`; a trained `MultiLightGBMPolicyEngine` and a rule engine (`create_policy_engine(None)`).
- Produces:
  - `lstm_rmse_vs_persistence(model, X, Y) -> dict` with `{"lstm_rmse", "persistence_rmse", "beats_baseline": bool}` (persistence = last input step value per channel).
  - `lightgbm_vs_threshold(trained_engine, replay, lstm_model, seq_len=60) -> dict[str, dict]` — per trained head: `{"model_acc", "rule_acc", "model_f1", "beats_baseline": bool}` (rule baseline = `create_policy_engine(None)` decisions mapped to class indices).
  - `write_report(report: dict, out_dir: str)` → writes `eval_report.json` and `eval_report.md`.

- [ ] **Step 1: Write the failing test**

`tests/test_evaluate.py`:
```python
import os
import numpy as np
from training.evaluate import lstm_rmse_vs_persistence, write_report
from training.train_lstm import train_lstm
from training.features import build_lstm_sequences
from training.data.replay import ReplayResult


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    # smooth, learnable signal -> LSTM should beat (or match) persistence
    x = np.linspace(0, 6.28, n)
    occ = {"cpu": 0.5 + 0.3 * np.sin(x), "memory": 0.5 + 0.2 * np.cos(x),
           "gpu": np.full(n, 0.3)}
    return ReplayResult(times=t, occupancy={k: np.clip(v, 0, 1) for k, v in occ.items()},
                        queue_pending=np.zeros(n, dtype=int),
                        queue_admitted=np.zeros(n, dtype=int))


def test_lstm_eval_keys_and_report(tmp_path):
    r = _replay(300)
    X, Y = build_lstm_sequences(r, seq_len=60)
    model, _ = train_lstm(X, Y, epochs=2, batch_size=32, seed=0)
    res = lstm_rmse_vs_persistence(model, X, Y)
    assert {"lstm_rmse", "persistence_rmse", "beats_baseline"} <= set(res)
    assert np.isfinite(res["lstm_rmse"]) and res["lstm_rmse"] >= 0
    write_report({"lstm": res, "lightgbm": {}}, str(tmp_path))
    assert os.path.exists(tmp_path / "eval_report.json")
    assert os.path.exists(tmp_path / "eval_report.md")
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_evaluate.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.evaluate'`.

- [ ] **Step 3: Write minimal implementation**

`training/evaluate.py`:
```python
"""Evaluate trained models against honest baselines and emit a report."""
import json
import os
import numpy as np
from sklearn.metrics import accuracy_score, f1_score

from training.features import build_lstm_sequences  # noqa: F401 (documented entrypoint)
from training.train_lightgbm import build_lightgbm_matrix, TRAINED_HEADS
from training.labels import derive_labels
from models.multi_lightgbm import create_policy_engine

HORIZONS = [15, 30, 60, 120]
_HEAD_TO_DECISION_IDX = {  # map engine decision -> class index per head
    "node_health": {"NORMAL": 0, "STRESSED": 1, "CRITICAL": 2},
    "autoscale": {"NO": 0, "YES": 1},
    "migration": {"NO": 0, "YES": 1},
    "provisioning": {"NO": 0, "YES": 1},
}


def lstm_rmse_vs_persistence(model, X, Y) -> dict:
    errs, perr = [], []
    for i in range(X.shape[0]):
        pred = model.predict(X[i])
        last = X[i, -1, :]  # persistence baseline: last observed step
        for h in HORIZONS:
            tgt = Y[h][i]
            p = np.array([pred[h]["predicted_cpu"], pred[h]["predicted_memory"],
                          pred[h]["predicted_gpu"], pred[h]["predicted_pending_pods"]])
            errs.append(np.mean((p - tgt) ** 2))
            perr.append(np.mean((last - tgt) ** 2))
    lstm_rmse = float(np.sqrt(np.mean(errs)))
    pers_rmse = float(np.sqrt(np.mean(perr)))
    return {"lstm_rmse": lstm_rmse, "persistence_rmse": pers_rmse,
            "beats_baseline": lstm_rmse <= pers_rmse}


def _engine_decision_class(engine, replay, lstm_model, seq_len, head):
    from training.features import current_state_at, node_info_at
    X = build_lightgbm_matrix(replay, lstm_model, seq_len=seq_len)  # ensures alignment
    n = X.shape[0]
    out = np.zeros(n, dtype=np.int64)
    windows, _ = build_lstm_sequences(replay, seq_len=seq_len)
    for i in range(n):
        preds = lstm_model.predict(windows[i])
        state = current_state_at(replay, i + seq_len)
        info = node_info_at(replay, i + seq_len)
        dec = engine.predict(state, preds, info)
        d = next(x for x in dec.decisions if x.task_name == head)
        out[i] = _HEAD_TO_DECISION_IDX[head].get(d.decision, 0)
    return out


def lightgbm_vs_threshold(trained_engine, replay, lstm_model, seq_len: int = 60) -> dict:
    labels = derive_labels(replay, label_horizon=30, seq_len=seq_len)
    rule_engine = create_policy_engine(None)
    report = {}
    for head in TRAINED_HEADS:
        y = labels[head]
        n = len(y)
        y_model = _engine_decision_class(trained_engine, replay, lstm_model, seq_len, head)[:n]
        y_rule = _engine_decision_class(rule_engine, replay, lstm_model, seq_len, head)[:n]
        avg = "macro" if head == "node_health" else "binary"
        report[head] = {
            "model_acc": float(accuracy_score(y, y_model)),
            "rule_acc": float(accuracy_score(y, y_rule)),
            "model_f1": float(f1_score(y, y_model, average=avg, zero_division=0)),
            "beats_baseline": bool(accuracy_score(y, y_model) >= accuracy_score(y, y_rule)),
        }
    return report


def write_report(report: dict, out_dir: str):
    os.makedirs(out_dir, exist_ok=True)
    with open(os.path.join(out_dir, "eval_report.json"), "w") as f:
        json.dump(report, f, indent=2)
    lines = ["# APOLLO ML eval report (GPU-2023, allocation-occupancy)", ""]
    lstm = report.get("lstm", {})
    if lstm:
        lines += ["## LSTM-Attention (occupancy forecast)",
                  f"- lstm_rmse: {lstm.get('lstm_rmse'):.4f}",
                  f"- persistence_rmse: {lstm.get('persistence_rmse'):.4f}",
                  f"- beats_baseline: {lstm.get('beats_baseline')}", ""]
    lg = report.get("lightgbm", {})
    if lg:
        lines += ["## LightGBM heads vs threshold baseline"]
        for head, m in lg.items():
            lines.append(f"- {head}: acc={m['model_acc']:.3f} (rule {m['rule_acc']:.3f}), "
                         f"f1={m['model_f1']:.3f}, beats={m['beats_baseline']}")
        lines.append("")
    lines += ["> Trained on the Alibaba GPU-2023 trace (allocation occupancy). "
              "Metrics are held-out on the trace; production correctness is not claimed."]
    with open(os.path.join(out_dir, "eval_report.md"), "w") as f:
        f.write("\n".join(lines))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_evaluate.py -v`
Expected: PASS. (We assert keys/finiteness/report files, not that the model wins, to avoid flakiness on tiny data.)

- [ ] **Step 5: Commit**

```bash
git add training/evaluate.py tests/test_evaluate.py
git commit -m "feat(forecaster): eval vs persistence + threshold baselines, write report"
```

---

### Task 10: Capstone `train.py` (acquire → replay → train → evaluate)

**Files:**
- Create: `node-resource-forecaster/training/train.py`
- Test: `node-resource-forecaster/tests/test_train_capstone.py`

**Interfaces:**
- Consumes: all of Tasks 2–9.
- Produces:
  - `run_training(out_dir, cache_dir, sample_pods=None, lstm_epochs=10, seed=0, replay=None) -> dict` — orchestrates the full pipeline; if `replay` is provided it skips acquire (for tests); if `sample_pods` is set, subsamples pods for a fast build. Writes `lstm_attention.pt`, 4 `*_model.txt`, `eval_report.{json,md}` into `out_dir`. Returns the eval report dict.
  - CLI: `python -m training.train --out artifacts --cache training/data/cache [--sample 2000] [--epochs 10]`.

- [ ] **Step 1: Write the failing test**

`tests/test_train_capstone.py`:
```python
import os
import numpy as np
from training.train import run_training
from training.data.replay import ReplayResult


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    x = np.linspace(0, 12.5, n)
    occ = {"cpu": np.clip(0.5 + 0.35 * np.sin(x), 0, 1),
           "memory": np.clip(0.4 + 0.2 * np.cos(x), 0, 1),
           "gpu": np.clip(0.3 + 0.3 * np.sin(x / 2), 0, 1)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=(np.abs(np.sin(x)) * 20).astype(int),
                        queue_admitted=np.ones(n, dtype=int))


def test_capstone_produces_all_artifacts(tmp_path):
    rep = run_training(out_dir=str(tmp_path), cache_dir=str(tmp_path / "cache"),
                       lstm_epochs=1, seed=0, replay=_replay(300))
    for f in ("lstm_attention.pt", "node_health_model.txt", "autoscale_model.txt",
              "migration_model.txt", "provisioning_model.txt",
              "eval_report.json", "eval_report.md"):
        assert os.path.exists(tmp_path / f), f"missing {f}"
    assert "lstm" in rep and "lightgbm" in rep
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_capstone.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.train'`.

- [ ] **Step 3: Write minimal implementation**

`training/train.py`:
```python
"""Capstone: acquire -> replay -> train LSTM -> train LightGBM -> evaluate -> save.

Deterministic (seeded). Run at Docker build (sampled) or offline (full).
"""
import argparse
import os
import random
import numpy as np
import torch

from training.data.acquire import load_trace
from training.data.replay import replay_trace
from training.features import build_lstm_sequences
from training.train_lstm import train_lstm, save_lstm
from training.train_lightgbm import train_lightgbm
from training.evaluate import lstm_rmse_vs_persistence, lightgbm_vs_threshold, write_report


def run_training(out_dir, cache_dir, sample_pods=None, lstm_epochs=10, seed=0, replay=None):
    random.seed(seed); np.random.seed(seed); torch.manual_seed(seed)
    os.makedirs(out_dir, exist_ok=True)

    if replay is None:
        nodes_df, pods_df = load_trace(cache_dir)
        if sample_pods:
            pods_df = pods_df.sample(n=min(sample_pods, len(pods_df)), random_state=seed)
        replay = replay_trace(nodes_df, pods_df, step_seconds=60)

    X, Y = build_lstm_sequences(replay, seq_len=60)
    if X.shape[0] == 0:
        raise RuntimeError("Not enough trace steps to build sequences; widen sample/time span.")

    lstm_model, trainer = train_lstm(X, Y, epochs=lstm_epochs, batch_size=32, seed=seed)
    save_lstm(trainer, os.path.join(out_dir, "lstm_attention.pt"))

    engine = train_lightgbm(replay, lstm_model, out_dir, seq_len=60)

    report = {
        "lstm": lstm_rmse_vs_persistence(lstm_model, X, Y),
        "lightgbm": lightgbm_vs_threshold(engine, replay, lstm_model, seq_len=60),
    }
    write_report(report, out_dir)
    return report


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="artifacts")
    ap.add_argument("--cache", default="training/data/cache")
    ap.add_argument("--sample", type=int, default=None)
    ap.add_argument("--epochs", type=int, default=10)
    ap.add_argument("--seed", type=int, default=0)
    args = ap.parse_args()
    rep = run_training(args.out, args.cache, sample_pods=args.sample,
                       lstm_epochs=args.epochs, seed=args.seed)
    print(rep["lightgbm"])


if __name__ == "__main__":
    main()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_train_capstone.py -v`
Expected: PASS.

- [ ] **Step 5: Run the full unit suite (no regressions)**

Run: `cd node-resource-forecaster && python3 -m pytest`
Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add training/train.py tests/test_train_capstone.py
git commit -m "feat(forecaster): capstone training pipeline (acquire->replay->train->eval)"
```

---

### Task 11: Server wiring + artifact-status logging

**Files:**
- Modify: `node-resource-forecaster/server/grpc_server.py` (servicer `__init__` around lines 330–368; `serve`/`main` around 1081–1128)
- Create: `node-resource-forecaster/server/artifact_status.py`
- Test: `node-resource-forecaster/tests/test_artifact_status.py`

**Interfaces:**
- Produces (`artifact_status.py`):
  - `TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]`
  - `RULE_HEADS = ["caching", "storage_tiering", "load_balancing"]`
  - `artifact_status(model_path: str | None, policy_dir: str | None) -> dict` → `{"lstm_loaded": bool, "lightgbm_trained": [..], "lightgbm_rule": [..], "summary": str}`. Pure (filesystem checks only) → unit-testable.
- Consumes (server): `models.forecaster_config.make_occupancy_forecaster_config` so the served LSTM matches the trained artifact's `input_dim`.

- [ ] **Step 1: Write the failing test**

`tests/test_artifact_status.py`:
```python
from server.artifact_status import artifact_status, TRAINED_HEADS, RULE_HEADS


def test_status_reports_trained_and_rule(tmp_path):
    (tmp_path / "node_health_model.txt").write_text("x")
    (tmp_path / "autoscale_model.txt").write_text("x")
    lstm = tmp_path / "lstm_attention.pt"
    lstm.write_text("x")
    st = artifact_status(str(lstm), str(tmp_path))
    assert st["lstm_loaded"] is True
    assert "node_health" in st["lightgbm_trained"]
    assert "autoscale" in st["lightgbm_trained"]
    # migration/provisioning artifacts absent here -> they fall to rule list
    assert "migration" in st["lightgbm_rule"]
    assert set(RULE_HEADS) <= set(st["lightgbm_rule"])
    assert "LSTM" in st["summary"]


def test_status_missing_lstm(tmp_path):
    st = artifact_status(str(tmp_path / "nope.pt"), str(tmp_path))
    assert st["lstm_loaded"] is False
    assert st["lightgbm_trained"] == []
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd node-resource-forecaster && python3 -m pytest tests/test_artifact_status.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'server.artifact_status'`.

- [ ] **Step 3: Write the implementation + wire the server**

`server/artifact_status.py`:
```python
"""Report which models are artifact-backed vs rule/threshold-backed (honesty)."""
import os

TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]
RULE_HEADS = ["caching", "storage_tiering", "load_balancing"]


def artifact_status(model_path, policy_dir) -> dict:
    lstm_loaded = bool(model_path) and os.path.exists(model_path)
    trained, rule = [], list(RULE_HEADS)
    for head in TRAINED_HEADS:
        p = os.path.join(policy_dir, f"{head}_model.txt") if policy_dir else ""
        if p and os.path.exists(p):
            trained.append(head)
        else:
            rule.append(head)
    summary = (f"loaded: LSTM-Attention={'ok' if lstm_loaded else 'MISSING->fallback'}, "
               f"LightGBM {len(trained)}/7 trained ({','.join(trained) or 'none'}); "
               f"rule-based: {','.join(rule)}")
    return {"lstm_loaded": lstm_loaded, "lightgbm_trained": trained,
            "lightgbm_rule": rule, "summary": summary}
```

In `server/grpc_server.py`, make three edits:

(a) Build the served LSTM from the shared occupancy config (so it matches the trained
artifact's `input_dim`). Replace the attention init block (around lines 333–337):
```python
        if HAS_LSTM_ATTENTION:
            from models.forecaster_config import make_occupancy_forecaster_config
            self.attention_config = make_occupancy_forecaster_config()
            self.attention_model, self.attention_trainer = create_lstm_attention_forecaster(self.attention_config)
            self.use_attention = True
            logger.info("Using LSTM-Attention model (allocation-occupancy config)")
        else:
            self.use_attention = False
```

(b) After the model-load block (after line 368), log the artifact status and store a flag:
```python
        from server.artifact_status import artifact_status
        self.artifacts = artifact_status(model_path, policy_model_dir)
        logger.info("[apollo-ml] %s", self.artifacts["summary"])
        self.model_ready = self.artifacts["lstm_loaded"]
```

(c) Default the artifact paths in `serve()` (around line 1081) so a baked image loads
without extra flags. Change the signature defaults:
```python
def serve(port: int = 50055, http_port: int = 8080,
          model_path: Optional[str] = None, policy_model_dir: Optional[str] = None):
    model_path = model_path or os.environ.get("MODEL_PATH", "/models/lstm_attention.pt")
    policy_model_dir = policy_model_dir or os.environ.get("POLICY_MODEL_DIR", "/models")
```
(Leave the CLI `--model`/`--policy-models` overrides intact; they still win when passed.)

- [ ] **Step 4: Run the status test + confirm server still imports**

Run:
```bash
cd node-resource-forecaster && python3 -m pytest tests/test_artifact_status.py -v && python3 -c "import ast; ast.parse(open('server/grpc_server.py').read()); print('grpc_server parses')"
```
Expected: tests PASS; prints `grpc_server parses`.

- [ ] **Step 5: Commit**

```bash
git add server/artifact_status.py server/grpc_server.py tests/test_artifact_status.py
git commit -m "feat(forecaster): load baked artifacts via shared config + honest status logging"
```

---

### Task 12: Multi-stage Dockerfile (train at build, bake artifacts) + entrypoint args

**Files:**
- Modify: `node-resource-forecaster/Dockerfile`
- Reference: confirm `MODEL_PATH`/volume in `node-resource-forecaster` deployment manifest (do not change cluster manifests here; override path is preserved).

**Interfaces:**
- Produces: a runtime image containing `/models/lstm_attention.pt` + 4 `*_model.txt`, loaded by `serve()` defaults from Task 11.

- [ ] **Step 1: Rewrite the Dockerfile as multi-stage**

`node-resource-forecaster/Dockerfile`:
```dockerfile
# ---------- Stage 1: train artifacts from the GPU-2023 trace ----------
FROM python:3.11-slim AS trainer
WORKDIR /build
RUN pip install --no-cache-dir torch>=2.0.0 --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir lightgbm>=4.0.0 pandas>=2.0.0 scikit-learn>=1.3.0 numpy>=1.24.0
COPY models/ models/
COPY training/ training/
# Deterministic, sampled build-time training (keeps image builds bounded).
RUN python -m training.train --out /artifacts --cache /trace-cache --sample 4000 --epochs 10 \
 && ls -la /artifacts

# ---------- Stage 2: runtime image ----------
FROM python:3.11-slim AS runtime
WORKDIR /app
RUN pip install --no-cache-dir torch>=2.0.0 --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir lightgbm>=4.0.0 numpy>=1.24.0
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
# Bake trained artifacts to the default load path.
COPY --from=trainer /artifacts/ /models/
ENV MODEL_PATH=/models/lstm_attention.pt
ENV POLICY_MODEL_DIR=/models
EXPOSE 50055 8080
CMD ["python", "-m", "server.grpc_server"]
```
> If the repo's existing Dockerfile uses a different base/entrypoint, preserve those
> specifics and only add: the `trainer` stage, the `COPY --from=trainer /artifacts/ /models/`,
> and the two `ENV` defaults. The `/models` volume mount in the deployment still overrides.

- [ ] **Step 2: Build the image (integration check)**

Run (on the build box with network):
```bash
cd node-resource-forecaster && docker build -t keti-node-resource-forecaster:ml-test .
```
Expected: stage 1 prints `/artifacts` listing with `lstm_attention.pt` + 4 `*_model.txt`; build succeeds.

- [ ] **Step 3: Smoke-run the container; confirm honest load log**

Run:
```bash
docker run --rm keti-node-resource-forecaster:ml-test python -c \
"from server.artifact_status import artifact_status; print(artifact_status('/models/lstm_attention.pt','/models')['summary'])"
```
Expected: prints `loaded: LSTM-Attention=ok, LightGBM 4/7 trained (node_health,autoscale,migration,provisioning); rule-based: caching,storage_tiering,load_balancing`.

- [ ] **Step 4: Commit**

```bash
git add Dockerfile
git commit -m "feat(forecaster): multi-stage build trains + bakes GPU-2023 artifacts"
```

---

### Task 13: Honesty documentation (rule heads + root CLAUDE.md)

**Files:**
- Modify: `node-resource-forecaster/models/multi_lightgbm.py` (rule-head docstrings only — comments, no logic change)
- Modify: `/root/workspace/CLAUDE.md` (the "Known Limitations / APOLLO" note)

**Interfaces:** none (documentation).

- [ ] **Step 1: Annotate the 3 rule heads (comment-only)**

In `models/multi_lightgbm.py`, add a one-line note to the docstrings of `_decide_caching`,
`_decide_load_balancing`, and `_decide_storage_tiering` (do NOT change logic):
```python
        """
        ... (existing description) ...

        NOTE: rule-based on forecast — NOT model-trained. Requires storage/IO
        telemetry (cache hit ratio, IOPS, throughput) that is a Year-3 metric;
        no honest training data exists yet, so this head stays rule-based.
        """
```

- [ ] **Step 2: Update the root CLAUDE.md APOLLO note**

In `/root/workspace/CLAUDE.md`, replace the bullet that begins
"**APOLLO forecaster is currently threshold-based, not trained ML.**" with:
```markdown
4. **APOLLO ML is now partially trained (GPU-2023 anchored).** The LSTM-Attention
   forecaster and 4 LightGBM heads (node_health, autoscale, migration, provisioning)
   are trained on the Alibaba GPU-2023 trace (allocation-occupancy signal) and baked into
   the forecaster image; artifacts load at runtime (`/models`, MODEL_PATH override). The
   forecaster predicts **allocation occupancy** (requests/capacity), not true utilization.
   The 3 storage/IO heads (caching, storage_tiering, load_balancing) remain **rule-based**
   (Year-3 metrics pending). **PPO scheduling policy is still untrained** (random weights;
   separate follow-up track). Eval metrics are held-out on the trace; production
   correctness is not claimed beyond the live divergence demo.
5. Forecaster historically read `cpu_requests/capacity` (allocation ratio); this is now the
   deliberate, train==serve occupancy signal. Live-usage telemetry is Year-3.
```

- [ ] **Step 3: Verify the docs changed**

Run:
```bash
grep -n "allocation-occupancy" /root/workspace/CLAUDE.md
grep -n "rule-based on forecast" node-resource-forecaster/models/multi_lightgbm.py
```
Expected: both grep commands return matches.

- [ ] **Step 4: Commit (two repos)**

```bash
# apollo repo
cd /root/workspace/apollo && git add node-resource-forecaster/models/multi_lightgbm.py \
 && git commit -m "docs(forecaster): mark caching/tiering/loadbalance heads rule-based (Year-3)"
# workspace root repo (note: root uses git user jja147; commit there separately)
cd /root/workspace && git add CLAUDE.md \
 && git commit -m "docs: APOLLO ML now partially trained (GPU-2023); honest scope"
```

---

### Task 14: Live verification on cluster 99 (user-driven) + E2E gate

**Files:**
- Modify (E2E gate): the orchestration E2E harness `02-scheduling` stage in
  `keti_ai_storage_orchestration/` (location per memory: build box
  `/root/workspace/keti_ai_storage_orchestration/`, cluster `/root/keti_ai_storage_orchestration/`).

**Interfaces:** operational; no unit test. Deploy steps run via the user's `!` (verification-week practice: commit first, deploy manually).

- [ ] **Step 1: Add a "trained-model active" gate to the E2E harness**

In the `02-scheduling` verify script, after the forecaster is up, assert the honest load
log is present:
```bash
# expect the forecaster to report trained artifacts, not fallback
kubectl logs -n keti -l app=node-resource-forecaster --tail=200 \
  | grep -q "LightGBM 4/7 trained" \
  && echo "PASS: forecaster serving trained models" \
  || { echo "FAIL: forecaster not serving trained models (fallback?)"; exit 1; }
```

- [ ] **Step 2: Build + push/import the image (user runs)**

On build box 80 (network OK), user runs via `!`:
```bash
cd node-resource-forecaster && docker build -t <registry>/keti-node-resource-forecaster:ml-<date> .
# push or ctr import per the component's existing deploy flow (see scripts/)
```

- [ ] **Step 3: Deploy to 99 + confirm honest load log (user runs)**

```bash
KUBECONFIG=~/.kube/config-99 kubectl rollout restart deploy/node-resource-forecaster -n keti
KUBECONFIG=~/.kube/config-99 kubectl logs -n keti -l app=node-resource-forecaster --tail=50 | grep "apollo-ml"
```
Expected: log line `loaded: LSTM-Attention=ok, LightGBM 4/7 trained (...); rule-based: caching,storage_tiering,load_balancing`.

- [ ] **Step 4: Demonstrate model-vs-threshold divergence**

Drive a forecast request (via the forecaster's HTTP/gRPC client used by the E2E harness)
on a trace-replayed load and show a case where the trained `node_health`/`migration`
decision differs from the pure-threshold baseline (rule engine), capturing both outputs.
Record the evidence in the E2E run log.

- [ ] **Step 5: Commit the E2E gate change**

```bash
cd /root/workspace/keti_ai_storage_orchestration && git add 02-scheduling/ \
 && git commit -m "test(e2e): gate forecaster on trained-model active log"
```
(If this package is not yet a tracked git repo, commit it within its tracking repo per the
E2E package's own convention.)

---

## Self-Review

**1. Spec coverage:**
- §3 scope (4 trained + 3 rule, PPO out, no synthetic) → Tasks 6/8 (4 heads), 13 (3 rule), constraints (PPO/synthetic out). ✓
- §4 GPU-2023 occupancy-consistent → Tasks 2/3 (replay/acquire), 5 (occupancy channels). ✓
- §5 components → Tasks 2–11 create exactly the spec's files (+ `models/forecaster_config.py` and `server/artifact_status.py`, refinements noted). ✓
- §6 data pipeline (replay: occupancy, queue, contention) → Task 2 (occupancy + queue). Contention events: folded into labels (Task 6 uses future occupancy bands); standalone contention array not separately needed → noted, no gap for the 4 heads. ✓
- §7 labels (4 heads forward-looking) → Task 6. ✓
- §8 feature contract single source → Task 5 + Task 8 reuse `_build_features`. ✓
- §9 training (LSTM, 4 LightGBM, eval) → Tasks 7/8/9/10. ✓
- §10 runtime bake + wiring → Tasks 11/12. ✓
- §11 honesty (3 rule heads, CLAUDE.md, naming) → Task 13. ✓
- §12 verification (unit, eval, live, E2E) → Tasks 2–11 unit, 9/10 eval, 14 live+E2E. ✓
- §13 risks / §14 out-of-scope → encoded in Global Constraints + Task 13 docs. ✓
- §15 definition of done → Tasks 10 (artifacts), 11/12 (bake+load), 9 (eval report), 2–11 (tests), 13 (CLAUDE.md), 14 (live). ✓

**2. Placeholder scan:** No "TBD"/"implement later"/"add error handling". Two upstream-detail
NOTEs (Task 3 CSV path, Task 12 base image) are explicit fallback instructions, not
placeholders — the tested behavior is unaffected.

**3. Type consistency:** `ReplayResult` fields used identically in Tasks 2/5/6/8/9.
`make_occupancy_forecaster_config` used in Tasks 4/7/11. `TRAINED_HEADS` defined in Task 8
and re-defined in Task 11's `artifact_status.py` (intentional — server must not import the
training package; the two lists are asserted identical by Task 11's test scope). `_build_features`
signature `(current_state, lstm_predictions, node_info)` matches Tasks 5/8/9 usage. LSTM
`train_epoch(batches)` batch format `(Tensor, {h: Tensor})` matches Task 7 `make_batches`.
Engine `predict(...).decisions[*].task_name/.decision` matches Task 9 usage.

No gaps found.
