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
