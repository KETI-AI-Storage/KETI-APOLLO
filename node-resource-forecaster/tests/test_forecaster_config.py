from models.forecaster_config import (
    make_occupancy_forecaster_config, OCCUPANCY_INPUT_DIM, OCCUPANCY_CHANNELS,
)


def test_occupancy_config_dims():
    cfg = make_occupancy_forecaster_config()
    assert cfg.input_dim == OCCUPANCY_INPUT_DIM == 4
    assert cfg.output_dim == 4
    assert cfg.sequence_length == 60
    assert cfg.forecast_horizons == [15, 30, 60, 120]
    assert OCCUPANCY_CHANNELS == ["cpu", "memory", "gpu", "pending"]
