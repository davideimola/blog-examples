CLUSTER := two-doors
EG_VERSION := 1.9.1
AIGW_VERSION := v1.2.0
K := kubectl --context kind-$(CLUSTER)

.PHONY: up cluster image platform world open closed probe logs down

# Everything from nothing: a kind cluster, Envoy Gateway with the AI Gateway
# extension, the outside world, the gateway and the tool doors.
up: cluster image platform world

cluster:
	kind get clusters | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER)

image:
	docker build -t two-doors:dev .
	kind load docker-image two-doors:dev --name $(CLUSTER)

platform:
	helm upgrade --install --kube-context kind-$(CLUSTER) eg oci://docker.io/envoyproxy/gateway-helm \
	  --version $(EG_VERSION) -n envoy-gateway-system --create-namespace -f k8s/platform-values.yaml --wait
	helm upgrade --install --kube-context kind-$(CLUSTER) aieg-crd oci://docker.io/envoyproxy/ai-gateway-crds-helm \
	  --version $(AIGW_VERSION) -n envoy-ai-gateway-system --create-namespace --wait
	helm upgrade --install --kube-context kind-$(CLUSTER) aieg oci://docker.io/envoyproxy/ai-gateway-helm \
	  --version $(AIGW_VERSION) -n envoy-ai-gateway-system --create-namespace --wait

world:
	$(K) apply -f k8s/base/
	$(K) -n internet rollout status deploy/provider deploy/web
	$(K) -n attacker rollout status deploy/collector
	$(K) -n fetcher rollout status deploy/fetcher
	$(K) -n egress-proxy rollout status deploy/squid
	$(K) -n envoy-gateway-system wait --for=condition=Available deploy/ai-gateway --timeout=180s

# The agent with its doors open: key in the pod, tools go anywhere.
open:
	$(K) delete namespace agent --ignore-not-found --wait
	$(K) apply -f k8s/open/
	$(K) -n agent wait --for=condition=Complete job/agent --timeout=60s || true
	$(K) -n agent logs job/agent

# The same agent with both doors closed.
closed:
	$(K) delete namespace agent --ignore-not-found --wait
	$(K) apply -f k8s/closed/egress.yaml
	$(K) apply -f k8s/closed/agent.yaml
	$(K) -n agent wait --for=condition=Complete job/agent --timeout=60s || true
	$(K) -n agent logs job/agent

probe:
	$(K) -n agent wait --for=condition=Complete job/probe --timeout=60s || true
	$(K) -n agent logs job/probe

# What the attacker received.
logs:
	$(K) -n attacker logs deploy/collector

down:
	kind delete cluster --name $(CLUSTER)
