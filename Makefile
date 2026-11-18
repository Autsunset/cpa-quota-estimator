PLUGIN_ID := cpa-quota-estimator
VERSION ?= 0.23.0
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

# Portable C baseline. The toolchain's default -march on some builders includes
# BMI2/AVX2, and the cgo parts (runtime/cgo) are linked into the plugin: a single
# `shlx` in x_cgo_sigaction made the whole library die with SIGILL on a host CPU
# without BMI2. Keep the C side at the x86-64 baseline for amd64 builds.
ifeq ($(GOARCH),amd64)
CGO_ARCH_FLAGS := -march=x86-64 -mtune=generic
endif

.PHONY: build package test clean

build:
	@mkdir -p dist
	CGO_ENABLED=1 CGO_CFLAGS="$${CGO_CFLAGS:--O2 -g} $(CGO_ARCH_FLAGS)" go build -trimpath -buildvcs=false -buildmode=c-shared \
		-ldflags="-s -w -X main.pluginVersion=$(VERSION)" -o $(OUT) .

package:
	PLUGIN_VERSION=$(VERSION) ./package-release.sh dist

test:
	go test ./...

clean:
	rm -rf dist
