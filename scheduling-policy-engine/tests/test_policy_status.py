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
