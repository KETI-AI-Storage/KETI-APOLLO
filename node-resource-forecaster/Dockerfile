# Node Resource Forecaster Dockerfile (multi-stage: train artifacts, then runtime)
# 발표자료 기반: LSTM-Attention + Multi-LightGBM. CPU-only.

# ---------- Stage 1: train artifacts from the GPU-2023 trace ----------
FROM python:3.11-slim AS trainer
WORKDIR /build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential libgomp1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
RUN pip install --no-cache-dir "lightgbm>=4.0.0" "pandas>=2.0.0" "scikit-learn>=1.3.0" "numpy>=1.24.0"
COPY models/ ./models/
COPY training/ ./training/
# Deterministic, bounded build-time training (--max-steps caps the 149-day trace span; mandatory).
RUN python -m training.train --out /artifacts --cache /trace-cache \
      --sample 4000 --epochs 10 --seed 0 --max-steps 3000 \
 && ls -la /artifacts

# ---------- Stage 2: runtime image ----------
FROM python:3.11-slim
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends build-essential libgomp1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
RUN pip install --no-cache-dir "lightgbm>=4.0.0"
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt || true
COPY proto/ ./proto/
RUN python -m grpc_tools.protoc \
    -I./proto \
    --python_out=./proto \
    --grpc_python_out=./proto \
    ./proto/forecaster.proto \
    ./proto/hub.proto
COPY models/ ./models/
COPY server/ ./server/
RUN mkdir -p /models /data /policy-models
# Bake trained artifacts to the paths the server loads from (MODEL_PATH + POLICY_MODEL_DIR).
COPY --from=trainer /artifacts/lstm_attention.pt /models/lstm_attention.pt
COPY --from=trainer /artifacts/node_health_model.txt /policy-models/
COPY --from=trainer /artifacts/autoscale_model.txt /policy-models/
COPY --from=trainer /artifacts/migration_model.txt /policy-models/
COPY --from=trainer /artifacts/provisioning_model.txt /policy-models/
ENV PYTHONUNBUFFERED=1
ENV GRPC_PORT=50055
ENV HTTP_PORT=8080
ENV MODEL_PATH=/models/lstm_attention.pt
ENV POLICY_MODEL_DIR=/policy-models
ENV LOG_LEVEL=INFO
EXPOSE 50055
EXPOSE 8080
EXPOSE 9091
CMD ["python", "-m", "server.grpc_server", "--port", "50055", "--http-port", "8080"]
