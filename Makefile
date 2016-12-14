CC = cc
CXX = c++
STRIP = strip

PLATFORMS = \
	android-arm \
	android-x64 \
	android-x86 \
	darwin-x64 \
	linux-arm \
	linux-armv7 \
	linux-arm64 \
	linux-x64 \
	linux-x86 \
	windows-x64 \
	windows-x86

include platform_host.mk

ifneq ($(CROSS_TRIPLE),)
	CC := $(CROSS_TRIPLE)-$(CC)
	CXX := $(CROSS_TRIPLE)-$(CXX)
	STRIP := $(CROSS_TRIPLE)-strip
endif

include platform_target.mk

ifeq ($(TARGET_ARCH),x86)
	GOARCH = 386
else ifeq ($(TARGET_ARCH),x64)
	GOARCH = amd64
else ifeq ($(TARGET_ARCH),arm)
	GOARCH = arm
	GOARM = 6
else ifeq ($(TARGET_ARCH), armv7)
	GOARCH = arm
	GOARM = 7
	PKGDIR = -pkgdir /go/pkg/linux_armv7
else ifeq ($(TARGET_ARCH), arm64)
	GOARCH = arm64
	GOARM =
endif

BUILDMODE = default
ifeq ($(TARGET_OS), windows)
	EXT = .exe
	GOOS = windows
	LDFLAGS := $(LDFLAGS) -linkmode=external -extld=$(CC) '-extldflags=-lstdc++ -static'
else ifeq ($(TARGET_OS), darwin)
	EXT =
	GOOS = darwin
	LDFLAGS := $(LDFLAGS) -linkmode=external -extld=$(CC) -extldflags=-lstdc++
else ifeq ($(TARGET_OS), linux)
	EXT =
	GOOS = linux
	LDFLAGS := $(LDFLAGS) -linkmode=external -extld=$(CC) -extldflags=-lstdc++ -extldflags=-lrt
else ifeq ($(TARGET_OS), android)
	EXT =
	GOOS = android
	LDFLAGS := $(LDFLAGS) -linkmode=external -extld=$(CC) -extldflags=-lstdc++
	ifeq ($(TARGET_ARCH), arm)
		GOARM = 7
	else
		GOARM =
	endif
	BUILDMODE = pie
endif

DOCKER = docker
DOCKER_IMAGE = quasarhq/libtorrent-go
GO_PACKAGE_NS = github.com/afedchin
NAME = torrent2http
GO = go
UPX = upx
CGO_ENABLED = 1
GIT = git
GIT_VERSION = $(shell $(GIT) describe --tags)
OUTPUT_NAME = $(NAME)$(EXT)
BUILD_PATH = build/$(TARGET_OS)_$(TARGET_ARCH)
LIBTORRENT_GO = github.com/scakemyer/libtorrent-go
LIBTORRENT_GO_HOME = $(shell go env GOPATH)/src/$(LIBTORRENT_GO)

.PHONY: $(PLATFORMS)

all: $(PLATFORMS)

$(PLATFORMS):
	$(MAKE) build TARGET_OS=$(firstword $(subst -, ,$@)) TARGET_ARCH=$(word 2, $(subst -, ,$@))

libtorrent-go: 
	$(MAKE) -C $(LIBTORRENT_GO_HOME) $(PLATFORM)

$(BUILD_PATH):
	mkdir -p $(BUILD_PATH)

$(BUILD_PATH)/$(OUTPUT_NAME): $(BUILD_PATH)
	CC=$(CC) \
	GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM=$(GOARM) \
	CGO_ENABLED=$(CGO_ENABLED) \
	$(GO) build -v -x \
		-o $(BUILD_PATH)/$(OUTPUT_NAME) \
		-ldflags="$(LDFLAGS)" \
		$(PKGDIR)

vendor_darwin vendor_linux:

vendor_windows:
	find $(shell go env GOPATH)/pkg/$(GOOS)_$(GOARCH) -name *.dll -exec cp -f {} $(BUILD_PATH) \;

vendor_android:
	cp $(CROSS_ROOT)/$(CROSS_TRIPLE)/lib/libgnustl_shared.so $(BUILD_PATH)
	chmod +rx $(BUILD_PATH)/libgnustl_shared.so

t2h: $(BUILD_PATH)/$(OUTPUT_NAME)

re: clean build

clean:
	rm -rf $(BUILD_PATH)

distclean:
	rm -rf build

build:
	$(DOCKER) run --rm -v $(GOPATH):/go -e GOPATH=/go -v $(shell pwd):/go/src/$(GO_PACKAGE_NS)/$(NAME) -w /go/src/$(GO_PACKAGE_NS)/$(NAME) $(DOCKER_IMAGE):$(TARGET_OS)-$(TARGET_ARCH) make dist TARGET_OS=$(TARGET_OS) TARGET_ARCH=$(TARGET_ARCH) GIT_VERSION=$(GIT_VERSION)

strip:
	@find $(BUILD_PATH) -type f ! -name "*.xxx" -exec $(STRIP) {} \;

upx:
# Do not .exe files, as upx doesn't really work with 8l/6l linked files.
# It's fine for other platforms, because we link with an external linker, namely
# GCC or Clang. However, on Windows this feature is not yet supported.
	@find $(BUILD_PATH) -type f ! -name "*.exe" -a ! -name "*.so" -exec $(UPX) --lzma {} \;

checksum: $(BUILD_PATH)/$(OUTPUT_NAME)
	shasum -b $(BUILD_PATH)/$(OUTPUT_NAME) | cut -d' ' -f1 >> $(BUILD_PATH)/$(OUTPUT_NAME)

ifeq ($(TARGET_ARCH), arm)
dist: t2h vendor_$(TARGET_OS) strip checksum
else ifeq ($(TARGET_OS), darwin)
dist: t2h vendor_$(TARGET_OS) strip checksum
else
dist: t2h vendor_$(TARGET_OS) strip upx checksum
endif

libs:
	$(MAKE) libtorrent-go PLATFORM=$(PLATFORM)

binaries:
	$(GIT) config --global push.default simple
	$(GIT) clone --depth=1 https://github.com/afedchin/t2h-binaries binaries
	cp -Rf build/* binaries/
	cd binaries && git add * && $(GIT) commit -m "Update to ${GIT_VERSION}"

pull:
	docker pull $(DOCKER_IMAGE):$(PLATFORM)
	docker tag $(DOCKER_IMAGE):$(PLATFORM) libtorrent-go:$(PLATFORM)
