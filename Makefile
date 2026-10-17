PROTOC ?= protoc
TOOLS := $(CURDIR)/.bin

.PHONY: proto tools build check

tools:
	GOBIN="$(TOOLS)" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	GOBIN="$(TOOLS)" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.4.0

proto: tools
	PATH="$(TOOLS):$$PATH" $(PROTOC) --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/*.proto

build:
	go build .

check:
	go vet ./...
	go test -race ./...
