# APOLLO PPO Initial Model — Analytic Uniform Init (design)

**Date:** 2026-06-25
**Component:** `apollo/scheduling-policy-engine` (Python, gRPC `:50054`)
**Status:** approved design, pre-implementation

## 1. Problem

The scheduling-policy-engine serves scheduler plugin weights from a PPO actor that is
**untrained and random at serve time**. Investigation (3-axis, 2026-06-25) confirmed:

- **Random weights actually reach the live scheduler.** The path is wired end-to-end:
  scheduler (`ai-storage-scheduler/internal/scheduler/schedule_one.go:252`) →
  gRPC `GetSchedulingPolicy` → PPO `get_action(deterministic=True)`
  (`server/grpc_server.py:534`) → `PluginWeights` (5 floats) →
  `framework.go:246/273` multiplies node scores by `int32(weight*10)`. The deployed
  scheduler overrides `APOLLO_ENDPOINT` to the **Python** server (`:50054`), so the PPO
  output is genuinely consumed (not the rule-based Go `apollo-policy-server`). Random
  weights are therefore *worse than no-op*.
- **`MODEL_PATH` is dead code.** `serve()` / `__main__` only read the `--model` argparse
  arg (default `None`); the `CMD` passes no `--model`. The Dockerfile and deployment set
  `ENV MODEL_PATH=/models/ppo_scheduler.pt` but **nothing reads it** → the live pod always
  runs random init. (Contrast: the forecaster does
  `model_path or os.environ.get("MODEL_PATH", ...)`.)
- **No artifact is ever baked.** The Dockerfile is single-stage with no train/COPY step.
- **GPU-2023 cannot train PPO.** The trace is scheduling-requests only (no latency /
  transfer / io-wait / placement / node identity), has no action labels, and the only
  trace-derivable reward (occupancy delta) is action-blind → no learning gradient. So an
  honest "initial model" cannot be RL-trained from the trace this round.

## 2. Goal & honest claim

Replace random weights with a **deterministic, uniform** policy and make the baked
artifact actually load. The honest claim — no RL or benchmark superiority is asserted:

> "PPO actor serves a **deterministic uniform** plugin-weight policy (`[0.2]×5`),
> replacing random initialization. Online RL is **disabled** pending a trustworthy reward
> signal. State-conditioned policy learning is a follow-up track."

This is strictly more sensible than random (random → equal weighting of all plugins) and
removes the "random worse than no-op" defect, with zero overclaiming.

## 3. Decisions (user-approved)

| Decision | Choice |
|---|---|
| Prior / target | Uniform constant `[0.2]×5` over the 5 plugins |
| Realization | **Analytic uniform init** (not SGD/BC training) |
| Online `trainer.update()` | **Freeze** (env-gated, default off) |
| Scope | Full path to **99 live deploy + verify** (forecaster `operational-v2` pattern) |
| Critic | Zero head → deterministic confidence (consistent with uniform) |
| Dormant `update()` bugs | Left unmodified (frozen path), documented only (YAGNI) |

The 5 plugins (`PluginWeights`, proto field 13):
`[data_locality_aware, storage_tier_aware, io_pattern_based, kueue_aware, pipeline_stage_aware]`,
matching PPO `num_plugins=5`.

## 4. Design

### 4.1 Analytic uniform init — why it is exactly uniform

`ActorNetwork`: `network` (2×Linear+ReLU) → `policy_head = nn.Linear(hidden, 5)` →
`F.softmax(logits)`. If `policy_head.weight = 0` and `policy_head.bias = 0`, then for
**any** input `logits = bias = [0,0,0,0,0]` → `softmax = [0.2,0.2,0.2,0.2,0.2]` exactly.
The encoders and fusion layers can keep their random init — they feed `hidden`, which is
zeroed out by `policy_head.weight=0`. Output is input-independent by construction, so no
training data and no train==serve feature-parity concern. Critic head similarly zeroed for
a deterministic (meaningless-but-stable) confidence value.

### 4.2 Components (all under `apollo/scheduling-policy-engine/`)

1. **`training/init_model.py`** (new): instantiate `SchedulingPolicyPPO(PPOConfig())`,
   zero `actor.policy_head` and `critic` head, **self-verify** (reload + forward over many
   random inputs, assert output == `[0.2]×5` within `1e-6`), then `PPOTrainer.save()` to
   the artifact path. CLI `--out` with default `/policy-models/ppo_scheduler.pt`.
2. **MODEL_PATH env fix** (`server/grpc_server.py` `serve()` + `__main__`):
   `model_path = model_path or os.environ.get("MODEL_PATH", "/policy-models/ppo_scheduler.pt")`.
   Without this, baking is inert.
3. **Freeze online RL** (`server/grpc_server.py`, around `store_transition`@672 /
   `update()`@686): wrap in an `APOLLO_PPO_ONLINE_RL` env gate (default off). Comment:
   "unsound clip math + sparse/untrusted reward → frozen until honest reward exists."
4. **Honest status line** at startup: load success/failure + `uniform-init`,
   `online-RL=frozen`, `critic=untrained (confidence not meaningful)`. On load failure,
   log a **loud** `FELL BACK TO RANDOM` warning (current fallback is too quiet to catch in
   deploy verification).

### 4.3 Build & artifact path

- **Multi-stage Dockerfile** (forecaster precedent): a `trainer` stage runs
  `init_model.py` → `/policy-models/ppo_scheduler.pt`; runtime stage `COPY --from=trainer`.
- **Path = `/policy-models/`, not `/models/`** — the deployment mounts an NFS PVC at
  `/models` which would shadow a baked file. `ENV MODEL_PATH=/policy-models/ppo_scheduler.pt`.

## 5. Error handling

- Load failure (missing file / shape mismatch) → keep existing warn+random fallback **but**
  surface it loudly in the status line so deploy verification catches a silent regression.
- Build-time config-drift guard: `init_model.py` reloads its own artifact into a fresh
  `SchedulingPolicyServicer`-compatible model and asserts uniform output → guarantees the
  baked artifact is load-compatible with the server's `PPOConfig` before it ships.

## 6. Testing (TDD)

1. Initialized model outputs **exactly** `[0.2]×5` (±tol) over many varied random inputs.
2. `save → load` round-trip preserves uniform output (serving-path equivalence).
3. `MODEL_PATH` env branch: set → loads; unset/missing → loud warn + random fallback.
4. Freeze gate: with `APOLLO_PPO_ONLINE_RL` unset, `update()` is not invoked on feedback.

## 7. Deploy & live verification (99)

- 80 build → DockerHub push (`ketidevit2/scheduling-policy-engine:operational-v2`-style
  tag, `imagePullPolicy: Always`) → 99 `rollout restart`. Preserve rollback tag.
- Confirm scheduler `APOLLO_ENDPOINT` stays `scheduling-policy-engine.apollo...:50054`.
- **Live evidence:** server log `Model loaded from /policy-models/...` + honest status line;
  scheduler log shows plugin weights all `0.2` (uniform, **not** random); a test pod
  schedules successfully.

## 8. Out of scope (follow-up)

- State-conditioned / RL-trained policy (needs a placement simulator or real reward loop).
- Fixing the dormant `update()` averaging bug and the unsound PPO clip math (only relevant
  once online RL is re-enabled).
- Per-node placement replay, pipeline/io/locality state features.

## 9. Effort

~2–3 engineer-days through 99 live deploy + verification.
