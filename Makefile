PLUGIN_ID := cpa-quota-estimator
VERSION ?= 0.4.5
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
EXT := so
ifeq ($(GOOS),darwin)
EXT := dylib
endif
ifeq ($(GOOS),windows)
EXT := dll
endif
OUT := dist/$(PLUGIN_ID).$(EXT)

.PHONY: build package test clean

build:
	@mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared \
		-ldflags="-s -w -X main.pluginVersion=$(VERSION)" -o $(OUT) .

package:
	PLUGIN_VERSION=$(VERSION) ./package-release.sh dist

test:
	go test ./...

clean:
	rm -rf dist
