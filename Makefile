.PHONY: run test vet build docker tidy

run:    ## run the server (dev)
	go run ./cmd/server

test:   ## run all tests
	go test ./...

vet:    ## static checks
	go vet ./...

build:  ## build the server binary into bin/
	go build -ldflags="-s -w" -o bin/server ./cmd/server

docker: ## build the docker image
	docker build -t tonpayment:latest .

tidy:   ## tidy go.mod / go.sum
	go mod tidy
