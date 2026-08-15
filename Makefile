.DEFAULT_GOAL := help

# Builds and exports the two worker container images (deploy/docker/*.Dockerfile)
# using Podman. Both Dockerfiles require the repo root as their build context —
# see the comment at the top of each for why (they COPY from both workflows/
# and activities/). Image names/tags default to what
# deploy/helm/agent-harness/values.yaml already expects.

IMAGE_TAG  ?= local
REGISTRY   ?=
EXPORT_DIR ?= dist/images

WORKFLOW_IMAGE_NAME := agent-harness-workflow-worker
ACTIVITY_IMAGE_NAME := agent-harness-activity-worker

ifdef REGISTRY
WORKFLOW_IMAGE := $(REGISTRY)/$(WORKFLOW_IMAGE_NAME):$(IMAGE_TAG)
ACTIVITY_IMAGE := $(REGISTRY)/$(ACTIVITY_IMAGE_NAME):$(IMAGE_TAG)
else
WORKFLOW_IMAGE := $(WORKFLOW_IMAGE_NAME):$(IMAGE_TAG)
ACTIVITY_IMAGE := $(ACTIVITY_IMAGE_NAME):$(IMAGE_TAG)
endif

.PHONY: help build build-workflow-worker build-activity-worker \
        export export-workflow-worker export-activity-worker \
        clean clean-images clean-exports

help: ## Show this help
	@echo "Usage: make <target> [IMAGE_TAG=local] [REGISTRY=host/org] [EXPORT_DIR=dist/images]"
	@echo
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*##"}; {printf "  %-24s %s\n", $$1, $$2}'

build: build-workflow-worker build-activity-worker ## Build both worker images

build-workflow-worker: ## Build the Go workflow-worker image
	podman build -f deploy/docker/workflow-worker.Dockerfile -t $(WORKFLOW_IMAGE) .

build-activity-worker: ## Build the Python activity-worker image
	podman build -f deploy/docker/activity-worker.Dockerfile -t $(ACTIVITY_IMAGE) .

export: export-workflow-worker export-activity-worker ## Build and export both images as tars under $(EXPORT_DIR)

export-workflow-worker: build-workflow-worker ## Build and export the workflow-worker image
	mkdir -p $(EXPORT_DIR)
	podman save -o $(EXPORT_DIR)/$(WORKFLOW_IMAGE_NAME)-$(IMAGE_TAG).tar $(WORKFLOW_IMAGE)

export-activity-worker: build-activity-worker ## Build and export the activity-worker image
	mkdir -p $(EXPORT_DIR)
	podman save -o $(EXPORT_DIR)/$(ACTIVITY_IMAGE_NAME)-$(IMAGE_TAG).tar $(ACTIVITY_IMAGE)

clean-exports: ## Remove exported image tars
	rm -rf $(EXPORT_DIR)

clean-images: ## Remove the locally built images from Podman's store
	-podman rmi $(WORKFLOW_IMAGE) $(ACTIVITY_IMAGE)

clean: clean-exports clean-images ## Remove both exported tars and built images
