#!/usr/bin/env bash

registry="ketidevit2"
image_name="orchestration-policy-engine"
version="latest"
dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir="$dir/.."
namespace="apollo"

IMG="$registry/$image_name:$version"

if [ "$1" == "delete" ] || [ "$1" == "d" ]; then
    echo "================================"
    echo "Deleting Orchestration Policy Engine deployment..."
    echo "================================"
    cd "$project_dir" || exit 1
    make undeploy

    if [ $? -eq 0 ]; then
        echo "Deployment deleted successfully"
    else
        echo "Error: Deployment deletion failed"
        exit 1
    fi

elif [ "$1" == "apply" ] || [ "$1" == "a" ]; then
    echo "================================"
    echo "Applying Orchestration Policy Engine deployment..."
    echo "Namespace: $namespace"
    echo "Image: $IMG"
    echo "================================"

    # Create namespace if not exists
    kubectl get namespace $namespace > /dev/null 2>&1
    if [ $? -ne 0 ]; then
        echo "Creating namespace $namespace..."
        kubectl create namespace $namespace
    fi

    cd "$project_dir" || exit 1
    make deploy IMG="$IMG"

    if [ $? -eq 0 ]; then
        echo "Deployment applied successfully"
        echo ""
        echo "Checking pod status..."
        sleep 3
        kubectl get pods -n $namespace -l control-plane=controller-manager
    else
        echo "Error: Deployment apply failed"
        exit 1
    fi

else
    echo "Usage: $0 [apply|a|delete|d]"
    echo ""
    echo "Examples:"
    echo "  $0 apply    # Apply deployment"
    echo "  $0 a        # Apply deployment (short)"
    echo "  $0 delete   # Delete deployment"
    echo "  $0 d        # Delete deployment (short)"
    exit 1
fi
