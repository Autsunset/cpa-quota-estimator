VERSION ?= 0.1.0
OUT := dist/cpa-quota-estimator-v$(VERSION).so

.PHONY: build test clean

build:
	@mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags="-s -w" -o $(OUT) .

test:
	go test ./...

clean:
	rm -rf dist
