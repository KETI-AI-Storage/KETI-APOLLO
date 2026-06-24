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
