package viamrtsp

import (
	"fmt"
	"image"
	"unsafe"

	"github.com/pkg/errors"
)

/*
#cgo pkg-config: libavcodec libavutil libswscale libavformat
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libswscale/swscale.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
#include <stdlib.h>

// get_video_codec checks the provided AVFormatContext to find a supported video codec.
// It prioritizes H264 over H265 if both are found.
// If no supported codec is identified, it returns AV_CODEC_ID_NONE.
int get_video_codec(AVFormatContext *avFormatCtx) {
    if (avFormatCtx == NULL) {
        return AV_CODEC_ID_NONE;
    }
    int found_h265 = 0;
    for (int i = 0; i < avFormatCtx->nb_streams; i++) {
        AVStream *stream = avFormatCtx->streams[i];
        if (stream == NULL) {
            continue;
        }
        AVCodecParameters *codecParams = stream->codecpar;
        if (codecParams == NULL) {
            continue;
        }
        if (codecParams->codec_id == AV_CODEC_ID_H264) {
            return AV_CODEC_ID_H264;
        } else if (codecParams->codec_id == AV_CODEC_ID_H265) {
            found_h265 = 1;
        }
    }
    if (found_h265) {
        return AV_CODEC_ID_H265;
    }
    return AV_CODEC_ID_NONE;
}
*/
import "C"

// Decoder is a generic FFmpeg decoder.
type decoder struct {
	codecCtx    *C.AVCodecContext
	srcFrame    *C.AVFrame
	swsCtx      *C.struct_SwsContext
	dstFrame    *C.AVFrame
	dstFramePtr []uint8
}

type videoCodec int

const (
	Unknown videoCodec = iota
	H264
	H265
)

func frameData(frame *C.AVFrame) **C.uint8_t {
	return (**C.uint8_t)(unsafe.Pointer(&frame.data[0]))
}

func frameLineSize(frame *C.AVFrame) *C.int {
	return (*C.int)(unsafe.Pointer(&frame.linesize[0]))
}

// getStreamInfo opens a stream URL and retrieves the video codec.
func getStreamInfo(url string) (videoCodec, error) {
	cUrl := C.CString(url)
	defer C.free(unsafe.Pointer(cUrl))

	var avFormatCtx *C.AVFormatContext = nil
	ret := C.avformat_open_input(&avFormatCtx, cUrl, nil, nil)
	if ret < 0 {
		return Unknown, fmt.Errorf("avformat_open_input() failed: %s", avError(ret))
	}
	defer C.avformat_close_input(&avFormatCtx)

	ret = C.avformat_find_stream_info(avFormatCtx, nil)
	if ret < 0 {
		return Unknown, fmt.Errorf("avformat_find_stream_info() failed: %s", avError(ret))
	}

	cCodec := C.get_video_codec(avFormatCtx)
	codec := convertCodec(cCodec)

	if codec == Unknown {
		return Unknown, errors.New("no supported codec found")
	}
	return codec, nil
}

// convertCodec converts a C int to a Go videoCodec.
func convertCodec(cCodec C.int) videoCodec {
	switch cCodec {
	case C.AV_CODEC_ID_H264:
		return H264
	case C.AV_CODEC_ID_H265:
		return H265
	default:
		return Unknown
	}
}

// avError converts an AV error code to a AV error message string.
func avError(avErr C.int) string {
	var errbuf [C.AV_ERROR_MAX_STRING_SIZE]C.char
	if C.av_strerror(avErr, &errbuf[0], C.AV_ERROR_MAX_STRING_SIZE) < 0 {
		return fmt.Sprintf("Unknown error with code %d", avErr)
	}
	return C.GoString(&errbuf[0])
}

// SetLibAVLogLevelFatal sets libav errors to fatal log level
// to cut down on log spam
func SetLibAVLogLevelFatal() {
	C.av_log_set_level(C.AV_LOG_FATAL)
}

// newDecoder creates a new decoder for the given codec.
func newDecoder(codecID C.enum_AVCodecID) (*decoder, error) {
	codec := C.avcodec_find_decoder(codecID)
	if codec == nil {
		return nil, fmt.Errorf("avcodec_find_decoder() failed")
	}

	codecCtx := C.avcodec_alloc_context3(codec)
	if codecCtx == nil {
		return nil, fmt.Errorf("avcodec_alloc_context3() failed")
	}

	res := C.avcodec_open2(codecCtx, codec, nil)
	if res < 0 {
		C.avcodec_close(codecCtx)
		return nil, fmt.Errorf("avcodec_open2() failed")
	}

	srcFrame := C.av_frame_alloc()
	if srcFrame == nil {
		C.avcodec_close(codecCtx)
		return nil, fmt.Errorf("av_frame_alloc() failed")
	}

	return &decoder{
		codecCtx: codecCtx,
		srcFrame: srcFrame,
	}, nil
}

// newH264Decoder creates a new H264 decoder.
func newH264Decoder() (*decoder, error) {
	return newDecoder(C.AV_CODEC_ID_H264)
}

// newH265Decoder creates a new H265 decoder.
func newH265Decoder() (*decoder, error) {
	return newDecoder(C.AV_CODEC_ID_H265)
}

// close closes the decoder.
func (d *decoder) close() {
	if d.dstFrame != nil {
		C.av_frame_free(&d.dstFrame)
	}

	if d.swsCtx != nil {
		C.sws_freeContext(d.swsCtx)
	}

	C.av_frame_free(&d.srcFrame)
	C.avcodec_close(d.codecCtx)
}

func (d *decoder) decode(nalu []byte) (image.Image, error) {
	nalu = append([]uint8{0x00, 0x00, 0x00, 0x01}, []uint8(nalu)...)

	// send frame to decoder
	var avPacket C.AVPacket
	avPacket.data = (*C.uint8_t)(C.CBytes(nalu))
	defer C.free(unsafe.Pointer(avPacket.data))
	avPacket.size = C.int(len(nalu))
	res := C.avcodec_send_packet(d.codecCtx, &avPacket)
	if res < 0 {
		return nil, nil
	}

	// receive frame if available
	res = C.avcodec_receive_frame(d.codecCtx, d.srcFrame)
	if res < 0 {
		return nil, nil
	}

	// if frame size has changed, allocate needed objects
	if d.dstFrame == nil || d.dstFrame.width != d.srcFrame.width || d.dstFrame.height != d.srcFrame.height {
		if d.dstFrame != nil {
			C.av_frame_free(&d.dstFrame)
		}

		if d.swsCtx != nil {
			C.sws_freeContext(d.swsCtx)
		}

		d.dstFrame = C.av_frame_alloc()
		d.dstFrame.format = C.AV_PIX_FMT_RGBA
		d.dstFrame.width = d.srcFrame.width
		d.dstFrame.height = d.srcFrame.height
		d.dstFrame.color_range = C.AVCOL_RANGE_JPEG
		res = C.av_frame_get_buffer(d.dstFrame, 1)
		if res < 0 {
			return nil, fmt.Errorf("av_frame_get_buffer() err")
		}

		d.swsCtx = C.sws_getContext(d.srcFrame.width, d.srcFrame.height, C.AV_PIX_FMT_YUV420P,
			d.dstFrame.width, d.dstFrame.height, (int32)(d.dstFrame.format), C.SWS_BILINEAR, nil, nil, nil)
		if d.swsCtx == nil {
			return nil, fmt.Errorf("sws_getContext() err")
		}

		dstFrameSize := C.av_image_get_buffer_size((int32)(d.dstFrame.format), d.dstFrame.width, d.dstFrame.height, 1)
		d.dstFramePtr = (*[1 << 30]uint8)(unsafe.Pointer(d.dstFrame.data[0]))[:dstFrameSize:dstFrameSize]
	}

	// convert frame from YUV420 to RGB
	res = C.sws_scale(d.swsCtx, frameData(d.srcFrame), frameLineSize(d.srcFrame),
		0, d.srcFrame.height, frameData(d.dstFrame), frameLineSize(d.dstFrame))
	if res < 0 {
		return nil, fmt.Errorf("sws_scale() err")
	}

	// embed frame into an image.Image
	return &image.RGBA{
		Pix:    d.dstFramePtr,
		Stride: 4 * (int)(d.dstFrame.width),
		Rect: image.Rectangle{
			Max: image.Point{(int)(d.dstFrame.width), (int)(d.dstFrame.height)},
		},
	}, nil
}
