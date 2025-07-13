include .envrc

# ==================================================================================== #
# HELPERS
# ==================================================================================== #

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' |  sed -e 's/^/ /'

.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

# ==================================================================================== #
# QUALITY CONTROL
# ==================================================================================== #

## audit: tidy and vendor dependencies and format, vet and test all code
.PHONY: audit
audit: vendor
	@echo 'Formatting code...'
	go fmt ./...
	@echo 'Vetting code...'
	go vet ./...
	staticcheck ./...
	@echo 'Running tests...'
	go test -race -vet=off ./...

## vendor: tidy and vendor dependencies
.PHONY: vendor
vendor:
	@echo 'Tidying and verifying module dependencies...'
	go mod tidy
	go mod verify
	@echo 'Vendoring dependencies...'
	go mod vendor

# ==================================================================================== #
# BUILD
# ==================================================================================== #

## build/remix: build the cmd/remix application
.PHONY: build/remix
build/remix:
	@echo 'Building cmd/remix...'
	GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o=./bin/remix ./cmd/remix
	# go build -ldflags="-w -s" -o=./bin/remix ./cmd/remix

## build/docker: build the cmd/remix docker image and push
.PHONY: build/docker
build/docker:
	@echo 'Building cmd/remix...'
	GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o=./bin/remix ./cmd/remix
	@echo 'Building docker image...'
	docker buildx build --platform linux/amd64 -t sqlpipe/remix:latest -f dockerfile . --load
	@echo 'Pushing docker image...'
	docker push sqlpipe/remix:latest

## test: run tests in the /test directory
.PHONY: test
test:
	@echo 'Running tests in the /test directory...'
	STRIPE_API_KEY=$(STRIPE_API_KEY) go test -v -count=1 ./test/...

## one-model: run the one_model_test.go test in the /test directory
.PHONY: one-model
one-model:
	@echo 'Running one_model_test.go in the /test directory...'
	STRIPE_API_KEY=$(STRIPE_API_KEY) go test -v -count=1 ./test/stripe_one_model_test.go

## two-models: run the two_models_test.go test in the /test directory
.PHONY: two-models
two-models:
	@echo 'Running two_models_test.go in the /test directory...'
	STRIPE_API_KEY=$(STRIPE_API_KEY) go test -v -count=1 ./test/stripe_two_models_test.go
