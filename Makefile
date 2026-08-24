.DEFAULT_GOAL := help

# Builds and exports the three container images (deploy/docker/*.Dockerfile)
# using Podman. Image names come from the two Helm charts' values.yaml files
# (deploy/helm/agent-harness-shared for the loop worker, deploy/helm/
# agent-harness-tenant for the tenant worker and gateway — split per
# docs/components/multi-tenancy.md) rather than duplicated here, so
# `helm install` always picks up exactly what this built without a
# values.yaml edit, and they can't silently drift apart. Tags default the
# same way, but can be overridden for all three images at once with
# `make export TAG=<tag>` — handy for a one-off build that isn't meant to
# match the checked-in values.yaml. All three Dockerfiles require the repo
# root as their build context — see the comment at the top of each for why
# (they COPY from workflows/ and/or activities/).

SHARED_VALUES_FILE := deploy/helm/agent-harness-shared/values.yaml
TENANT_VALUES_FILE := deploy/helm/agent-harness-tenant/values.yaml
EXPORT_DIR          ?= dist/images
TAG                 ?=

yaml_field = $(shell python3 -c "import yaml; print(yaml.safe_load(open('$(1)'))$(2))")

LOOP_IMAGE_NAME    := $(call yaml_field,$(SHARED_VALUES_FILE),['image']['repository'])
TENANT_IMAGE_NAME  := $(call yaml_field,$(TENANT_VALUES_FILE),['tenantWorker']['image']['repository'])
GATEWAY_IMAGE_NAME := $(call yaml_field,$(TENANT_VALUES_FILE),['gateway']['image']['repository'])

ifeq ($(TAG),)
LOOP_IMAGE_TAG     := $(call yaml_field,$(SHARED_VALUES_FILE),['image']['tag'])
TENANT_IMAGE_TAG   := $(call yaml_field,$(TENANT_VALUES_FILE),['tenantWorker']['image']['tag'])
GATEWAY_IMAGE_TAG  := $(call yaml_field,$(TENANT_VALUES_FILE),['gateway']['image']['tag'])
else
LOOP_IMAGE_TAG     := $(TAG)
TENANT_IMAGE_TAG   := $(TAG)
GATEWAY_IMAGE_TAG  := $(TAG)
endif

LOOP_IMAGE    := $(LOOP_IMAGE_NAME):$(LOOP_IMAGE_TAG)
TENANT_IMAGE  := $(TENANT_IMAGE_NAME):$(TENANT_IMAGE_TAG)
GATEWAY_IMAGE := $(GATEWAY_IMAGE_NAME):$(GATEWAY_IMAGE_TAG)

# Exported tar filenames use just the last path component of the repository
# (e.g. gcr.io/kumarabd/agent-harness/loop-worker -> loop-worker) — the full
# repository is a registry path, not a filesystem-safe name.
LOOP_TAR    := $(EXPORT_DIR)/$(notdir $(LOOP_IMAGE_NAME))-$(LOOP_IMAGE_TAG).tar
TENANT_TAR  := $(EXPORT_DIR)/$(notdir $(TENANT_IMAGE_NAME))-$(TENANT_IMAGE_TAG).tar
GATEWAY_TAR := $(EXPORT_DIR)/$(notdir $(GATEWAY_IMAGE_NAME))-$(GATEWAY_IMAGE_TAG).tar

.PHONY: help build build-loop-worker build-tenant-worker build-gateway \
        export export-loop-worker export-tenant-worker export-gateway \
        clean clean-images clean-exports

help: ## Show this help
	@echo "Usage: make <target> [EXPORT_DIR=dist/images] [TAG=<tag>]"
	@echo "Image name comes from each chart's values.yaml; tag does too unless TAG is set:"
	@echo "  loop worker:   $(LOOP_IMAGE)  ($(SHARED_VALUES_FILE))"
	@echo "  tenant worker: $(TENANT_IMAGE)  ($(TENANT_VALUES_FILE))"
	@echo "  gateway:       $(GATEWAY_IMAGE)  ($(TENANT_VALUES_FILE))"
	@echo
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*##"}; {printf "  %-24s %s\n", $$1, $$2}'

build: build-loop-worker build-tenant-worker build-gateway ## Build all three images

build-loop-worker: ## Build the Go loop-worker image
	podman build -f deploy/docker/loop-worker.Dockerfile -t $(LOOP_IMAGE) .

build-tenant-worker: ## Build the Python tenant-worker image
	podman build -f deploy/docker/tenant-worker.Dockerfile -t $(TENANT_IMAGE) .

build-gateway: ## Build the Go gateway image
	podman build -f deploy/docker/gateway.Dockerfile -t $(GATEWAY_IMAGE) .

export: export-loop-worker export-tenant-worker export-gateway ## Build and export all three images as tars under $(EXPORT_DIR)

export-loop-worker: build-loop-worker ## Build and export the loop-worker image
	mkdir -p $(EXPORT_DIR)
	rm -f $(LOOP_TAR)
	podman save -o $(LOOP_TAR) $(LOOP_IMAGE)

export-tenant-worker: build-tenant-worker ## Build and export the tenant-worker image
	mkdir -p $(EXPORT_DIR)
	rm -f $(TENANT_TAR)
	podman save -o $(TENANT_TAR) $(TENANT_IMAGE)

export-gateway: build-gateway ## Build and export the gateway image
	mkdir -p $(EXPORT_DIR)
	rm -f $(GATEWAY_TAR)
	podman save -o $(GATEWAY_TAR) $(GATEWAY_IMAGE)

clean-exports: ## Remove exported image tars
	rm -rf $(EXPORT_DIR)

clean-images: ## Remove the locally built images from Podman's store
	-podman rmi $(LOOP_IMAGE) $(TENANT_IMAGE) $(GATEWAY_IMAGE)

clean: clean-exports clean-images ## Remove all exported tars and built images
