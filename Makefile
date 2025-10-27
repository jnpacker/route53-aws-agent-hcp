.PHONY: build
build:
	go build -o bin/manager cmd/manager/main.go

.PHONY: run
run:
	go run cmd/manager/main.go

.PHONY: deploy
deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/deployment.yaml

.PHONY: undeploy
undeploy:
	kubectl delete -f deploy/deployment.yaml
	kubectl delete -f deploy/rbac.yaml

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./... -coverprofile cover.out

.PHONY: tidy
tidy:
	go mod tidy

# Podman/Container image targets
IMAGE_NAME ?= route53-aws-agent-hcp
IMAGE_TAG ?= latest
IMAGE_REGISTRY ?= localhost
IMAGE := $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: podman-build
podman-build:
	podman build -t $(IMAGE) -f Dockerfile .

.PHONY: podman-push
podman-push:
	podman push $(IMAGE)

.PHONY: podman-build-push
podman-build-push: podman-build podman-push
	@echo "Updating deployment.yaml with new image SHA..."
	@SHA=$$(podman inspect $(IMAGE) --format='{{.ID}}' | sed 's/sha256://'); \
	sed -i "s|image: .*route53-aws-agent-hcp.*|image: $(IMAGE_REGISTRY)/$(IMAGE_NAME)@sha256:$$SHA|" deploy/deployment.yaml; \
	echo "Deployment updated with image: $(IMAGE_REGISTRY)/$(IMAGE_NAME)@sha256:$$SHA"
