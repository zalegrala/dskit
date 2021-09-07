IMAGE_PREFIX ?= quay.io/cortexproject/
BUILD_IMAGE ?= $(IMAGE_PREFIX)build-image
TTY := --tty
BUILD_IN_CONTAINER := true
SUDO := $(shell docker info >/dev/null 2>&1 || echo "sudo -E")

# put tools at the root of the folder
PATH := $(CURDIR)/.tools/bin:$(PATH)

DONT_FIND := -name lib -prune -o -name .git -prune -o -name .cache -prune -o -name .pkg -prune -o -name .tools -prune -o

# Generating proto code is automated.
PROTO_DEFS := $(shell find . $(DONT_FIND) -type f -name '*.proto' -print)
PROTO_GOS := $(patsubst %.proto,%.pb.go,$(PROTO_DEFS))

# Manually declared dependencies
dskitpb/dskit.pb.go: dskitpb/dskit.proto
ruler/rulespb/rules.pb.go: ruler/rulespb/rules.proto
ruler/ruler.pb.go: ruler/ruler.proto
ring/ring.pb.go: ring/ring.proto
kv/memberlist/kv.pb.go: kv/memberlist/kv.proto
chunk/grpc/grpc.pb.go: chunk/grpc/grpc.proto
chunk/storage/caching_index_client.pb.go: chunk/storage/caching_index_client.proto
chunk/purger/delete_plan.pb.go: chunk/purger/delete_plan.proto

ifeq ($(BUILD_IN_CONTAINER),true)

GOVOLUMES=	-v $(shell pwd)/.cache:/go/cache:delegated,z \
			-v $(shell pwd)/.pkg:/go/pkg:delegated,z \
			-v $(shell pwd):/go/src/github.com/cortexproject/cortex:delegated,z

.PHONY: protos
protos $(PROTO_GOS):
	@mkdir -p .pkg
	@mkdir -p .cache
	@echo
	@echo ">>>> Entering build container: $@"
	$(SUDO) time docker run --rm $(TTY) -i $(GOVOLUMES) $(BUILD_IMAGE) $@;

else

.PHONY: protos
protos: $(PROTO_GOS)

%.pb.go:
	protoc -I $(GOPATH)/src:./lib/github.com/gogo/protobuf:./lib:./:./$(@D) --gogoslick_out=plugins=grpc,Mgoogle/protobuf/any.proto=github.com/gogo/protobuf/types,:./$(@D) ./$(patsubst %.pb.go,%.proto,$@)

endif

.PHONY: check-protos
check-protos: clean-protos protos
	@git diff --exit-code -- $(PROTO_GOS)

.PHONY: clean-protos
clean-protos:
	rm -rf $(PROTO_GOS)

.PHONY: test
test: protos
	go test -tags netgo -timeout 30m -race -count 1 $(shell go list ./... | grep -v /lib/)

.PHONY: lint
lint: .tools/bin/misspell .tools/bin/faillint .tools/bin/golangci-lint protos
	misspell -error README.md CONTRIBUTING.md LICENSE

	# Configured via .golangci.yml.
	golangci-lint run

	# Ensure no blocklisted package is imported.
	faillint -paths "github.com/bmizerany/assert=github.com/stretchr/testify/assert,\
		golang.org/x/net/context=context,\
		sync/atomic=go.uber.org/atomic,\
		github.com/prometheus/client_golang/prometheus.{MustRegister}=github.com/prometheus/client_golang/prometheus/promauto,\
		github.com/prometheus/client_golang/prometheus.{NewCounter,NewCounterVec,NewGauge,NewGaugeVec,NewGaugeFunc,NewHistogram,NewHistogramVec,NewSummary,NewSummaryVec}\
		=github.com/prometheus/client_golang/prometheus/promauto.With.{NewCounter,NewCounterVec,NewGauge,NewGaugeVec,NewGaugeFunc,NewHistogram,NewHistogramVec,NewSummary,NewSummaryVec}"\
		./...

.PHONY: clean
clean:
	@# go mod makes the modules read-only, so before deletion we need to make them deleteable
	@chmod -R u+rwX .tools 2> /dev/null || true
	rm -rf .tools/

.PHONY: mod-check
mod-check:
	GO111MODULE=on go mod download
	GO111MODULE=on go mod verify
	GO111MODULE=on go mod tidy
	@git diff --exit-code -- go.sum go.mod

.tools:
	mkdir -p .tools/

.tools/bin/misspell: .tools
	GOPATH=$(CURDIR)/.tools go install github.com/client9/misspell/cmd/misspell@v0.3.4

.tools/bin/faillint: .tools
	GOPATH=$(CURDIR)/.tools go install github.com/fatih/faillint@v1.5.0

.tools/bin/golangci-lint: .tools
	GOPATH=$(CURDIR)/.tools go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.41.1

drone: .drone/drone.yml

.drone/drone.yml: .drone/drone.jsonnet
	# Drones jsonnet formatting causes issues where arrays disappear
	drone jsonnet --source $< --target $@.tmp --stream --format=false
	drone sign --save grafana/dskit $@.tmp
	drone lint --trusted $@.tmp
	# When all passes move to correct destination
	mv $@.tmp $@
