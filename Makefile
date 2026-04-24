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

UNITTEST_VERSION ?= 0.8.2
# helm-unittest release artifacts use 'macos' for Darwin rather than 'darwin'.
UNITTEST_OS      := $(shell uname -s | tr '[:upper:]' '[:lower:]' | sed 's/darwin/macos/')
UNITTEST_ARCH    := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
UNITTEST         ?= $(LOCALBIN)/untt

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

.PHONY: test-helm test-helm-lint test-helm-unit test-helm-integration

test-helm: test-helm-lint test-helm-unit test-helm-integration

test-helm-lint:
	./scripts/test-helm-lint.sh

test-helm-unit: $(UNITTEST)
	UNITTEST=$(UNITTEST) ./scripts/test-helm-unit.sh

test-helm-integration: $(K3D)
	K3D=$(K3D) \
	HELM_TEST_CLUSTER=$(HELM_TEST_CLUSTER) \
	HELM_TEST_IMAGE_NAME=$(HELM_TEST_IMAGE_NAME) \
	HELM_TEST_IMAGE_TAG=$(HELM_TEST_IMAGE_TAG) \
	HELM_TEST_NAMESPACE=$(HELM_TEST_NAMESPACE) \
	HELM_MONITORING_NS=$(HELM_MONITORING_NS) \
	./scripts/test-helm-integration.sh

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

# SHA-256 checksums for helm-unittest $(UNITTEST_VERSION) tarballs. Sourced from
# the official helm-unittest-checksum.sha asset on each release.
UNITTEST_SHA256_linux_amd64 := 56ab3091e6fa52a7c92ee951def9bed957f295d9ce98483aed404e748d7b3a94
UNITTEST_SHA256_linux_arm64 := 10a7cad7aab812f26f1478d53c16cc58935870475f449d03aab5e81ee7be3ab4
UNITTEST_SHA256_macos_amd64 := 31e177db0f86b7cd383bb27c0ff219498f8cfb11da5057875183f148ef19f229
UNITTEST_SHA256_macos_arm64 := aa1520a7b156755f62e5692cd565fb4c396c540a0b40f631af826949eadceceb
UNITTEST_SHA256             := $(UNITTEST_SHA256_$(UNITTEST_OS)_$(UNITTEST_ARCH))

$(UNITTEST): $(LOCALBIN)
	curl -sSfL \
		"https://github.com/helm-unittest/helm-unittest/releases/download/v$(UNITTEST_VERSION)/helm-unittest-$(UNITTEST_OS)-$(UNITTEST_ARCH)-$(UNITTEST_VERSION).tgz" \
		-o $(UNITTEST).tgz
	ACTUAL=$$($(SHA256SUM) $(UNITTEST).tgz | awk '{print $$1}'); \
	test "$(UNITTEST_SHA256)" = "$$ACTUAL" || \
		{ echo "helm-unittest checksum mismatch: expected $(UNITTEST_SHA256) got $$ACTUAL" >&2; \
		  rm -f $(UNITTEST).tgz; exit 1; }
	tar -xzf $(UNITTEST).tgz -C $(LOCALBIN) untt
	rm -f $(UNITTEST).tgz
	chmod +x $(UNITTEST)
