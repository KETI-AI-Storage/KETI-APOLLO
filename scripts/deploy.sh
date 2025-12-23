#!/bin/bash
# ============================================
# APOLLO Deploy Script
# Deploys all APOLLO components to Kubernetes
# ============================================

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DEPLOY_DIR="${PROJECT_DIR}/deployments"

ACTION="${1:-apply}"

echo "============================================"
echo "APOLLO Deployment"
echo "Action: ${ACTION}"
echo "============================================"

# Check kubectl
if ! command -v kubectl &> /dev/null; then
    echo "ERROR: kubectl not found"
    exit 1
fi

# Create namespace if needed
kubectl get namespace keti &>/dev/null || kubectl create namespace keti

case "$ACTION" in
    apply|a)
        echo "Deploying APOLLO components..."

        # Deploy in order
        echo "[1/3] Deploying Apollo Forecaster..."
        kubectl apply -f "${DEPLOY_DIR}/apollo-forecaster.yaml"

        echo "[2/3] Deploying Apollo Policy Server..."
        kubectl apply -f "${DEPLOY_DIR}/apollo-policy-server.yaml"

        echo "[3/3] Deploying Insight Scope (DaemonSet)..."
        kubectl apply -f "${DEPLOY_DIR}/insight-scope.yaml"

        echo ""
        echo "Waiting for deployments to be ready..."
        kubectl -n keti wait --for=condition=available deployment/apollo-policy-server --timeout=120s || true
        kubectl -n keti wait --for=condition=available deployment/apollo-forecaster --timeout=120s || true

        echo ""
        echo "APOLLO Deployment Status:"
        kubectl -n keti get pods -l app.kubernetes.io/part-of=apollo
        ;;

    delete|d)
        echo "Deleting APOLLO components..."
        kubectl delete -f "${DEPLOY_DIR}/insight-scope.yaml" --ignore-not-found=true
        kubectl delete -f "${DEPLOY_DIR}/apollo-policy-server.yaml" --ignore-not-found=true
        kubectl delete -f "${DEPLOY_DIR}/apollo-forecaster.yaml" --ignore-not-found=true
        echo "APOLLO components deleted"
        ;;

    status|s)
        echo "APOLLO Status:"
        echo ""
        echo "Pods:"
        kubectl -n keti get pods -l app.kubernetes.io/part-of=apollo -o wide
        echo ""
        echo "Services:"
        kubectl -n keti get svc -l app.kubernetes.io/part-of=apollo
        echo ""
        echo "DaemonSet:"
        kubectl -n keti get daemonset -l app.kubernetes.io/part-of=apollo
        ;;

    logs|l)
        COMPONENT="${2:-apollo-policy-server}"
        echo "Logs for ${COMPONENT}:"
        kubectl -n keti logs -l app.kubernetes.io/name=${COMPONENT} --tail=100 -f
        ;;

    restart|r)
        echo "Restarting APOLLO deployments..."
        kubectl -n keti rollout restart deployment/apollo-policy-server
        kubectl -n keti rollout restart deployment/apollo-forecaster
        kubectl -n keti rollout restart daemonset/insight-scope
        ;;

    *)
        echo "Usage: $0 {apply|delete|status|logs|restart}"
        echo ""
        echo "  apply   (a) - Deploy all APOLLO components"
        echo "  delete  (d) - Delete all APOLLO components"
        echo "  status  (s) - Show APOLLO component status"
        echo "  logs    (l) [component] - Show logs (default: apollo-policy-server)"
        echo "  restart (r) - Restart all APOLLO deployments"
        exit 1
        ;;
esac

echo ""
echo "Done!"
