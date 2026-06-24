"""Report which models are actually active (artifact-backed) vs rule-backed (honesty).

Reflects RUNTIME load truth, not mere file presence: lstm_loaded is whether the
checkpoint actually loaded; loaded_heads is the set of LightGBM heads the policy
engine actually loaded as Boosters.
"""
TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]
RULE_HEADS = ["caching", "storage_tiering", "load_balancing"]


def artifact_status(lstm_loaded: bool, loaded_heads) -> dict:
    loaded_heads = list(loaded_heads or [])
    trained = [h for h in TRAINED_HEADS if h in loaded_heads]
    rule = list(RULE_HEADS) + [h for h in TRAINED_HEADS if h not in loaded_heads]
    summary = (f"loaded: LSTM-Attention={'ok' if lstm_loaded else 'MISSING->fallback'}, "
               f"LightGBM {len(trained)}/{len(TRAINED_HEADS) + len(RULE_HEADS)} trained "
               f"({','.join(trained) or 'none'}); rule-based: {','.join(rule)}")
    return {"lstm_loaded": lstm_loaded, "lightgbm_trained": trained,
            "lightgbm_rule": rule, "summary": summary}
