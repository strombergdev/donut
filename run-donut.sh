#!/bin/bash

# Set SRT library paths
export SRT_FOLDER="/opt/srt_lib"
FFMPEG_PATH="/Users/max/src/go-astiav/tmp/n5.1.4"

export CGO_CFLAGS="-I${SRT_FOLDER}/include -I${FFMPEG_PATH}/include/"
export CGO_LDFLAGS="-L${SRT_FOLDER}/lib -L${FFMPEG_PATH}/lib/"
export PKG_CONFIG_PATH="${FFMPEG_PATH}/lib/pkgconfig"

# Set library paths
export LD_LIBRARY_PATH="${SRT_FOLDER}/lib:${FFMPEG_PATH}/lib:$LD_LIBRARY_PATH"
export DYLD_LIBRARY_PATH="${SRT_FOLDER}/lib:${FFMPEG_PATH}/lib:$DYLD_LIBRARY_PATH"
export DYLD_FALLBACK_LIBRARY_PATH="${SRT_FOLDER}/lib:${FFMPEG_PATH}/lib:$DYLD_FALLBACK_LIBRARY_PATH"

# Debug: Print library locations
echo "Checking for FFmpeg libraries..."
ls -l ${FFMPEG_PATH}/lib/libavdevice*

# Run the Go program with modified linker flags
go run -ldflags "-X github.com/asticode/go-astiav.ffmpegPath=${FFMPEG_PATH}/lib" main.go "$@"