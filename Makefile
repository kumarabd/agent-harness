.DEFAULT_GOAL := help

# Builds and exports the two worker container images (deploy/docker/*.Dockerfile)
# using Podman. Image names and tags are read directly out of the two Helm
# charts' values.yaml files (deploy/helm/agent-harness-shared for the
# loop worker, deploy/helm/agent-harness-tenant for the tenant worker —
# split per docs/components/multi-tenancy.md) rather than duplicated here, so
# `helm install` always picks up exactly what this built without a
# values.yaml edit, and the two can't silently drift apart. Both Dockerfiles
# require the repo root as their build context — see the comment at the top
# of each for why (they COPY from both workflows/ and activities/).

SHARED_VALUES_FILE := deploy/helm/agent-harness-shared/values.yaml
TENANT_VALUES_FILE := deploy/helm/agent-harness-tenant/values.yaml
EXPORT_DIR          ?= dist/images

yaml_field = $(shell python3 -c "import yaml; print(yaml.safe_load(open('$(1)'))$(2))")

LOOP_IMAGE_NAME   := $(call yaml_field,$(SHARED_VALUES_FILE),['image']['repository'])
LOOP_IMAGE_TAG    := $(call yaml_field,$(SHARED_VALUES_FILE),['image']['tag'])
TENANT_IMAGE_NAME := $(call yaml_field,$(TENANT_VALUES_FILE),['tenantWorker']['image']['repository'])
TENANT_IMAGE_TAG  := $(call yaml_field,$(TENANT_VALUES_FILE),['tenantWorker']['image']['tag'])

LOOP_IMAGE   := $(LOOP_IMAGE_NAME):$(LOOP_IMAGE_TAG)
TENANT_IMAGE := $(TENANT_IMAGE_NAME):$(TENANT_IMAGE_TAG)

# Exported tar filenames use just the last path component of the repository
# (e.g. gcr.io/kumarabd/agent-harness/loop-worker -> loop-worker) — the full
# repository is a registry path, not a filesystem-safe name.
LOOP_TAR   := $(EXPORT_DIR)/$(notdir $(LOOP_IMAGE_NAME))-$(LOOP_IMAGE_TAG).tar
TENANT_TAR := $(EXPORT_DIR)/$(notdir $(TENANT_IMAGE_NAME))-$(TENANT_IMAGE_TAG).tar

.PHONY: help build build-loop-worker build-tenant-worker \
        export export-loop-worker export-tenant-worker \
        clean clean-images clean-exports

help: ## Show this help
	@echo "Usage: make <target> [EXPORT_DIR=dist/images]"
	@echo "Image name/tag come from each chart's values.yaml:"
	@echo "  loop worker:   $(LOOP_IMAGE)  ($(SHARED_VALUES_FILE))"
	@echo "  tenant worker: $(TENANT_IMAGE)  ($(TENANT_VALUES_FILE))"
	@echo
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*##"}; {printf "  %-24s %s\n", $$1, $$2}'

build: build-loop-worker build-tenant-worker ## Build both worker images

build-loop-worker: ## Build the Go loop-worker image
	podman build -f deploy/docker/loop-worker.Dockerfile -t $(LOOP_IMAGE) .

build-tenant-worker: ## Build the Python tenant-worker image
	podman build -f deploy/docker/tenant-worker.Dockerfile -t $(TENANT_IMAGE) .

export: export-loop-worker export-tenant-worker ## Build and export both images as tars under $(EXPORT_DIR)

export-loop-worker: build-loop-worker ## Build and export the loop-worker image
	mkdir -p $(EXPORT_DIR)
	podman save -o $(LOOP_TAR) $(LOOP_IMAGE)

export-tenant-worker: build-tenant-worker ## Build and export the tenant-worker image
	mkdir -p $(EXPORT_DIR)
	podman save -o $(TENANT_TAR) $(TENANT_IMAGE)

clean-exports: ## Remove exported image tars
	rm -rf $(EXPORT_DIR)

clean-images: ## Remove the locally built images from Podman's store
	-podman rmi $(LOOP_IMAGE) $(TENANT_IMAGE)

clean: clean-exports clean-images ## Remove both exported tars and built images
