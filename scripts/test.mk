ifeq ($(shell getconf LONG_BIT),64)
  RACE=-race
endif

test-internal:
	go generate ./...
	go test -v $(RACE) -coverprofile=coverage-internal.txt \
	$$(go list ./internal/... | grep -v /core)

test-core:
	go test -v $(RACE) -coverprofile=coverage-core.txt ./internal/core

test-nodocker: test-internal test-core

# libmxl is published for 64-bit glibc only, so three packages cannot be built
# for a 32-bit target: the MXL static source, the static source registry that
# imports it, and internal/core through that registry. The sibling static
# sources do not reach it and stay covered.
#
# go list -e because those three fail to load without libmxl, which would
# otherwise abort the listing before the filter runs.
test-internal-32:
	go generate ./...
	go test -v -coverprofile=coverage-internal.txt \
	$$(go list -e ./internal/... | grep -Ev '/internal/core$$|/internal/staticsources$$|/internal/staticsources/mxl$$')

test-nodocker-32: test-internal-32

define DOCKERFILE_TEST
ARG ARCH
FROM $$ARCH/$(BASE_IMAGE)
RUN apk add --no-cache make gcc musl-dev
WORKDIR /s
COPY go.mod go.sum ./
RUN go mod download
endef
export DOCKERFILE_TEST

test:
	echo "$$DOCKERFILE_TEST" | docker build -q . -f - -t temp --build-arg ARCH=amd64
	docker run --rm \
	-v "$(shell pwd):/s" \
	temp \
	make test-nodocker

test-32:
	echo "$$DOCKERFILE_TEST" | docker build -q . -f - -t temp --build-arg ARCH=i386
	docker run --rm \
	-v "$(shell pwd):/s" \
	temp \
	make test-nodocker-32
