#!/usr/bin/env bash
set -eo pipefail # stop on any error; commands with '|' fail if any command fails

CLUSTER=lab
IMAGE=lab-api:0.1

# create the kind cluster if it doesn't exist yet
# q = "print nothing, just give the result"
# x = "match the whole line"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER"

# build api image from Dockerfile
docker build -t "$IMAGE" ./api
# ... and make it available inside the cluster
kind load docker-image "$IMAGE" --name "$CLUSTER"

# add prometheus and grafana to helm (k8s package manager)
# >dev/null to send messages like "Repository added successfully!" to linux folder that'll auto-delete them
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
helm repo add grafana https://grafana.github.io/helm-charts >/dev/null

# fetch latest charts (collection of yamls) from both repositories
helm repo update >/dev/null

# prometheus, alertmanager, grafana, helpers
helm upgrade --install kps prometheus-community/kube-prometheus-stack -n monitoring --create-namespace -f helm/values/kube-prometheus-stack.yaml --wait --timeout 10m
# loki
helm upgrade --install loki grafana/loki -n monitoring -f helm/values/loki.yaml --wait --timeout 10m
# log collector
helm upgrade --install alloy grafana/alloy -n monitoring -f helm/values/alloy.yaml --wait --timeout 5m
# jaeger
helm upgrade --install extras ./helm/observability-extras -n monitoring --wait

helm upgrade --install lab-api ./helm/lab-api -n lab --create-namespace --wait

echo "done. run ./port-forward.sh to open the UIs."
