# APOLLO Real ML Training & Loading (GPU-2023 anchored) — Design

- **Date**: 2026-06-24
- **Component**: `apollo/` — `node-resource-forecaster` (LSTM-Attention) + Orchestration Policy Engine (Multi-LightGBM)
- **Status**: Design / spec (pre-implementation)
- **Track**: Code-quality recommendation #3 — "ML honesty / real training-loading"

## 1. Goal

Make APOLLO's 2-Stage ML genuinely **trained and loaded at runtime**, replacing the
current state where models are never trained, no artifacts exist, and serving silently
falls through to thresholds / random output. The result must be **honest**: real models
where real data supports them, transparent rules where it does not, and no fabricated
"intelligence."

Anchored on the project's own blueprint (2nd-year workshop slides 9–11): 2-Stage ML
(LSTM-Attention forecaster → Multi-LightGBM policy), real trace data, outcome-derived
labels, and a clear separation of Year-3 (storage/IO) metrics.

## 2. Background — current state (verified by code read)

- **LightGBM** (`node-resource-forecaster/models/multi_lightgbm.py`): `train_models()`
  (lgb.train) and `_load_models()` (loads `{task}_model.txt`) both **exist** but
  `self.models` starts `{}` (line 119) and `_load_models` returns early when no model dir
  (175–177). Every `_decide_*` falls through to **hardcoded thresholds**. No artifacts; no
  caller ever trains.
- **LSTM-Attention** (`models/lstm_attention.py`): real PyTorch model + `LSTMAttentionTrainer`
  (`train_epoch`, `save`/`load`) **exist**. Server (`server/grpc_server.py:333-368`)
  instantiates it and tries `attention_trainer.load(model_path)` but the artifact never
  exists → random weights → simulated forecasts with `np.random` drift.
- **Wiring**: `serve(model_path, policy_model_dir)` exists and is fed from CLI
  `--model`/`--policy-models` (grpc_server.py:1081-1128). Deployment sets `MODEL_PATH` env
  but the entrypoint path→args wiring is incomplete; `/models` PVC is mounted but empty.
- **Features**: 25-feature LightGBM contract (multi_lightgbm.py:133-165); 16 of 25 are
  forecaster predictions at 4 horizons. Runtime feeds `cpu_requests/capacity` (allocation
  ratio, not live usage) and hardcodes several features.
- **PPO** (`scheduling-policy-engine`): untrained random weights flow to the scheduler.
  **Explicitly OUT OF SCOPE** this round (see §10).

## 3. Scope

### In scope
- Train the **LSTM-Attention forecaster** on real data and load it at runtime.
- Train **4 of 7 LightGBM heads** on real trace-derived labels: `node_health`,
  `autoscale`, `migration`, `provisioning`.
- Keep **3 LightGBM heads** as **transparent rules** (no fake training):
  `caching`, `storage_tiering`, `load_balancing` — they depend on storage/IO signals that
  no available data provides (Year-3 per blueprint).
- Bake artifacts into the image (multi-stage build); preserve `MODEL_PATH`/`--policy-models`
  override.
- Evaluation vs baselines; unit tests; live 99 deploy + E2E divergence demo.
- Honesty updates (logs, code comments, root CLAUDE.md).

### Out of scope
- **PPO / scheduling-policy-engine RL** — no training, no isolation guard this round
  (separate follow-up track, §10).
- **Synthetic data** — reviewed and **not used**. The occupancy-consistent GPU-2023 path
  removes the need; standalone synthetic risks "a real model that learned a fake world."
- **Live-usage feature plumbing** (metrics-server / Prometheus true utilization) and
  storage/IO/cache telemetry — Year-3 / follow-up. Models stay occupancy-consistent.

## 4. Data strategy — GPU-2023 anchor, occupancy-consistent

**Source**: Alibaba `cluster-trace-gpu-v2023` (`openb`) — public CSVs from
`alibaba/clusterdata`. Verified contents: 1523 nodes (capacities `cpu_milli`,
`memory_mib`, `gpu`, `model`), 8152 pods (`cpu_milli`, `memory_mib`, `num_gpu`,
`gpu_milli`, `gpu_spec`, `qos`, `pod_phase`, `creation_time`, `scheduled_time`,
`deletion_time`). **No utilization time-series, no storage/IO** — it is a scheduling /
placement request trace.

**Key insight (why this fits)**: the runtime forecaster sees `requests/capacity`
(allocation occupancy). Replaying GPU-2023 pod lifecycles yields exactly the same kind of
signal — node **occupancy** over time. Training on it is therefore **train/serve
consistent**. We honestly name the forecaster's target **"allocation occupancy
forecasting,"** not true-utilization forecasting.

**What the trace cannot provide** (→ rules / Year-3):
- True intra-pod utilization curves → not modeled (no synthetic overlay this round).
- storage/IO/cache signals → `caching`/`storage_tiering`/`load_balancing` stay rule-based.

## 5. Architecture / new components

```
node-resource-forecaster/
├── training/                       [NEW]
│   ├── data/
│   │   ├── acquire.py              # download GPU-2023 openb CSVs + checksum + cache
│   │   └── replay.py               # pod lifecycle → per-node occupancy time series
│   │                               #   + queue metrics (pending, wait) + contention events
│   ├── labels.py                   # forward-looking labels for the 4 trained heads
│   ├── features.py                 # 25-feature contract — SINGLE SOURCE (train == serve)
│   ├── train_lstm.py               # train LSTM-Attention on occupancy series → .pt
│   ├── train_lightgbm.py           # train 4 heads (forecaster preds as features) → .txt
│   ├── evaluate.py                 # train/val/test split → eval_report.{json,md}
│   └── train.py                    # capstone, seeded/deterministic, reproducible
├── models/                         # train/load methods already exist — only call them
└── server/grpc_server.py           # [MOD] wire baked paths; active/fallback logging
```

## 6. Data pipeline (`training/data/`)

1. **acquire.py** — download `openb_node_list_*.csv` + `openb_pod_list_default.csv` from
   `alibaba/clusterdata` (GPU-2023) into a cache dir; verify size/checksum; idempotent.
2. **replay.py** — minute-step replay over `[min(creation), max(deletion)]`:
   - per-node occupancy: `Σ requests(running pods on node) / capacity` for cpu/mem/gpu
     (a pod is "running" on its node when `scheduled_time ≤ t < deletion_time`).
   - queue metrics: `queue_pending(t)` = pods created but not yet scheduled;
     `queue_admitted(t)` = scheduled in window; per-pod `wait = scheduled − creation`.
   - contention events: timestamps where pending demand exceeds free capacity.
   - **deterministic** given trace + step; emits tidy arrays for training.

## 7. Labels for the 4 trained heads (`labels.py`)

Forward-looking, derived from the **true future of the replay** (not current-instant
thresholds → breaks circularity):
- **node_health** (3-class): band of future occupancy over horizon → NORMAL/STRESSED/CRITICAL.
- **autoscale** (binary): future pending exceeds capacity for a sustained window → yes.
- **migration** (binary): a scheduled pod's node reaches sustained CRITICAL within horizon → yes.
- **provisioning** (binary): future GPU request demand spikes (forward pending GPU) → yes.

The other 3 heads receive **no labels** and remain rule-based at serve time.

## 8. Feature contract (`features.py`) — single source

The 25-feature vector is built by ONE module imported by both training and serving, so
train and serve cannot drift:
- current occupancy: `current_cpu/memory/gpu_util` (= requests/capacity), `current_pending_pods`
- forecaster predictions (16): `pred_{15,30,60,120}_{cpu,memory,gpu,pending}` — produced by
  the **trained LSTM** (at train time too, so the LightGBM sees the same distribution it
  will see at serve time)
- `cpu_trend`, `memory_trend`
- `node_type` (0=compute,1=gpu,2=storage from node `model`/`gpu`)
- `queue_pending`, `queue_admitted` (from replay at train; from Kueue at serve)

All 25 features are suppliable from GPU-2023 replay (train) and from runtime sources
(serve). No hardcoded-constant features enter the trained models.

## 9. Training pipeline (`training/`)

- **train_lstm.py**: sliding windows over occupancy series → `LSTMAttentionTrainer.train_epoch`
  loop → save `lstm_attention.pt` (+ any scaler). Reuses existing trainer; adds the caller.
- **train_lightgbm.py**: build features (predictions via trained LSTM) + 4 labels →
  `MultiLightGBM.train_models()` → write `{node_health,autoscale,migration,provisioning}_model.txt`.
  The 3 rule heads are intentionally not written.
- **evaluate.py**: chronological train/val/test split.
  - LSTM: RMSE/MAE per horizon vs **persistence baseline** (predict = last value).
  - LightGBM: accuracy + macro-F1 per head vs **threshold-rule baseline**.
  - Emits `eval_report.json` + `eval_report.md` (committed as evidence).
- **train.py**: capstone (acquire → replay → label → train_lstm → train_lightgbm →
  evaluate). Fixed seed, deterministic, reproducible from source.

## 10. Runtime loading + image bake (`server/`, Dockerfile)

- **Multi-stage Dockerfile**: stage 1 runs `train.py` (deterministic) producing artifacts;
  stage 2 copies only the artifacts into the runtime image. No committed binaries; fully
  reproducible from source.
- Entrypoint passes `--model /models/lstm_attention.pt --policy-models /models/`
  (fix the env↔args gap). `MODEL_PATH`/`--policy-models` remain as overrides (swap artifacts
  without rebuild via the existing `/models` PVC).
- **Active/fallback logging + health flag**: at startup log e.g.
  `loaded: LSTM-Attention=ok, LightGBM 4/7 trained (caching/storage_tiering/load_balancing=rule-based)`.
  If an artifact is missing/corrupt → fall back to rule/threshold (existing safety) but log
  it loudly and expose a readiness flag.

## 11. Honesty changes

- 3 rule heads: code comments + startup log + docs state
  "rule-based on forecast (Year-3 metric pending)."
- Root `CLAUDE.md`: update the "forecaster is threshold-based, not trained ML" note to the
  new reality — forecaster + 4 heads trained on GPU-2023 (occupancy), 3 heads rule-based,
  **PPO still untrained (separate track)**.
- Forecaster naming everywhere: "allocation occupancy forecasting," not true utilization.

## 12. Verification

- **Unit (TDD)**: replay determinism; feature-contract equality (train builder == serve
  builder); `train.py` smoke on a tiny slice; artifact load → finite predictions;
  "model path active, not fallback" assertion; eval beats baseline on the test split.
- **Eval evidence**: committed `eval_report.md` showing LSTM < persistence error and
  LightGBM > threshold baseline F1 on held-out data.
- **Live 99**: multi-stage build → deploy forecaster → confirm startup "loaded" log →
  forecast endpoint returns model output (not random-sim) → demonstrate a
  **model-vs-threshold divergence** case. Respect the verification-week practice: commit
  first; deploy via the user's `!`.
- **E2E**: add a "trained-model active" gate to the `02-scheduling` / `03-orchestration`
  harness so regressions surface.

## 13. Risks & limitations (stated honestly)

- Occupancy ≠ true utilization: forecaster forecasts **allocation occupancy**; named as such.
  True-usage forecasting needs Year-3 telemetry + runtime plumbing (follow-up).
- GPU-2023 has no storage/IO → 3 heads cannot be trained honestly now (kept as rules).
- Eval proves models learned the **trace**; production correctness on the 99 cluster is not
  claimed beyond the live divergence demo. We do not overclaim.
- Idle 99 cluster: the live divergence demo may require replaying trace-driven load; this is
  a demonstration aid, noted as such.
- Multi-stage build trains torch+lightgbm at build time → longer image builds (acceptable).

## 14. Out of scope / follow-ups

- **PPO / scheduling-policy-engine**: untrained; no training and no isolation guard this
  round (user directive). Separate track later.
- Live-usage / storage-IO / cache telemetry (Year-3) → enables true-utilization forecasting
  and the 3 rule heads to become trained.
- 6h sliding-window online retraining loop (blueprint slide 11) — wire later once live
  outcome accumulation exists.

## 15. Definition of done

1. `train.py` deterministically produces `lstm_attention.pt` + 4 `*_model.txt` from GPU-2023.
2. Multi-stage image bakes them; forecaster starts and logs all artifacts loaded; 4/7
   LightGBM heads served from models, 3 from rules.
3. `eval_report.md` shows trained models beat their baselines on held-out data.
4. Unit tests green (replay determinism, contract equality, load≠fallback, eval>baseline).
5. Root CLAUDE.md reflects the honest new state.
6. Live 99: forecaster deployed, "loaded" confirmed, one model-vs-threshold divergence shown.
