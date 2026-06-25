import torch

from models.ppo import SchedulingPolicyPPO, PPOConfig, PPOTrainer
from training.init_model import (
    build_uniform_model,
    random_state,
    verify_uniform,
    save_uniform_model,
)


def _max_dev_from_uniform(policy: torch.Tensor) -> float:
    expected = torch.full_like(policy, 1.0 / policy.shape[-1])
    return (policy - expected).abs().max().item()


def test_forward_is_exactly_uniform_for_varied_inputs():
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(7)
    for _ in range(32):
        w, p, c = random_state(batch=4, gen=gen)
        with torch.no_grad():
            policy, value = model.forward(w, p, c)
        assert policy.shape[-1] == 5
        assert _max_dev_from_uniform(policy) < 1e-5


def test_get_action_deterministic_is_uniform():
    # This is the exact call the server makes (deterministic=True).
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(11)
    w, p, c = random_state(batch=2, gen=gen)
    with torch.no_grad():
        action, log_prob, value = model.get_action(w, p, c, deterministic=True)
    assert _max_dev_from_uniform(action) < 1e-5


def test_critic_value_is_zero():
    model = build_uniform_model()
    model.eval()
    gen = torch.Generator().manual_seed(3)
    w, p, c = random_state(batch=4, gen=gen)
    with torch.no_grad():
        _, value = model.forward(w, p, c)
    assert value.abs().max().item() < 1e-6


def test_save_load_roundtrip_preserves_uniform(tmp_path):
    out = str(tmp_path / "ppo_scheduler.pt")
    save_uniform_model(out)

    # Reload exactly the way the server does: PPOTrainer.load into a fresh model.
    fresh = SchedulingPolicyPPO(PPOConfig())
    trainer = PPOTrainer(fresh, PPOConfig())
    trainer.load(out)
    fresh.eval()

    gen = torch.Generator().manual_seed(99)
    w, p, c = random_state(batch=4, gen=gen)
    with torch.no_grad():
        policy, _ = fresh.forward(w, p, c)
    assert _max_dev_from_uniform(policy) < 1e-5


def test_verify_uniform_returns_true():
    assert verify_uniform(build_uniform_model()) is True
