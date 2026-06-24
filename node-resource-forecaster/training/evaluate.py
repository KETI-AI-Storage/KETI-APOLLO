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


def _head_verdict(model_f1: float, rule_f1: float, n_positive: int) -> str:
    """Return an honest per-head evaluation verdict.

    Returns ``"not_evaluable"`` when there are no positive examples in the
    held-out slice (both sides trivially score 1.0 accuracy / 0.0 F1, so the
    comparison is meaningless).  Otherwise uses F1 as the primary metric.
    """
    if n_positive == 0:
        return "not_evaluable"  # no positive examples in held-out slice
    if model_f1 > rule_f1:
        return "model_better"
    if model_f1 == rule_f1:
        return "tie"
    return "rule_better"


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
        model_f1 = float(f1_score(y, y_model, average=avg, zero_division=0))
        rule_f1 = float(f1_score(y, y_rule, average=avg, zero_division=0))
        # For binary heads: positive = class 1.
        # For node_health (multiclass): positive = STRESSED or CRITICAL (non-NORMAL, i.e. != 0).
        n_positive = int((y != 0).sum())
        verdict = _head_verdict(model_f1, rule_f1, n_positive)
        report[head] = {
            "model_acc": float(accuracy_score(y, y_model)),
            "rule_acc": float(accuracy_score(y, y_rule)),
            "model_f1": model_f1,
            "rule_f1": rule_f1,
            "n_positive": n_positive,
            "n_test": n,
            "verdict": verdict,
            # beats_baseline is True ONLY when the model is genuinely better (F1-based).
            # Ties and not_evaluable heads are NOT counted as wins.
            "beats_baseline": verdict == "model_better",
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
            verdict = m.get("verdict", "unknown")
            model_f1 = m.get("model_f1", float("nan"))
            rule_f1 = m.get("rule_f1", float("nan"))
            n_pos = m.get("n_positive", "?")
            n_test = m.get("n_test", "?")
            model_acc = m.get("model_acc", float("nan"))
            rule_acc = m.get("rule_acc", float("nan"))
            lines.append(
                f"- {head}: **{verdict}** | "
                f"model_f1={model_f1:.3f} vs rule_f1={rule_f1:.3f} | "
                f"positives={n_pos}/{n_test} | "
                f"acc=({model_acc:.3f} vs {rule_acc:.3f})"
            )
        lines.append("")
        lines.append(
            "> **Note:** heads with `not_evaluable` had no positive examples in the "
            "held-out slice (sparse events) and are therefore not validated by this metric."
        )
        lines.append("")
    lines += ["> Trained on the Alibaba GPU-2023 trace (allocation occupancy). "
              "Metrics are held-out on the trace; production correctness is not claimed."]
    with open(os.path.join(out_dir, "eval_report.md"), "w") as f:
        f.write("\n".join(lines))
