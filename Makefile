VERSION       := $(shell git describe --tags --always --dirty="-dev")
DATE          := $(shell date -u '+%Y-%m-%d-%H%M UTC')
VERSION_FLAGS := -ldflags='-X "main.Version=$(VERSION)" -X "main.BuildTime=$(DATE)"'

IMPORT_PATH   := github.com/cloudflare/cloudflared
PACKAGE_DIR   := $(CURDIR)/packaging
INSTALL_BINDIR := /usr/bin/
MAN_DIR := /usr/share/man/man1/

EQUINOX_FLAGS = --version="$(VERSION)" \
	--platforms="$(EQUINOX_BUILD_PLATFORMS)" \
	--app="$(EQUINOX_APP_ID)" \
	--token="$(EQUINOX_TOKEN)" \
	--channel="$(EQUINOX_CHANNEL)"

ifeq ($(EQUINOX_IS_DRAFT), true)
	EQUINOX_FLAGS := --draft $(EQUINOX_FLAGS)
endif

LOCAL_ARCH ?= $(shell uname -m)
ifneq ($(GOARCH),)
    TARGET_ARCH ?= $(GOARCH)
else ifeq ($(LOCAL_ARCH),x86_64)
    TARGET_ARCH ?= amd64
else ifeq ($(shell echo $(LOCAL_ARCH) | head -c 5),armv8)
    TARGET_ARCH ?= arm64
else ifeq ($(LOCAL_ARCH),aarch64)
    TARGET_ARCH ?= arm64
else ifeq ($(shell echo $(LOCAL_ARCH) | head -c 4),armv)
    TARGET_ARCH ?= arm
else
    $(error This system's architecture $(LOCAL_ARCH) isn't supported)
endif

LOCAL_OS ?= $(shell go env GOOS)
ifeq ($(LOCAL_OS),linux)
    TARGET_OS ?= linux
else ifeq ($(LOCAL_OS),darwin)
    TARGET_OS ?= darwin
else ifeq ($(LOCAL_OS),windows)
    TARGET_OS ?= windows
else
    $(error This system's OS $(LOCAL_OS) isn't supported)
endif

ifeq ($(TARGET_OS), windows)
	EXECUTABLE_PATH=./cloudflared.exe
else
	EXECUTABLE_PATH=./cloudflared
endif

ifeq ($(FLAVOR), centos-7)
	TARGET_PUBLIC_REPO ?= el7
else
	TARGET_PUBLIC_REPO ?= $(FLAVOR)
endif

.PHONY: all
all: cloudflared test

.PHONY: clean
clean:
	go clean

.PHONY: cloudflared
cloudflared: tunnel-deps
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build -v -mod=vendor $(VERSION_FLAGS) $(IMPORT_PATH)/cmd/cloudflared

.PHONY: container
container:
	docker build --build-arg=TARGET_ARCH=$(TARGET_ARCH) --build-arg=TARGET_OS=$(TARGET_OS) -t cloudflare/cloudflared-$(TARGET_OS)-$(TARGET_ARCH):"$(VERSION)" .

.PHONY: test
test: vet
	go test -v -mod=vendor -race $(VERSION_FLAGS) ./...

.PHONY: test-ssh-server
test-ssh-server:
	docker-compose -f ssh_server_tests/docker-compose.yml up

define publish_package
	for HOST in $(CF_PKG_HOSTS); do \
		ssh-keyscan -t rsa $$HOST >> ~/.ssh/known_hosts; \
		scp -4 cloudflared_$(VERSION)_amd64.deb cfsync@$$HOST:/state/cf-pkg/staging/$(1)/$(TARGET_PUBLIC_REPO)/cloudflared/; \
	done
endef

.PHONY: publish-deb
publish-deb: cloudflared-deb
	$(call publish_package,apt)

.PHONY: publish-rpm
publish-rpm: cloudflared-rpm
	$(call publish_package,yum)

define build_package
	mkdir -p $(PACKAGE_DIR)
	cp cloudflared $(PACKAGE_DIR)/cloudflared
	cat cloudflared_man_template | sed -e 's/\$${VERSION}/$(VERSION)/; s/\$${DATE}/$(DATE)/' > $(PACKAGE_DIR)/cloudflared.1
	fakeroot fpm -C $(PACKAGE_DIR) -s dir -t $(1) --$(1)-compression bzip2 \
		-a $(TARGET_ARCH) -v $(VERSION) -n cloudflared --after-install postinst.sh --after-remove postrm.sh \
		cloudflared=$(INSTALL_BINDIR) cloudflared.1=$(MAN_DIR)
endef

.PHONY: cloudflared-deb
cloudflared-deb: cloudflared
	$(call build_package,deb)

.PHONY: cloudflared-rpm
cloudflared-rpm: cloudflared
	$(call build_package,rpm)

.PHONY: cloudflared-darwin-amd64.tgz
cloudflared-darwin-amd64.tgz: cloudflared
	tar czf cloudflared-darwin-amd64.tgz cloudflared
	rm cloudflared

.PHONY: homebrew-upload
homebrew-upload: cloudflared-darwin-amd64.tgz
	aws s3 --endpoint-url $(S3_ENDPOINT) cp --acl public-read $$^ $(S3_URI)/cloudflared-$$(VERSION)-$1.tgz
	aws s3 --endpoint-url $(S3_ENDPOINT) cp --acl public-read $(S3_URI)/cloudflared-$$(VERSION)-$1.tgz  $(S3_URI)/cloudflared-stable-$1.tgz

.PHONY: homebrew-release
homebrew-release: homebrew-upload
	./publish-homebrew-formula.sh cloudflared-darwin-amd64.tgz $(VERSION) homebrew-cloudflare

.PHONY: release
release: bin/equinox
	bin/equinox release $(EQUINOX_FLAGS) -- $(VERSION_FLAGS) $(IMPORT_PATH)/cmd/cloudflared

.PHONY: github-release
github-release: cloudflared
	python3 github_release.py --path $(EXECUTABLE_PATH) --release-version $(VERSION)

bin/equinox:
	mkdir -p bin
	curl -s https://bin.equinox.io/c/75JtLRTsJ3n/release-tool-beta-$(EQUINOX_PLATFORM).tgz | tar xz -C bin/

.PHONY: tunnel-deps
tunnel-deps: tunnelrpc/tunnelrpc.capnp.go

tunnelrpc/tunnelrpc.capnp.go: tunnelrpc/tunnelrpc.capnp
	which capnp  # https://capnproto.org/install.html
	which capnpc-go  # go get zombiezen.com/go/capnproto2/capnpc-go
	capnp compile -ogo tunnelrpc/tunnelrpc.capnp

.PHONY: vet
vet:
	go vet -mod=vendor ./...
	which go-sumtype  # go get github.com/BurntSushi/go-sumtype
	go-sumtype $$(go list -mod=vendor ./...)
