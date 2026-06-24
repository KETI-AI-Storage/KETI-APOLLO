from server.artifact_status import artifact_status, TRAINED_HEADS, RULE_HEADS


def test_status_reports_trained_and_rule():
    st = artifact_status(lstm_loaded=True, loaded_heads=["node_health", "autoscale"])
    assert st["lstm_loaded"] is True
    assert "node_health" in st["lightgbm_trained"]
    assert "autoscale" in st["lightgbm_trained"]
    # migration/provisioning not in loaded_heads -> fall to rule list
    assert "migration" in st["lightgbm_rule"]
    assert "provisioning" in st["lightgbm_rule"]
    assert set(RULE_HEADS) <= set(st["lightgbm_rule"])
    assert "LSTM-Attention=ok" in st["summary"]
    assert "2/7" in st["summary"]


def test_status_missing_lstm_and_no_heads():
    st = artifact_status(lstm_loaded=False, loaded_heads=[])
    assert st["lstm_loaded"] is False
    assert st["lightgbm_trained"] == []
    assert "MISSING->fallback" in st["summary"]
