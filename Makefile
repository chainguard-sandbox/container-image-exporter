LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST         ?= $(LOCALBIN)/setup-envtest
ENVTEST_VERSION ?= release-0.21
ENVTEST_K8S_VERSION ?= 1.33.x

.PHONY: build
build:
	go build ./...

.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./...

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install \
		sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
