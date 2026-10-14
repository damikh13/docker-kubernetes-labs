#!/usr/bin/env bash
trap 'kill 0' EXIT
kubectl -n lab        port-forward svc/lab-api                 8080:8080  &   # API
kubectl -n monitoring port-forward svc/grafana                 3000:80    &   # Grafana  admin/admin
kubectl -n monitoring port-forward svc/prom-stack-prometheus   9090:9090  &   # Prometheus
kubectl -n monitoring port-forward svc/prom-stack-alertmanager 9093:9093  &   # Alertmanager
kubectl -n monitoring port-forward svc/jaeger                  16686:16686 &  # Jaeger
kubectl -n monitoring port-forward svc/karma                   8081:8080  &   # Karma
echo "API :8080 | Grafana :3000 (admin/admin) | Prometheus :9090 | Alertmanager :9093 | Jaeger :16686 | Karma :8081"
wait
