.DEFAULT_GOAL := help

# Builds and exports the two worker container images (deploy/docker/*.Dockerfile)
# using Podman. Image names and tags are read directly out of
# deploy/helm/agent-harness/values.yaml (workflowWorker.image /
# activityWorker.image) rather than duplicated here, so `helm install` always
# picks up exactly what this built without a values.yaml edit, and the two
# can't silently drift apart. Both Dockerfiles require the repo root as their
# build context — see the comment at the top of each for why (they COPY from
# both workflows/ and activities/).

VALUES_FILE := deploy/helm/agent-harness/values.yaml
EXPORT_DIR  ?= dist/images

chart_image_field = $(shell python3 -c "import yaml; print(yaml.safe_load(open('$(VALUES_FILE)'))['$(1)']['image']['$(2)'])")

WORKFLOW_IMAGE_NAME := $(call chart_image_field,workflowWorker,repository)
WORKFLOW_IMAGE_TAG  := $(call chart_image_field,workflowWorker,tag)
ACTIVITY_IMAGE_NAME := $(call chart_image_field,activityWorker,repository)
ACTIVITY_IMAGE_TAG  := $(call chart_image_field,activityWorker,tag)

WORKFLOW_IMAGE := $(WORKFLOW_IMAGE_NAME):$(WORKFLOW_IMAGE_TAG)
ACTIVITY_IMAGE := $(ACTIVITY_IMAGE_NAME):$(ACTIVITY_IMAGE_TAG)

# Exported tar filenames use just the last path component of the repository
# (e.g. gcr.io/kumarabd/agent-harness/workflow-worker -> workflow-worker) —
# the full repository is a registry path, not a filesystem-safe name.
WORKFLOW_TAR := $(EXPORT_DIR)/$(notdir $(WORKFLOW_IMAGE_NAME))-$(WORKFLOW_IMAGE_TAG).tar
ACTIVITY_TAR := $(EXPORT_DIR)/$(notdir $(ACTIVITY_IMAGE_NAME))-$(ACTIVITY_IMAGE_TAG).tar

.PHONY: help build build-workflow-worker build-activity-worker \
        export export-workflow-worker export-activity-worker \
        clean clean-images clean-exports

help: ## Show this help
	@echo "Usage: make <target> [EXPORT_DIR=dist/images]"
	@echo "Image name/tag come from $(VALUES_FILE):"
	@echo "  workflow worker: $(WORKFLOW_IMAGE)"
	@echo "  activity worker: $(ACTIVITY_IMAGE)"
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
	podman save -o $(WORKFLOW_TAR) $(WORKFLOW_IMAGE)

export-activity-worker: build-activity-worker ## Build and export the activity-worker image
	mkdir -p $(EXPORT_DIR)
	podman save -o $(ACTIVITY_TAR) $(ACTIVITY_IMAGE)

clean-exports: ## Remove exported image tars
	rm -rf $(EXPORT_DIR)

clean-images: ## Remove the locally built images from Podman's store
	-podman rmi $(WORKFLOW_IMAGE) $(ACTIVITY_IMAGE)

clean: clean-exports clean-images ## Remove both exported tars and built images
