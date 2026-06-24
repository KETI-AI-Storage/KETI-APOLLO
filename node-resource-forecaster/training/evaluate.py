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
