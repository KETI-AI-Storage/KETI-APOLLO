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
