UNAME_S ?= $(shell uname -s)
UNAME_M ?= $(shell uname -m)
FFMPEG_PREFIX ?= $(shell pwd)/ffmpeg/$(UNAME_S)-$(UNAME_M)
FFMPEG_OPTS ?= --prefix=$(FFMPEG_PREFIX) \
               --enable-static \
               --disable-shared \
               --disable-programs \
               --disable-doc \
               --disable-everything \
               --enable-decoder=h264 \
               --enable-decoder=hevc \
               --enable-swscale
CGO_LDFLAGS := -L$(FFMPEG_PREFIX)/lib
ifeq ($(UNAME_S),Linux)
	CGO_LDFLAGS := "$(CGO_LDFLAGS) -l:libjpeg.a"
endif

.PHONY: build-ffmpeg test lint updaterdk module

bin/viamrtsp: build-ffmpeg *.go cmd/module/*.go
	PKG_CONFIG_PATH=$(FFMPEG_PREFIX)/lib/pkgconfig \
		CGO_LDFLAGS=$(CGO_LDFLAGS) \
		go build -o bin/viamrtsp cmd/module/cmd.go

test:
	go test

lint:
	gofmt -w -s .

updaterdk:
	go get go.viam.com/rdk@latest
	go mod tidy

FFmpeg:
	git submodule init && git submodule update

$(FFMPEG_PREFIX): FFmpeg
	cd FFmpeg && ./configure $(FFMPEG_OPTS) && $(MAKE) -j$(shell nproc) && $(MAKE) install

build-ffmpeg:
ifeq ($(UNAME_S),Linux)
ifeq ($(UNAME_M),x86_64)
	which nasm || (sudo apt update && sudo apt install -y nasm)
endif
endif
	$(MAKE) $(FFMPEG_PREFIX)

module: bin/viamrtsp
	tar czf module.tar.gz bin/viamrtsp
