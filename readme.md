### Project Introduction

> A distributed C2C platform order processing backend built with Golang, utilizing microservices, NATS, Docker, and Kubernetes (local) to demonstrate a scalable architecture.

---

### Local Deployment Guide

**Prerequisites:**
*   Go (version 1.24)
*   Docker
*   Kind (v0.27.0)
*   kubectl (v1.32.2)
*   Helm (v3.18.0)

**Steps:**

**1. Start Local Kubernetes Cluster:**
```bash
kind create cluster --name my-dev-cluster
```
*(If a cluster named `my-dev-cluster` already exists, delete it first with `kind delete cluster --name my-dev-cluster` or use a different name)*

**2. Deploy NATS JetStream:**
*   The NATS Kubernetes configuration file is located at `k8s/nats.yaml`.
    ```bash
    kubectl apply -f k8s/nats.yaml
    ```
*   Wait for the NATS Pods to start and run (e.g., `nats-0`, `nats-1`, `nats-2`):
    ```bash
    kubectl get pods -l app=nats -w
    ```
*   **(Optional) Verify NATS JetStream Status:**
Port-forward to a NATS pod (e.g., `nats-0`):
    ```bash
    kubectl port-forward nats-0 8222:8222
    ```
In another terminal, access the JetStream account status endpoint:
```bash
curl http://localhost:8222/jsz?acc=true
# Or for more details: curl http://localhost:8222/jsz?config=true&accounts=true&streams=true&consumers=true
```

**3. Build and Load Application Docker Images:**
*   Execute the following commands in the project root directory:
    ```bash
    # OrderService
    docker build -t orderservice:latest -f cmd/orderservice/Dockerfile .
    kind load docker-image orderservice:latest --name my-dev-cluster

    # InventoryWorker
    docker build -t inventoryworker:latest -f cmd/inventoryworker/Dockerfile .
    kind load docker-image inventoryworker:latest --name my-dev-cluster

    # NotificationWorker
    docker build -t notificationworker:latest -f cmd/notificationworker/Dockerfile .
    kind load docker-image notificationworker:latest --name my-dev-cluster

    # PaymentSimulatorWorker
    docker build -t paymentsimulatorworker:latest -f cmd/paymentsimulatorworker/Dockerfile .
    kind load docker-image paymentsimulatorworker:latest --name my-dev-cluster
    ```

**4. Deploy Application Services:**
*   All Kubernetes configuration files for the applications (ConfigMaps, Deployments, Services) are located in the `k8s/` directory.
    ```bash
    kubectl apply -f k8s/configmap.yaml
    kubectl apply -f k8s/orderservice.yaml
    kubectl apply -f k8s/workers.yaml 
    ```
*(Alternatively, if all relevant configurations are in one directory and you want to apply them at once: `kubectl apply -f k8s/`)*
*   Check Pod status:
    ```bash
    kubectl get pods -w
    ```

**5. Verification and Testing:**

**5.1 Basic Testing (via `port-forward`):**
*   Port-forward the `OrderService` to access it locally:
    ```bash
    kubectl port-forward svc/orderservice 8080:8080
    ```
*   In another terminal, send a test order request (using `curl`):
    ```bash
    curl -X POST -H "Content-Type: application/json" \
    -d '{"userID":"test-user-123","items":[{"itemID":"item-abc","quantity":2,"price":10.50},{"itemID":"item-xyz","quantity":1,"price":25.00}]}' \
    http://localhost:8080/api/orders
    ```
*   Alternatively, use `hey` for a simple load test (if `hey` is installed locally):
    ```bash
    hey -n 10 -c 2 -m POST -T "application/json" \
    -d '{"userID":"hey-user","items":[{"itemID":"item-hey-001","quantity":1,"price":19.99}]}' \
    http://localhost:8080/api/orders
    ```

**5.2 In-Cluster Load Testing (with `hey` pod):**
*   **Build and load `hey` Docker image:**
(The project includes a `Dockerfile.hey` to build a custom `hey` image. Ensure this is built and loaded if you haven't already.)
    ```bash
    # If not already done, or to ensure it's available:
    docker build -t my-hey:latest -f Dockerfile.hey .
    kind load docker-image my-hey:latest --name my-dev-cluster
    ```
*   **Run `hey` as a one-off Pod:**
Replace `-n <number_of_requests>` and `-c <concurrency_level>` with your desired values.
    ```bash
    kubectl run hey-load-test \
    --image=my-hey:latest \
    --image-pull-policy=Never \
    --rm -it --restart=Never -- \
    hey -n 1000 -c 50 -m POST -T "application/json" \
    -d '{"userID":"cluster-load-test","items":[{"itemID":"item-clt-001","quantity":1,"price":9.99}]}' \
    http://orderservice.default.svc.cluster.local:8080/api/orders
    ```
*(This command runs the `hey` pod, prints its output/logs to your terminal, and then removes the pod. For longer tests or to inspect logs later, you might omit `--rm -it` and use `kubectl logs hey-load-test -f`)*

**5.3 View Service Logs:**
```bash
kubectl logs -l app=orderservice -f
kubectl logs -l app=inventoryworker -f
kubectl logs -l app=notificationworker -f
kubectl logs -l app=paymentsimulatorworker -f
```

**6. (Optional) Deploy Monitoring Stack (Prometheus & Grafana):**

```bash
# Add Helm repository
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Create monitoring namespace
kubectl create namespace monitoring

# Install kube-prometheus-stack
helm install prometheus prometheus-community/kube-prometheus-stack --namespace monitoring --set prometheus.prometheusSpec.maximumStartupDurationSeconds=60

# Apply ServiceMonitors for Prometheus to scrape application metrics (assuming they are in k8s/servicemonitors.yaml)
kubectl apply -f k8s/servicemonitors.yaml
```

*   Access Grafana:
1.  Get Grafana Admin password:

    ```bash
    kubectl get secret --namespace monitoring prometheus-grafana -o jsonpath="{.data.admin-password}" | base64 --decode ; echo
    ```

2.  Port-forward the Grafana Pod:

    ```bash
    export GRAFANA_POD_NAME=$(kubectl get pods --namespace monitoring -l "app.kubernetes.io/name=grafana,app.kubernetes.io/instance=prometheus" -o jsonpath="{.items[0].metadata.name}")
    kubectl port-forward --namespace monitoring $GRAFANA_POD_NAME 3000
    ```

3.  Open `http://localhost:3000` in your browser and log in with username `admin` and the retrieved password.

*   **(Optional) Access Prometheus UI Directly:**
1.  Port-forward the Prometheus Pod (usually `prometheus-prometheus-kube-prometheus-prometheus-0`):

    ```bash
    export PROMETHEUS_POD_NAME=$(kubectl get pods --namespace monitoring -l "app.kubernetes.io/name=prometheus,app.kubernetes.io/instance=prometheus-kube-prometheus" -o jsonpath="{.items[0].metadata.name}")
    kubectl port-forward --namespace monitoring $PROMETHEUS_POD_NAME 9090
    ```

2.  Open `http://localhost:9090` in your browser to access the Prometheus UI.

---