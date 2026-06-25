# APOLLO PPO Uniform-Init Initial Model — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the PPO scheduling actor's random weights with a deterministic, analytically-constructed uniform policy (`[0.2]×5`), make the baked artifact actually load at runtime, freeze online RL, and deploy + verify on cluster 99.

**Architecture:** Add an offline `training/init_model.py` that instantiates the existing `SchedulingPolicyPPO`, zeros the actor's `policy_head` (and the critic head) so `softmax(logits)` is exactly uniform for any input, and saves it with `PPOTrainer.save()`. Fix the dead `MODEL_PATH` env read so the server loads the baked artifact, gate the online `trainer.update()` loop behind an env flag (default off), and add an honest startup status line. Bake the artifact into a multi-stage Docker image (forecaster `operational-v2` precedent) at `/policy-models/` to avoid the `/models` PVC shadow.

**Tech Stack:** Python 3.11, PyTorch (CPU-only), gRPC, pytest, Docker (multi-stage), Kubernetes.

## Global Constraints

- All paths below are under `apollo/scheduling-policy-engine/` unless stated; the apollo module root is `/root/workspace/apollo`.
- Commits use author `evergyeol95 <evergyeol95@users.noreply.github.com>` with **no Co-Authored-By / Claude trailer** (apollo repo convention). Set via `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env on each commit.
- The 5 plugins are exactly `[data_locality_aware, storage_tier_aware, io_pattern_based, kueue_aware, pipeline_stage_aware]`; `PPOConfig.num_plugins == 5`. Uniform target = `1/5 = 0.2` each.
- Bake/load artifact path is `/policy-models/ppo_scheduler.pt` (NOT `/models/...`, which the deployment PVC shadows).
- Online-RL env flag is `APOLLO_PPO_ONLINE_RL` (truthy = `1/true/yes/on`, case-insensitive); default/unset = frozen.
- Honest claim only: "deterministic uniform policy, random removed, online RL frozen, state-conditioned policy is follow-up." No RL/benchmark superiority claims anywhere (code comments, logs, docs).
- Test env must have `torch` (CPU) + `pytest` installed (same as the forecaster test env): `pip install torch --index-url https://download.pytorch.org/whl/cpu pytest numpy`.
- Spec: `apollo/docs/superpowers/specs/2026-06-25-apollo-ppo-uniform-init-design.md`.

---

### Task 1: Pure config/status helpers + test harness

Extract the three pure decisions (model-path resolution, online-RL gate, status string) into a dependency-light module so they are unit-testable without importing `grpc_server.py` (which pulls generated protos + torch). Also establishes the pytest harness for all later tasks.

**Files:**
- Create: `scheduling-policy-engine/pytest.ini`
- Create: `scheduling-policy-engine/tests/conftest.py`
- Create: `scheduling-policy-engine/server/policy_status.py`
- Test: `scheduling-policy-engine/tests/test_policy_status.py`

**Interfaces:**
- Produces:
  - `resolve_model_path(arg_path: Optional[str] = None, env: Optional[Mapping] = None) -> str`
  - `online_rl_enabled(env: Optional[Mapping] = None) -> bool`
  - `policy_status(model_loaded: bool, online_rl: bool) -> str`
  - module constant `DEFAULT_MODEL_PATH = "/policy-models/ppo_scheduler.pt"`

- [ ] **Step 1: Create the pytest harness files**

Create `scheduling-policy-engine/pytest.ini`:

```ini
[pytest]
testpaths = tests
python_files = test_*.py
addopts = -q
```

Create `scheduling-policy-engine/tests/conftest.py`:

```python
import os
import sys

# Make the component root importable so tests can `import models`, `import server`, `import training`.
COMPONENT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if COMPONENT_ROOT not in sys.path:
    sys.path.insert(0, COMPONENT_ROOT)
```

- [ ] **Step 2: Write the failing test**

Create `scheduling-policy-engine/tests/test_policy_status.py`:

```python
from server.policy_status import (
    resolve_model_path,
    online_rl_enabled,
    policy_status,
    DEFAULT_MODEL_PATH,
)


def test_resolve_model_path_prefers_arg():
    assert resolve_model_path("/a/b.pt", {"MODEL_PATH": "/env.pt"}) == "/a/b.pt"


def test_resolve_model_path_falls_back_to_env():
    assert resolve_model_path(None, {"MODEL_PATH": "/env.pt"}) == "/env.pt"


def test_resolve_model_path_default_when_unset():
    assert resolve_model_path(None, {}) == DEFAULT_MODEL_PATH
    assert DEFAULT_MODEL_PATH == "/policy-models/ppo_scheduler.pt"


def test_online_rl_disabled_by_default():
    assert online_rl_enabled({}) is False
    assert online_rl_enabled({"APOLLO_PPO_ONLINE_RL": ""}) is False
    assert online_rl_enabled({"APOLLO_PPO_ONLINE_RL": "false"}) is False


def test_online_rl_enabled_for_truthy():
    for v in ("1", "true", "TRUE", "yes", "On"):
        assert online_rl_enabled({"APOLLO_PPO_ONLINE_RL": v}) is True


def test_policy_status_loaded_is_honest_and_frozen():
    s = policy_status(model_loaded=True, online_rl=False)
    assert "uniform-init" in s
    assert "online-RL=frozen" in s
    assert "FELL BACK TO RANDOM" not in s


def test_policy_status_not_loaded_is_loud():
    s = policy_status(model_loaded=False, online_rl=False)
    assert "FELL BACK TO RANDOM" in s
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m pytest tests/test_policy_status.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'server.policy_status'`

- [ ] **Step 4: Write minimal implementation**

Create `scheduling-policy-engine/server/policy_status.py`:

```python
"""Pure helpers for the scheduling-policy-engine model lifecycle.

Kept dependency-light (stdlib only) so they are unit-testable without importing
the gRPC server (which pulls generated protos + torch).
"""
import os
from typing import Mapping, Optional

DEFAULT_MODEL_PATH = "/policy-models/ppo_scheduler.pt"
ONLINE_RL_ENV = "APOLLO_PPO_ONLINE_RL"
_TRUTHY = ("1", "true", "yes", "on")


def resolve_model_path(arg_path: Optional[str] = None,
                       env: Optional[Mapping] = None) -> str:
    """CLI arg wins; else MODEL_PATH env; else the baked default."""
    env = os.environ if env is None else env
    return arg_path or env.get("MODEL_PATH", DEFAULT_MODEL_PATH)


def online_rl_enabled(env: Optional[Mapping] = None) -> bool:
    """Online PPO updates are FROZEN unless APOLLO_PPO_ONLINE_RL is truthy."""
    env = os.environ if env is None else env
    return env.get(ONLINE_RL_ENV, "").strip().lower() in _TRUTHY


def policy_status(model_loaded: bool, online_rl: bool) -> str:
    """One honest startup line; no RL/benchmark superiority is claimed."""
    if model_loaded:
        head = "PPO actor=uniform-init (deterministic [0.2]x5)"
    else:
        head = "PPO actor=!!FELL BACK TO RANDOM (model missing/load failed)!!"
    rl = "online-RL=ON" if online_rl else "online-RL=frozen"
    return f"[apollo-ml] {head}; {rl}; critic=untrained (confidence not meaningful)"
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m pytest tests/test_policy_status.py -v`
Expected: PASS (7 passed)

- [ ] **Step 6: Commit**

```bash
cd /root/workspace/apollo
git add scheduling-policy-engine/pytest.ini scheduling-policy-engine/tests/conftest.py \
        scheduling-policy-engine/server/policy_status.py \
        scheduling-policy-engine/tests/test_policy_status.py
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "feat(scheduling-policy): pure model-path/online-RL/status helpers + test harness"
```

---

### Task 2: Analytic uniform-init model builder + artifact

Build the deterministic uniform model and save it in the exact format `PPOTrainer.load` expects. Harden `PPOTrainer.load` so it loads regardless of the installed torch's `weights_only` default (the checkpoint pickles a `PPOConfig`).

**Files:**
- Create: `scheduling-policy-engine/training/__init__.py`
- Create: `scheduling-policy-engine/training/init_model.py`
- Modify: `scheduling-policy-engine/models/ppo.py:549-553` (`PPOTrainer.load`)
- Test: `scheduling-policy-engine/tests/test_init_model.py`

**Interfaces:**
- Consumes: `models.ppo.SchedulingPolicyPPO`, `PPOConfig`, `PPOTrainer`.
- Produces:
  - `build_uniform_model(config: PPOConfig = None) -> SchedulingPolicyPPO`
  - `random_state(batch: int = 1, gen: torch.Generator = None) -> tuple[dict, dict, dict]` (workload, pipeline, cluster tensor dicts valid for `SchedulingPolicyPPO.forward`)
  - `verify_uniform(model, num_samples: int = 64, tol: float = 1e-5) -> bool`
  - `save_uniform_model(out_path: str, config: PPOConfig = None) -> str`
  - CLI: `python -m training.init_model --out <path> --seed <int>`

- [ ] **Step 1: Write the failing test**

Create `scheduling-policy-engine/tests/test_init_model.py`:

```python
import torch

from models.ppo import SchedulingPolicyPPO, PPOConfig, PPOTrainer
from training.init_model import (
    build_uniform_model,
    random_state,
    verify_uniform,
    save_uniform_model,
)


def _max_dev_from_uniform(policy: torch.Tensor) -> float:
    expected = torch.full_like(policy, 1.0 / policy.shape[-1])
    return (policy - expected).abs().max().item()


def test_forward_is_exactly_uniform_for_varied_inputs():
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(7)
    for _ in range(32):
        w, p, c = random_state(batch=4, gen=gen)
        with torch.no_grad():
            policy, value = model.forward(w, p, c)
        assert policy.shape[-1] == 5
        assert _max_dev_from_uniform(policy) < 1e-5


def test_get_action_deterministic_is_uniform():
    # This is the exact call the server makes (deterministic=True).
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(11)
    w, p, c = random_state(batch=2, gen=gen)
    with torch.no_grad():
        action, log_prob, value = model.get_action(w, p, c, deterministic=True)
    assert _max_dev_from_uniform(action) < 1e-5


def test_critic_value_is_zero():
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(3)
    w, p, c = random_state(batch=4, gen=gen)
    with torch.no_grad():
        _, value = model.forward(w, p, c)
    assert value.abs().max().item() < 1e-6


def test_save_load_roundtrip_preserves_uniform(tmp_path):
    out = str(tmp_path / "ppo_scheduler.pt")
    save_uniform_model(out)

    # Reload exactly the way the server does: PPOTrainer.load into a fresh model.
    fresh = SchedulingPolicyPPO(PPOConfig())
    trainer = PPOTrainer(fresh, PPOConfig())
    trainer.load(out)
    fresh.eval()

    gen = torch.Generator().manual_seed(99)
    w, p, c = random_state(batch=4, gen=gen)
    with torch.no_grad():
        policy, _ = fresh.forward(w, p, c)
    assert _max_dev_from_uniform(policy) < 1e-5


def test_verify_uniform_returns_true():
    assert verify_uniform(build_uniform_model()) is True
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m pytest tests/test_init_model.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'training.init_model'`

- [ ] **Step 3: Create the training package + init_model implementation**

Create `scheduling-policy-engine/training/__init__.py` (empty file):

```python
```

Create `scheduling-policy-engine/training/init_model.py`:

```python
"""Analytic uniform initialization for the PPO scheduling actor.

Produces a DETERMINISTIC policy whose output is uniform plugin weights
([0.2]*5) for ANY input, by zeroing the actor's policy head (and the critic
head). The uniform output is exact by construction:

    logits = policy_head.weight @ hidden + policy_head.bias
    with weight=0 and bias=0  ->  logits = 0  ->  softmax(logits) = uniform

This replaces random-initialized weights (which are worse than no-op when fed
to the live scheduler). It is NOT reinforcement learning and claims no
performance superiority. State-conditioned policy learning is a follow-up.
"""
import argparse
import os
from typing import Optional, Tuple

import torch

from models.ppo import SchedulingPolicyPPO, PPOConfig, PPOTrainer

# ClusterEncoder default capacity (SchedulingPolicyPPO -> ClusterEncoder(max_nodes=32)).
_MAX_NODES = 32


def build_uniform_model(config: Optional[PPOConfig] = None) -> SchedulingPolicyPPO:
    """Instantiate the PPO model and force exactly-uniform, deterministic output."""
    config = config or PPOConfig()
    model = SchedulingPolicyPPO(config)
    with torch.no_grad():
        # Actor: zeroing the final head makes logits input-independent (==bias==0)
        # -> softmax is uniform for every input.
        torch.nn.init.zeros_(model.actor.policy_head.weight)
        torch.nn.init.zeros_(model.actor.policy_head.bias)
        # Critic: zero the final layer -> deterministic value 0 (confidence is
        # not meaningful; flagged in the server status line).
        torch.nn.init.zeros_(model.critic.network[-1].weight)
        torch.nn.init.zeros_(model.critic.network[-1].bias)
    return model


def random_state(batch: int = 1,
                 gen: Optional[torch.Generator] = None) -> Tuple[dict, dict, dict]:
    """Build a random but structurally-valid (workload, pipeline, cluster) input
    triple matching the encoders' expected tensor shapes/dtypes."""
    def r(*shape):
        return torch.rand(*shape, generator=gen)

    workload = {
        "workload_type": torch.randint(0, 10, (batch,), generator=gen),
        "preprocessing_type": torch.randint(0, 10, (batch,), generator=gen),
        "io_pattern": r(batch, 7),
        "compute_profile": r(batch, 6),
        "flags": r(batch, 3),
    }
    pipeline = {
        "position": r(batch, 3),
        "dag_structure": r(batch, 4),
        "locality": r(batch, 2),
    }
    cluster = {
        "nodes": r(batch, _MAX_NODES, 10),
        "node_mask": torch.ones(batch, _MAX_NODES, dtype=torch.bool),
        "cluster_metrics": r(batch, 8),
        "queue_status": r(batch, 4),
    }
    return workload, pipeline, cluster


def verify_uniform(model: SchedulingPolicyPPO,
                   num_samples: int = 64,
                   tol: float = 1e-5) -> bool:
    """Assert the model outputs uniform weights across many varied inputs."""
    model.eval()
    gen = torch.Generator().manual_seed(0)
    n_plugins = model.config.num_plugins
    for _ in range(num_samples):
        w, p, c = random_state(batch=4, gen=gen)
        with torch.no_grad():
            policy, _ = model.forward(w, p, c)
        expected = torch.full_like(policy, 1.0 / n_plugins)
        max_dev = (policy - expected).abs().max().item()
        if max_dev > tol:
            raise AssertionError(f"non-uniform output: max deviation {max_dev} > {tol}")
    return True


def save_uniform_model(out_path: str,
                       config: Optional[PPOConfig] = None) -> str:
    """Build, self-verify, save (PPOTrainer format), then reload-verify."""
    config = config or PPOConfig()
    model = build_uniform_model(config)
    verify_uniform(model)

    out_dir = os.path.dirname(out_path)
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    PPOTrainer(model, config).save(out_path)

    # Build-time guard: the artifact must load into a fresh, server-shaped model
    # and still be uniform -> guarantees load-compatibility with PPOConfig.
    fresh = SchedulingPolicyPPO(config)
    PPOTrainer(fresh, config).load(out_path)
    verify_uniform(fresh)
    return out_path


def main() -> None:
    parser = argparse.ArgumentParser(description="Build the PPO uniform-init artifact.")
    parser.add_argument("--out", default="/policy-models/ppo_scheduler.pt")
    parser.add_argument("--seed", type=int, default=0)
    args = parser.parse_args()
    torch.manual_seed(args.seed)
    path = save_uniform_model(args.out)
    print(f"[apollo-ml] PPO uniform-init model written to {path}")


if __name__ == "__main__":
    main()
```

- [ ] **Step 4: Harden `PPOTrainer.load` for the installed torch's weights_only default**

The checkpoint written by `PPOTrainer.save` pickles a `PPOConfig` object; torch >= 2.6 defaults `torch.load(weights_only=True)`, which rejects it. The artifact is trusted/local, so load with `weights_only=False`.

In `scheduling-policy-engine/models/ppo.py`, change `PPOTrainer.load` (around lines 549-553) from:

```python
    def load(self, path: str):
        """모델 로드"""
        checkpoint = torch.load(path)
        self.model.load_state_dict(checkpoint['model_state_dict'])
        self.optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
```

to:

```python
    def load(self, path: str):
        """모델 로드 (trusted/local artifact -> weights_only=False for torch>=2.6)."""
        checkpoint = torch.load(path, weights_only=False)
        self.model.load_state_dict(checkpoint['model_state_dict'])
        self.optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m pytest tests/test_init_model.py -v`
Expected: PASS (5 passed)

- [ ] **Step 6: Smoke-run the CLI to a temp path**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m training.init_model --out /tmp/ppo_smoke.pt && python -c "import os; assert os.path.getsize('/tmp/ppo_smoke.pt') > 0; print('artifact ok')"`
Expected: prints the write line and `artifact ok`

- [ ] **Step 7: Commit**

```bash
cd /root/workspace/apollo
git add scheduling-policy-engine/training/__init__.py scheduling-policy-engine/training/init_model.py \
        scheduling-policy-engine/models/ppo.py scheduling-policy-engine/tests/test_init_model.py
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "feat(scheduling-policy): analytic uniform-init PPO model builder + artifact"
```

---

### Task 3: Wire the server (load env, freeze online RL, honest status)

Make the server read `MODEL_PATH`, track load success with a loud fallback, log the honest status line, and gate the online `trainer.update()` loop behind `APOLLO_PPO_ONLINE_RL` (default frozen).

**Files:**
- Modify: `scheduling-policy-engine/server/grpc_server.py` (`__init__` ~440-467; feedback block ~671-689; `serve()` ~1388-1392)

**Interfaces:**
- Consumes: `server.policy_status.{resolve_model_path, online_rl_enabled, policy_status}` (Task 1).
- Produces: `SchedulingPolicyServicer.model_loaded: bool`, `SchedulingPolicyServicer.online_rl: bool`.

**Note on testing:** `grpc_server.py` imports generated protos (`apollo_pb2`) that exist only inside the built image, so it cannot be imported in a host unit test. Its logic is covered by the Task 1 helper tests; behavioral verification happens in Task 4 (docker run shows the load + status log) and Task 5 (live uniform weights). The check below is a syntax/compile gate.

- [ ] **Step 1: Add the import near the other model imports**

In `scheduling-policy-engine/server/grpc_server.py`, immediately after line 47 (`from models.ppo import ...`), add:

```python
from server.policy_status import resolve_model_path, online_rl_enabled, policy_status
```

- [ ] **Step 2: Replace the model-load branch in `__init__`**

Replace the existing block (currently lines ~461-467):

```python
        # 모델 로드
        if model_path and os.path.exists(model_path):
            try:
                self.trainer.load(model_path)
                logger.info(f"Model loaded from {model_path}")
            except Exception as e:
                logger.warning(f"Failed to load model: {e}, using random initialization")
```

with:

```python
        # 모델 로드 (실패/부재 시 loud 폴백 — 배포 검증에서 즉시 포착)
        self.model_loaded = False
        if model_path and os.path.exists(model_path):
            try:
                self.trainer.load(model_path)
                self.model_loaded = True
                logger.info(f"Model loaded from {model_path}")
            except Exception as e:
                logger.error(f"Failed to load model from {model_path}: {e}")
        else:
            logger.error(f"Model path not found: {model_path}")

        # 온라인 RL은 기본 freeze (부실 clip 수식 + 희소/미신뢰 reward).
        self.online_rl = online_rl_enabled()
        status = policy_status(self.model_loaded, self.online_rl)
        (logger.info if self.model_loaded else logger.warning)(status)
```

- [ ] **Step 3: Gate the online-RL feedback block**

Replace the experience-store + periodic-update block (currently lines ~671-689):

```python
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
```

with:

```python
                # 온라인 RL: APOLLO_PPO_ONLINE_RL로 게이트, 기본 freeze.
                # 사유: clip 수식 부실 + 희소/미신뢰 reward → 정직 reward 확보 전까지 동결.
                if self.online_rl:
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
                    if self.training_episodes % 32 == 0:
                        metrics = self.trainer.update()
                        logger.info(f"[PolicyEngine] Training update: {metrics}")
                    logger.info(f"[PolicyEngine] Feedback processed: reward={reward:.4f}")
                else:
                    logger.debug(
                        f"[PolicyEngine] Feedback received (online-RL frozen): reward={reward:.4f}"
                    )
```

- [ ] **Step 4: Make `serve()` resolve `MODEL_PATH`**

Replace the start of `serve()` (currently lines ~1388-1392):

```python
def serve(port: int = 50054, model_path: Optional[str] = None):
    """gRPC 서버 시작"""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    servicer = SchedulingPolicyServicer(model_path)
```

with:

```python
def serve(port: int = 50054, model_path: Optional[str] = None):
    """gRPC 서버 시작"""
    model_path = resolve_model_path(model_path)
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    servicer = SchedulingPolicyServicer(model_path)
```

- [ ] **Step 5: Compile-check (no protos needed)**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && python -m py_compile server/grpc_server.py && echo "compile ok"`
Expected: `compile ok`

- [ ] **Step 6: Verify the wiring grep-assertions**

Run:
```bash
cd /root/workspace/apollo/scheduling-policy-engine && \
grep -q "model_path = resolve_model_path(model_path)" server/grpc_server.py && \
grep -q "if self.online_rl:" server/grpc_server.py && \
grep -q "self.model_loaded = True" server/grpc_server.py && \
echo "wiring ok"
```
Expected: `wiring ok`

- [ ] **Step 7: Commit**

```bash
cd /root/workspace/apollo
git add scheduling-policy-engine/server/grpc_server.py
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "feat(scheduling-policy): load MODEL_PATH artifact, freeze online RL, honest status line"
```

---

### Task 4: Multi-stage Dockerfile bake + container verification

Bake the uniform-init artifact into the image at `/policy-models/ppo_scheduler.pt` and point `MODEL_PATH` there. Verify by building and running the container and observing the honest load log.

**Files:**
- Modify: `scheduling-policy-engine/Dockerfile`

**Interfaces:**
- Consumes: `training/init_model.py` (Task 2) for the trainer stage.

- [ ] **Step 1: Rewrite the Dockerfile as multi-stage**

Replace the entire contents of `scheduling-policy-engine/Dockerfile` with:

```dockerfile
# Scheduling Policy Engine Dockerfile
# Multi-stage: build the analytic uniform-init PPO artifact, then runtime.
# PPO 정책 엔진 (CPU-only). 초기모델 = 결정적 균등 정책(랜덤 제거), 온라인 RL freeze.

# ---------- Stage 1: build the uniform-init artifact ----------
FROM python:3.11-slim AS trainer
WORKDIR /build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
RUN pip install --no-cache-dir "numpy>=1.24.0"
COPY models/ ./models/
COPY training/ ./training/
RUN python -m training.init_model --out /artifacts/ppo_scheduler.pt --seed 0 \
 && ls -la /artifacts

# ---------- Stage 2: runtime ----------
FROM python:3.11-slim
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY proto/ ./proto/
RUN python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/apollo.proto \
    && python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/forecaster.proto
COPY models/ ./models/
COPY server/ ./server/
COPY training/ ./training/
RUN mkdir -p /models /policy-models
# Bake the artifact to the path the server loads from (NOT /models — PVC shadows it).
COPY --from=trainer /artifacts/ppo_scheduler.pt /policy-models/ppo_scheduler.pt
ENV PYTHONUNBUFFERED=1
ENV GRPC_PORT=50054
ENV MODEL_PATH=/policy-models/ppo_scheduler.pt
ENV LOG_LEVEL=INFO
EXPOSE 50054
EXPOSE 9090
CMD ["python", "server/grpc_server.py", "--port", "50054"]
```

- [ ] **Step 2: Build the image (on the build host, 80)**

Run: `cd /root/workspace/apollo/scheduling-policy-engine && docker build -t ketidevit2/scheduling-policy-engine:operational-v2 .`
Expected: build succeeds; the trainer stage prints `[apollo-ml] PPO uniform-init model written to /artifacts/ppo_scheduler.pt` and `ls -la /artifacts` shows a non-empty `ppo_scheduler.pt`.

- [ ] **Step 3: Verify the artifact is baked**

Run: `docker run --rm ketidevit2/scheduling-policy-engine:operational-v2 sh -c "ls -la /policy-models/ppo_scheduler.pt"`
Expected: lists a non-empty `/policy-models/ppo_scheduler.pt`.

- [ ] **Step 4: Verify the server loads it (honest log)**

Run: `docker run --rm ketidevit2/scheduling-policy-engine:operational-v2 sh -c "timeout 20 python server/grpc_server.py --port 50054 2>&1 | head -40"`
Expected output contains both:
- `Model loaded from /policy-models/ppo_scheduler.pt`
- `[apollo-ml] PPO actor=uniform-init (deterministic [0.2]x5); online-RL=frozen; critic=untrained ...`
And does NOT contain `FELL BACK TO RANDOM`.

- [ ] **Step 5: Commit**

```bash
cd /root/workspace/apollo
git add scheduling-policy-engine/Dockerfile
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "build(scheduling-policy): multi-stage bake of uniform-init artifact to /policy-models"
```

---

### Task 5: Deployment manifest + 99 live deploy + verification

Point the deployment at the baked path and a pullable image, deploy to cluster 99, and verify end-to-end that the scheduler now receives uniform (non-random) weights.

**Files:**
- Modify: `apollo/deployments/scheduling-policy-engine.yaml`

**Note:** Privileged/live steps (DockerHub push or `ctr` import, `kubectl` against 99, possibly a GitOps commit if the app is ArgoCD-managed with selfHeal) are run by the user via `!`, mirroring the forecaster `operational-v2` playbook in memory `[[apollo-ml-training-gpu2023-plan]]`. Preserve a rollback tag before overwriting.

- [ ] **Step 1: Update the deployment manifest**

In `apollo/deployments/scheduling-policy-engine.yaml`:

Change the image + pull policy from:

```yaml
        image: docker.io/library/scheduling-policy-engine:latest
        imagePullPolicy: Never
```

to:

```yaml
        image: ketidevit2/scheduling-policy-engine:operational-v2
        imagePullPolicy: Always
```

Change the `MODEL_PATH` env value from:

```yaml
        - name: MODEL_PATH
          value: "/models/ppo_scheduler.pt"
```

to:

```yaml
        - name: MODEL_PATH
          value: "/policy-models/ppo_scheduler.pt"
```

(Leave the `/models` PVC volume mount as-is; the baked artifact lives at `/policy-models`, which the PVC does not shadow.)

- [ ] **Step 2: Commit the manifest**

```bash
cd /root/workspace/apollo
git add deployments/scheduling-policy-engine.yaml
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "deploy(scheduling-policy): operational-v2 image + /policy-models MODEL_PATH"
```

- [ ] **Step 3: Preserve rollback tag + push image (user via `!`)**

Tag the currently-live image as a rollback before publishing the new one, then push. Example (adapt to the live registry/tag actually in use on 99):

```bash
# Save rollback of whatever is currently deployed, then push the new image.
docker tag ketidevit2/scheduling-policy-engine:operational-v2 ketidevit2/scheduling-policy-engine:pre-uniform-init-rollback || true
docker push ketidevit2/scheduling-policy-engine:operational-v2
```

Expected: push succeeds (digest printed). Record the digest.

- [ ] **Step 4: Roll out on 99 (user via `!`)**

If the deployment is plain `kubectl`-managed:

```bash
kubectl -n apollo apply -f deployments/scheduling-policy-engine.yaml
kubectl -n apollo rollout restart deployment/scheduling-policy-engine
kubectl -n apollo rollout status deployment/scheduling-policy-engine
```

If the `apollo` deployment is ArgoCD-managed with selfHeal (as the forecaster was), instead commit/push the manifest to the gitops source and let selfHeal sync, then `kubectl -n argocd annotate ... refresh` (see `[[apollo-ml-training-gpu2023-plan]]`).

Expected: a new pod reaches Ready with image digest == the pushed digest, `RESTARTS=0`.

- [ ] **Step 5: Verify server load log on 99**

Run: `kubectl -n apollo logs deploy/scheduling-policy-engine | grep -E "Model loaded|apollo-ml|FELL BACK"`
Expected: `Model loaded from /policy-models/ppo_scheduler.pt` + the `uniform-init ... online-RL=frozen` status line; NO `FELL BACK TO RANDOM`.

- [ ] **Step 6: Verify the scheduler receives uniform weights (end-to-end)**

Create a test pod that uses the custom scheduler, then check the scheduler logs for the APOLLO plugin weights it received.

```bash
kubectl -n keti logs -l app=ai-storage-scheduler | grep -iE "APOLLO|plugin weight" | tail -20
```

Expected: the logged plugin weights are all `0.2` (uniform), NOT random/varying values. (If the scheduler quantizes to `int32(w*10)`, all five plugins map to the same integer weight `2`.)

- [ ] **Step 7: Final commit (if any verification-driven manifest tweaks were needed)**

```bash
cd /root/workspace/apollo
git add -A deployments/scheduling-policy-engine.yaml
GIT_AUTHOR_NAME=evergyeol95 GIT_AUTHOR_EMAIL=evergyeol95@users.noreply.github.com \
GIT_COMMITTER_NAME=evergyeol95 GIT_COMMITTER_EMAIL=evergyeol95@users.noreply.github.com \
git commit -m "deploy(scheduling-policy): finalize operational-v2 rollout" || echo "no changes to commit"
```

---

## Self-Review

**Spec coverage:**
- §3 uniform `[0.2]×5` + analytic init → Task 2 (`build_uniform_model`, tests). ✓
- §4.2 (1) init script → Task 2. ✓
- §4.2 (2) MODEL_PATH env fix → Task 3 Step 4 + Task 1 `resolve_model_path`. ✓
- §4.2 (3) freeze online RL → Task 3 Step 3 + Task 1 `online_rl_enabled`. ✓
- §4.2 (4) honest status line + loud fallback → Task 1 `policy_status` + Task 3 Step 2. ✓
- §4.3 multi-stage bake to `/policy-models` → Task 4. ✓
- §5 error handling (loud fallback, build-time guard) → Task 3 Step 2 + Task 2 `save_uniform_model` reload-verify. ✓
- §6 tests 1-4 → Task 2 (uniform, round-trip) + Task 1 (env branch, freeze gate). ✓
- §7 deploy + live verify → Task 5. ✓
- §3 critic zeroed → Task 2 `build_uniform_model` + `test_critic_value_is_zero`. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full content; commands have expected output. ✓

**Type consistency:** `resolve_model_path`/`online_rl_enabled`/`policy_status` signatures match between Task 1 (definition) and Task 3 (use). `random_state`/`build_uniform_model`/`verify_uniform`/`save_uniform_model` consistent between Task 2 definition and its tests. Artifact path `/policy-models/ppo_scheduler.pt` consistent across Tasks 2-5. Attribute paths `model.actor.policy_head` and `model.critic.network[-1]` match `models/ppo.py`. ✓

**Known caveat (honest):** Task 3 has no host-level unit test (proto import wall); covered by Task 1 helper tests + Task 4 container log + Task 5 live verification — stated in the task.
