APP_NAME := aadapter
VERSION ?= dev

.PHONY: test build build-all

test:
	go test ./...

build:
	go build -trimpath -ldflags "-s -w" -o bin/$(APP_NAME) .

build-all:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(APP_NAME)_$(VERSION)_linux_amd64 .
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(APP_NAME)_$(VERSION)_linux_arm64 .
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(APP_NAME)_$(VERSION)_windows_amd64.exe .
