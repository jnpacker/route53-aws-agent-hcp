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
