SOURCE_OS ?= $(shell uname -s | tr '[:upper:]' '[:lower:]')
SOURCE_ARCH ?= $(shell uname -m)
TARGET_OS ?= $(SOURCE_OS)
TARGET_ARCH ?= $(SOURCE_ARCH)
normalize_arch = $(if $(filter aarch64,$(1)),arm64,$(if $(filter x86_64,$(1)),amd64,$(1)))
# Normalize the source and target arch to arm64 or amd64 for compatibility with go build.
SOURCE_ARCH := $(call normalize_arch,$(SOURCE_ARCH))
TARGET_ARCH := $(call normalize_arch,$(TARGET_ARCH))

# Here we will handle error cases where the host/target combinations are not supported.
SUPPORTED_COMBINATIONS := \
    linux-arm64-linux-arm64 \
    linux-amd64-linux-amd64 \
    linux-amd64-android-arm64 \
    darwin-arm64-darwin-arm64 \
    darwin-arm64-android-arm64
CURRENT_COMBINATION := $(SOURCE_OS)-$(SOURCE_ARCH)-$(TARGET_OS)-$(TARGET_ARCH)
ifneq (,$(filter $(CURRENT_COMBINATION),$(SUPPORTED_COMBINATIONS)))
    $(info Supported combination: $(CURRENT_COMBINATION))
else
    $(error Error: Unsupported combination: $(CURRENT_COMBINATION))
endif

ifeq ($(SOURCE_OS),linux)
    NPROC ?= $(shell nproc)
else ifeq ($(SOURCE_OS),darwin)
    NPROC ?= $(shell sysctl -n hw.ncpu)
else
    NPROC ?= 1
endif

BIN_OUTPUT_PATH = bin/$(TARGET_OS)-$(TARGET_ARCH)
TOOL_BIN = bin/gotools/$(shell uname -s)-$(shell uname -m)

FFMPEG_TAG ?= n6.1
FFMPEG_VERSION ?= $(shell pwd)/FFmpeg/$(FFMPEG_TAG)
FFMPEG_VERSION_PLATFORM ?= $(FFMPEG_VERSION)/$(TARGET_OS)-$(TARGET_ARCH)
FFMPEG_BUILD ?= $(FFMPEG_VERSION_PLATFORM)/build
FFMPEG_OPTS ?= --prefix=$(FFMPEG_BUILD) \
               --enable-static \
               --disable-shared \
               --disable-programs \
               --disable-doc \
               --disable-everything \
               --enable-decoder=h264 \
               --enable-decoder=hevc \
               --enable-network \
               --enable-parser=h264 \
               --enable-parser=hevc

CGO_LDFLAGS := -L$(FFMPEG_BUILD)/lib
CGO_CFLAGS := -I$(FFMPEG_BUILD)/include
export PKG_CONFIG_PATH=$(FFMPEG_BUILD)/lib/pkgconfig

# If we are building for android, we need to set the correct flags
# and toolchain paths for FFMPEG and go binary cross-compilation.
ifeq ($(TARGET_OS),android)
# amd64 android targets have not been tested, so we do not support them for now.
ifeq ($(TARGET_ARCH),arm64)
    # Android build doesn't support most of our cgo libraries, so we use the no_cgo flag.
    GO_TAGS ?= -tags no_cgo
    # We need the go build command to think it's in cgo mode to support android NDK cross-compilation.
    export CGO_ENABLED = 1
    NDK_ROOT ?= $(shell pwd)/ndk/$(SOURCE_OS)/android-ndk-r26
    # We do not need to handle source arch for toolchain paths.
    # On darwin host, android toolchain binaries and libs are mach-O universal
    # with 2 architecture targets: x86_64 and arm64.
    CC = $(NDK_ROOT)/toolchains/llvm/prebuilt/$(SOURCE_OS)-x86_64/bin/aarch64-linux-android$(API_LEVEL)-clang
    export CC
    # Android API level is an integer value that uniquely identifies the revision of the Android platform framework API.
    # We use API level 30 as the default value. You can change it by setting the API_LEVEL variable.
    API_LEVEL ?= 30
    FFMPEG_OPTS += --target-os=android \
                   --arch=aarch64 \
                   --cpu=armv8-a \
                   --enable-cross-compile \
                   --cc=$(CC)
endif
endif

ifeq ($(TARGET_OS),linux)
	CGO_LDFLAGS := "$(CGO_LDFLAGS) -l:libjpeg.a"
endif

# Default values integration testing with ffmpeg
FFMPEG_ARGS ?= "libx264"
MEDIAMTX_ARCH ?= armv7
VIAM_SERVER_ARCH ?= aarch64

.PHONY: build-ffmpeg tool-install gofmt lint test update-rdk module integration-test clean clean-all

# We set GOOS, GOARCH, and GO_TAGS to support cross-compilation for android targets.
$(BIN_OUTPUT_PATH)/viamrtsp: build-ffmpeg *.go cmd/module/*.go
	CGO_LDFLAGS=$(CGO_LDFLAGS) \
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build $(GO_TAGS) -o $(BIN_OUTPUT_PATH)/viamrtsp cmd/module/cmd.go

tool-install:
	GOBIN=`pwd`/$(TOOL_BIN) go install \
		github.com/edaniels/golinters/cmd/combined \
		github.com/golangci/golangci-lint/cmd/golangci-lint \
		github.com/rhysd/actionlint/cmd/actionlint

gofmt:
	gofmt -w -s .

lint: gofmt tool-install build-ffmpeg
	go mod tidy
	export pkgs="`go list -f '{{.Dir}}' ./...`" && echo "$$pkgs" | xargs go vet -vettool=$(TOOL_BIN)/combined
	GOGC=50 $(TOOL_BIN)/golangci-lint run -v --fix --config=./etc/.golangci.yaml

test: build-ffmpeg
	CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) go test -race -v ./...

update-rdk:
	go get go.viam.com/rdk@latest
	go mod tidy

$(FFMPEG_VERSION_PLATFORM):
	git clone https://github.com/FFmpeg/FFmpeg.git --depth 1 --branch $(FFMPEG_TAG) $(FFMPEG_VERSION_PLATFORM)

$(FFMPEG_BUILD): $(FFMPEG_VERSION_PLATFORM)
	cd $(FFMPEG_VERSION_PLATFORM) && ./configure $(FFMPEG_OPTS) && $(MAKE) -j$(NPROC) && $(MAKE) install

build-ffmpeg: $(NDK_ROOT)
# Only need nasm to build assembly kernels for amd64 targets.
ifeq ($(SOURCE_OS),linux)
ifeq ($(SOURCE_ARCH),amd64)
	which nasm || (sudo apt update && sudo apt install -y nasm)
endif
endif
	$(MAKE) $(FFMPEG_BUILD)

# Warning: This will download a large file (1.5GB) and extract the contents. If you have 
# already downloaded the NDK, you can set the NDK_ROOT variable to the path of the NDK.
$(NDK_ROOT):
ifeq ($(TARGET_OS),android)
ifeq ($(SOURCE_OS),darwin)
	wget https://dl.google.com/android/repository/android-ndk-r26d-darwin.dmg && \
	hdiutil attach android-ndk-r26d-darwin.dmg && \
	mkdir -p $(NDK_ROOT) && \
	cp -R "/Volumes/Android NDK r26d"/AndroidNDK11579264.app/Contents/NDK/* $(NDK_ROOT) && \
	hdiutil detach "/Volumes/Android NDK r26d" && \
	rm android-ndk-r26d-darwin.dmg
endif
ifeq ($(SOURCE_OS),linux)
ifeq ($(SOURCE_ARCH),amd64)
	sudo apt update && sudo apt install -y unzip && \
	wget https://dl.google.com/android/repository/android-ndk-r26-linux.zip && \
	mkdir -p $(dir $(NDK_ROOT)) && \
	yes A | unzip android-ndk-r26-linux.zip -d $(dir $(NDK_ROOT)) && \
	rm android-ndk-r26-linux.zip
endif
endif
endif

module: $(BIN_OUTPUT_PATH)/viamrtsp
	cp $(BIN_OUTPUT_PATH)/viamrtsp bin/viamrtsp
	tar czf module.tar.gz bin/viamrtsp
	rm bin/viamrtsp

integration-test: $(BIN_OUTPUT_PATH)/viamrtsp
	echo "Downloading and setting up MediaMTX..."
	wget https://github.com/bluenviron/mediamtx/releases/download/v1.6.0/mediamtx_v1.6.0_linux_$(MEDIAMTX_ARCH).tar.gz && \
	tar -xzf mediamtx_v1.6.0_linux_$(MEDIAMTX_ARCH).tar.gz && \
    chmod +x ./mediamtx && \
	./mediamtx &

	echo "Starting RTSP stream..."
	ffmpeg -re -f lavfi -i testsrc=size=640x480:rate=30 -vcodec $(FFMPEG_ARGS) -pix_fmt yuv420p -f rtsp -rtsp_transport tcp rtsp://0.0.0.0:8554/live.stream &

	echo "Downloading and installing Viam server..."
	curl https://storage.googleapis.com/packages.viam.com/apps/viam-server/viam-server-stable-$(VIAM_SERVER_ARCH).AppImage -o viam-server && \
	chmod 755 viam-server && \
	sudo ./viam-server --aix-install

	echo "Generating configuration..."
	echo "{" > integration-test-config.json
	echo "  \"components\": [" >> integration-test-config.json
	echo "    {" >> integration-test-config.json
	echo "      \"name\": \"ip-cam\"," >> integration-test-config.json
	echo "      \"namespace\": \"rdk\"," >> integration-test-config.json
	echo "      \"type\": \"camera\"," >> integration-test-config.json
	echo "      \"model\": \"erh:viamrtsp:rtsp\"," >> integration-test-config.json
	echo "      \"attributes\": {" >> integration-test-config.json
	echo "        \"rtsp_address\": \"rtsp://localhost:8554/live.stream\"" >> integration-test-config.json
	echo "      }," >> integration-test-config.json
	echo "      \"depends_on\": []" >> integration-test-config.json
	echo "    }" >> integration-test-config.json
	echo "  ]," >> integration-test-config.json
	echo "  \"modules\": [" >> integration-test-config.json
	echo "    {" >> integration-test-config.json
	echo "      \"type\": \"local\"," >> integration-test-config.json
	echo "      \"name\": \"viamrtsp\"," >> integration-test-config.json
	echo "      \"executable_path\": \"$(realpath $(BIN_OUTPUT_PATH)/viamrtsp)\"" >> integration-test-config.json
	echo "    }" >> integration-test-config.json
	echo "  ]" >> integration-test-config.json
	echo "}" >> integration-test-config.json

	echo "Launching Viam server with configuration..."
	viam-server -debug -config "./integration-test-config.json" &
	sleep 10

	echo "Compiling and running test binary..."
	go build -o testBinary ./test/client.go
	chmod +x testBinary
	./testBinary

clean:
	rm -rf $(BIN_OUTPUT_PATH)/viamrtsp module.tar.gz

clean-all:
	rm -rf FFmpeg
	git clean -fxd
