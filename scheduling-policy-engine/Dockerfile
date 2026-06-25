# Scheduling Policy Engine Dockerfile
# Multi-stage: build the analytic uniform-init PPO artifact, then runtime.
# PPO 정책 엔진 (CPU-only). 초기모델 = 결정적 균등 정책(랜덤 제거), 온라인 RL freeze.

# ---------- Stage 1: build the uniform-init artifact ----------
FROM python:3.11-slim AS trainer
WORKDIR /build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
RUN pip install --no-cache-dir "numpy>=1.24.0"
COPY models/ ./models/
COPY training/ ./training/
RUN python -m training.init_model --out /artifacts/ppo_scheduler.pt --seed 0 \
 && ls -la /artifacts

# ---------- Stage 2: runtime ----------
FROM python:3.11-slim
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY proto/ ./proto/
RUN python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/apollo.proto \
    && python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/forecaster.proto
COPY models/ ./models/
COPY server/ ./server/
COPY training/ ./training/
RUN mkdir -p /models /policy-models
# Bake the artifact to the path the server loads from (NOT /models — PVC shadows it).
COPY --from=trainer /artifacts/ppo_scheduler.pt /policy-models/ppo_scheduler.pt
ENV PYTHONUNBUFFERED=1
ENV GRPC_PORT=50054
ENV MODEL_PATH=/policy-models/ppo_scheduler.pt
ENV LOG_LEVEL=INFO
EXPOSE 50054
EXPOSE 9090
CMD ["python", "server/grpc_server.py", "--port", "50054"]
