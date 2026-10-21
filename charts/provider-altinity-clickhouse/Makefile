HELM ?= helm

# values to override
IMAGE_PREFIX ?= ghcr.io/openeverest
PROVIDER_IMAGE_NAME ?= provider-altinity-clickhouse
VERSION ?= 0.1.0
CHART_VERSION ?= $(VERSION)

.PHONY: prepare-chart
prepare-chart:
	yq eval -i '.version = "$(CHART_VERSION)"' Chart.yaml
	yq eval -i '.appVersion = "$(VERSION)"' Chart.yaml
	yq eval -i '.image.repository = "$(IMAGE_PREFIX)/$(PROVIDER_IMAGE_NAME)"' values.yaml
	yq eval -i '.image.tag = "$(VERSION)"' values.yaml
	# The Provider resource reports this as its release; provider-sdk generate
	# took it from the appVersion at generation time, before this stamping.
	yq eval -i '.spec.release.version = "$(VERSION)"' generated/provider-spec.yaml
	test "$$(yq eval '.spec.release.version' generated/provider-spec.yaml)" = "$(VERSION)"
	$(MAKE) deps

.PHONY: release
release: prepare-chart

.PHONY: release-dev
release-dev: PROVIDER_IMAGE_NAME := provider-altinity-clickhouse-dev
release-dev: prepare-chart

.PHONY: deps
deps:
	$(HELM) version
	$(HELM) repo add altinity https://helm.altinity.com/
	$(HELM) dependency update .
