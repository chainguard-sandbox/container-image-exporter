LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST             ?= $(LOCALBIN)/setup-envtest
ENVTEST_VERSION     ?= release-0.21
ENVTEST_K8S_VERSION ?= 1.33.x

K3D_VERSION       ?= v5.7.5
K3D_OS            := $(shell uname -s | tr '[:upper:]' '[:lower:]')
K3D_ARCH          := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
K3D               ?= $(LOCALBIN)/k3d
NODE_TEST_CLUSTER ?= cie-node-test
NODE_TEST_IMAGE   ?= cgr.dev/chainguard/nginx:latest

HELM_TEST_CLUSTER    ?= cie-helm-test
HELM_TEST_IMAGE_NAME ?= container-image-exporter
HELM_TEST_IMAGE_TAG  ?= helm-test
HELM_TEST_NAMESPACE  ?= container-image-exporter
HELM_MONITORING_NS   ?= monitoring

ifeq ($(shell uname -s),Darwin)
SHA256SUM := shasum -a 256
else
SHA256SUM := sha256sum
endif

.PHONY: build
build:
	go build ./...

.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./...

.PHONY: test-node
test-node: $(K3D)
	@set -e; \
	trap '$(K3D) cluster delete $(NODE_TEST_CLUSTER) 2>/dev/null || true' EXIT; \
	$(K3D) cluster delete $(NODE_TEST_CLUSTER) 2>/dev/null || true; \
	$(K3D) cluster create $(NODE_TEST_CLUSTER) --no-lb --wait; \
	until kubectl get serviceaccount default -n default >/dev/null 2>&1; do sleep 1; done; \
	kubectl run test-pod --image=$(NODE_TEST_IMAGE); \
	kubectl wait --for=condition=ready pod/test-pod --timeout=60s; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$(K3D_ARCH) \
		go test -tags nodeintegration -c -o $(LOCALBIN)/node-integration.test ./internal/nodeexporter/; \
	docker cp $(LOCALBIN)/node-integration.test \
		k3d-$(NODE_TEST_CLUSTER)-server-0:/node-integration.test; \
	docker exec \
		-e CRI_SOCKET=/run/k3s/containerd/containerd.sock \
		k3d-$(NODE_TEST_CLUSTER)-server-0 /node-integration.test -test.v

.PHONY: test-helm
test-helm: $(K3D)
	K3D=$(K3D) \
	HELM_TEST_CLUSTER=$(HELM_TEST_CLUSTER) \
	HELM_TEST_IMAGE_NAME=$(HELM_TEST_IMAGE_NAME) \
	HELM_TEST_IMAGE_TAG=$(HELM_TEST_IMAGE_TAG) \
	HELM_TEST_NAMESPACE=$(HELM_TEST_NAMESPACE) \
	HELM_MONITORING_NS=$(HELM_MONITORING_NS) \
	./scripts/test-helm.sh

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install \
		sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

# SHA-256 checksums for k3d $(K3D_VERSION) binaries.
K3D_SHA256_linux_amd64   := 5d3f22817d9e163ab6ed43572189dd49fe724d7a6948075b570067747eca8d3f
K3D_SHA256_linux_arm64   := ac12fcf8e35481769e173c96d3fa70dc581826482d927b94a560a3375df2621e
K3D_SHA256_darwin_amd64  := 94f6277990c37ade24b69d3dd1a2dd5656bbcac1402ce33797a5751d93e8863e
K3D_SHA256_darwin_arm64  := 2e877c0c33e0fbc497faf3d1b14f22067aa9905c3777faa0abffba392a32ec27
K3D_SHA256               := $(K3D_SHA256_$(K3D_OS)_$(K3D_ARCH))

$(K3D): $(LOCALBIN)
	curl -sSfL \
		"https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/k3d-$(K3D_OS)-$(K3D_ARCH)" \
		-o $(K3D).tmp
	ACTUAL=$$($(SHA256SUM) $(K3D).tmp | awk '{print $$1}'); \
	test "$(K3D_SHA256)" = "$$ACTUAL" || \
		{ echo "k3d checksum mismatch: expected $(K3D_SHA256) got $$ACTUAL" >&2; \
		  rm -f $(K3D).tmp; exit 1; }
	mv $(K3D).tmp $(K3D)
	chmod +x $(K3D)
