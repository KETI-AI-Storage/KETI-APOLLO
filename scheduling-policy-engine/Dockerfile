# Scheduling Policy Engine Dockerfile
# PPO RL 기반 스케줄링 정책 엔진 (CPU-only 경량화)

FROM python:3.11-slim

WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

# Install PyTorch CPU-only version separately (약 200MB)
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu

# Copy and install other requirements
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy proto files and generate Python stubs
# forecaster.proto: build-images.sh에서 node-resource-forecaster/proto/forecaster.proto 복사 후 빌드
COPY proto/ ./proto/
RUN python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/apollo.proto \
    && python -m grpc_tools.protoc -I./proto --python_out=. --grpc_python_out=. ./proto/forecaster.proto

# Copy source code
COPY models/ ./models/
COPY server/ ./server/

# Create models directory for checkpoints
RUN mkdir -p /models

# Environment variables
ENV PYTHONUNBUFFERED=1
ENV GRPC_PORT=50054
ENV MODEL_PATH=/models/ppo_scheduler.pt
ENV LOG_LEVEL=INFO

EXPOSE 50054
EXPOSE 9090

CMD ["python", "server/grpc_server.py", "--port", "50054"]
