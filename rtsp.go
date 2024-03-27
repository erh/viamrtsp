package viamrtsp

import (
	"context"
	"image"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/google/uuid"

	"github.com/erh/viamrtsp/formatprocessor"
	"github.com/erh/viamrtsp/unit"

	"github.com/pion/rtp"
	"github.com/pkg/errors"
	goutils "go.viam.com/utils"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/gostream"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/rimage/transform"
)

var (
	family                       = resource.ModelNamespace("erh").WithFamily("viamrtsp")
	ModelH264                    = family.WithModel("rtsp-h264")
	ErrH264PassthroughNotEnabled = errors.New("H264 passthrough is not enabled")
)

func init() {
	resource.RegisterComponent(camera.API, ModelH264, resource.Registration[camera.Camera, *Config]{
		Constructor: func(
			ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger,
		) (camera.Camera, error) {
			newConf, err := resource.NativeConfig[*Config](conf)
			if err != nil {
				return nil, err
			}
			return newRTSPCamera(ctx, conf.ResourceName(), newConf, logger)
		},
	})
}

// Config are the config attributes for an RTSP camera model.
type Config struct {
	Address          string                             `json:"rtsp_address"`
	H264Passthrough  bool                               `json:"h264_passthrough"`
	IntrinsicParams  *transform.PinholeCameraIntrinsics `json:"intrinsic_parameters,omitempty"`
	DistortionParams *transform.BrownConrady            `json:"distortion_parameters,omitempty"`
}

// Validate checks to see if the attributes of the model are valid.
func (conf *Config) Validate(path string) ([]string, error) {
	_, err := base.ParseURL(conf.Address)
	if err != nil {
		return nil, err
	}
	if conf.IntrinsicParams != nil {
		if err := conf.IntrinsicParams.CheckValid(); err != nil {
			return nil, err
		}
	}
	if conf.DistortionParams != nil {
		if err := conf.DistortionParams.CheckValid(); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

type unitSubscriberFunc func(unit.Unit) error
type subAndCB struct {
	cb  unitSubscriberFunc
	sub *camera.StreamSubscription
}

// rtspCamera contains the rtsp client, and the reader function that fulfills the camera interface.
type rtspCamera struct {
	gostream.VideoReader
	u *base.URL

	client     *gortsplib.Client
	rawDecoder *decoder

	cancelCtx  context.Context
	cancelFunc context.CancelFunc

	activeBackgroundWorkers sync.WaitGroup

	latestFrame atomic.Pointer[image.Image]

	logger logging.Logger

	h264Passthrough bool

	subsMu       sync.RWMutex
	subAndCBByID map[camera.StreamSubscriptionID]subAndCB
}

// Close closes the camera. It always returns nil, but because of Close() interface, it needs to return an error.
func (rc *rtspCamera) Close(ctx context.Context) error {
	rc.cancelFunc()
	defer rc.activeBackgroundWorkers.Wait()
	rc.unsubscribeAll()
	rc.closeConnection()
	return nil
}

// clientReconnectBackgroundWorker checks every 5 sec to see if the client is connected to the server, and reconnects if not.
func (rc *rtspCamera) clientReconnectBackgroundWorker() {
	rc.activeBackgroundWorkers.Add(1)
	goutils.ManagedGo(func() {
		for goutils.SelectContextOrWait(rc.cancelCtx, 5*time.Second) {
			badState := false

			// use an OPTIONS request to see if the server is still responding to requests
			if rc.client == nil {
				badState = true
			} else {
				res, err := rc.client.Options(rc.u)
				if err != nil && (errors.Is(err, liberrors.ErrClientTerminated{}) ||
					errors.Is(err, io.EOF) ||
					errors.Is(err, syscall.EPIPE) ||
					errors.Is(err, syscall.ECONNREFUSED)) {
					rc.logger.Warnw("The rtsp client encountered an error, trying to reconnect", "url", rc.u, "error", err)
					badState = true
				} else if res != nil && res.StatusCode != base.StatusOK {
					rc.logger.Warnw("The rtsp server responded with non-OK status", "url", rc.u, "status code", res.StatusCode)
					badState = true
				}
			}

			if badState {
				if err := rc.reconnectClient(); err != nil {
					rc.logger.Warnw("cannot reconnect to rtsp server", "error", err)
				} else {
					rc.logger.Infow("reconnected to rtsp server", "url", rc.u)
				}
			}
		}
	}, rc.activeBackgroundWorkers.Done)
}

func (rc *rtspCamera) closeConnection() {
	if rc.client != nil {
		rc.client.Close()
		rc.client = nil
	}
	if rc.rawDecoder != nil {
		rc.rawDecoder.close()
		rc.rawDecoder = nil
	}
}

// reconnectClient reconnects the RTSP client to the streaming server by closing the old one and starting a new one.
func (rc *rtspCamera) reconnectClient() (err error) {
	if rc == nil {
		return errors.New("rtspCamera is nil")
	}
	rc.logger.Warnf("reconnectClient called")

	rc.closeConnection()

	// replace the client with a new one, but close it if setup is not successful
	rc.client = &gortsplib.Client{}
	rc.client.OnPacketLost = func(err error) {
		rc.logger.Debugf("OnPacketLost: err: %s", err.Error())
	}
	rc.client.OnTransportSwitch = func(err error) {
		rc.logger.Debugf("OnTransportSwitch: err: %s", err.Error())
	}
	rc.client.OnDecodeError = func(err error) {
		rc.logger.Debugf("OnDecodeError: err: %s", err.Error())
	}
	err = rc.client.Start(rc.u.Scheme, rc.u.Host)
	if err != nil {
		return err
	}

	var clientSuccessful bool
	defer func() {
		if !clientSuccessful {
			rc.closeConnection()
		}
	}()

	session, _, err := rc.client.Describe(rc.u)
	if err != nil {
		return err
	}

	codecInfo, err := getStreamInfo(rc.u.String())
	if err != nil {
		return err
	}

	switch codecInfo {
	case H264:
		rc.logger.Infof("setting up H264 decoder")
		err = rc.initH264(session)
	case H265:
		rc.logger.Infof("setting up H265 decoder")
		err = rc.initH265(session)
	default:
		return errors.Errorf("codec not supported %v", codecInfo)
	}
	if err != nil {
		return err
	}

	_, err = rc.client.Play(nil)
	if err != nil {
		return err
	}
	clientSuccessful = true

	return nil
}

// initH264 initializes the H264 decoder and sets up the client to receive H264 packets.
func (rc *rtspCamera) initH264(session *description.Session) (err error) {
	rc.logger.Info("initH264 BEGIN")
	defer rc.logger.Info("initH264 END")
	// setup RTP/H264 -> H264 decoder
	var f *format.H264
	var forma format.Format

	media := session.FindFormat(&f)
	if media == nil {
		rc.logger.Warn("tracks available")
		for _, x := range session.Medias {
			rc.logger.Warnf("\t %v", x)
		}
		return errors.New("h264 track not found")
	}
	forma = f

	// setup RTP/H264 -> H264 decoder
	rtpDec, err := f.CreateDecoder()
	if err != nil {
		rc.logger.Errorf("error creating H264 decoder %v", err)
		return err
	}

	// setup H264 -> raw frames decoder
	rc.rawDecoder, err = newH264Decoder()
	if err != nil {
		return err
	}

	// if SPS and PPS are present into the SDP, send them to the decoder
	if f.SPS != nil {
		rc.rawDecoder.decode(f.SPS) // nolint:errcheck
	} else {
		rc.logger.Warn("no SPS found in H264 format")
	}
	if f.PPS != nil {
		rc.rawDecoder.decode(f.PPS) // nolint:errcheck
	} else {
		rc.logger.Warn("no PPS found in H264 format")
	}

	var waitingForIframeLogged bool
	iFrameReceived := false
	storeImage := func(pkt *rtp.Packet) {
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				rc.logger.Debugf("error decoding(1) h264 rstp stream %v", err)
			}
			return
		}

		if !iFrameReceived {
			if !h264.IDRPresent(au) {
				if !waitingForIframeLogged {
					rc.logger.Debug("waiting for I-frame")
					waitingForIframeLogged = true
				}
				return
			}
			iFrameReceived = true
			rc.logger.Debug("got I-frame")
		}

		for _, nalu := range au {
			image, err := rc.rawDecoder.decode(nalu)
			if err != nil {
				rc.logger.Error("error decoding(2) h264 rtsp stream  %v", err)
				return
			}
			if image != nil {
				rc.latestFrame.Store(&image)
			}
		}
	}

	onPacketRTP := func(pkt *rtp.Packet) {
		storeImage(pkt)
	}

	if rc.h264Passthrough {
		fp, err := formatprocessor.New(1472, f, true)
		if err != nil {
			return err
		}

		publishToWebRTC := func(pkt *rtp.Packet) {
			pts, ok := rc.client.PacketPTS(media, pkt)
			if !ok {
				return
			}
			ntp := time.Now()
			// NOTE(NickS): Why is this false?
			u, err := fp.ProcessRTPPacket(pkt, ntp, pts, false)
			if err != nil {
				rc.logger.Debug(err.Error())
				return
			}
			rc.subsMu.RLock()
			defer rc.subsMu.RUnlock()
			if len(rc.subAndCBByID) == 0 {
				return
			}

			// Publish the newly received packet Unit to all subscribers
			for _, subAndCB := range rc.subAndCBByID {
				if err := subAndCB.sub.Publish(func() error { return subAndCB.cb(u) }); err != nil {
					rc.logger.Debug("RTP packet dropped due to %s", err.Error())
				}
			}
		}

		onPacketRTP = func(pkt *rtp.Packet) {
			publishToWebRTC(pkt)
			storeImage(pkt)
		}
	}

	resp, err := rc.client.Setup(session.BaseURL, media, 0, 0)
	if err != nil {
		return err
	}
	rc.logger.Infow("Setup", "resp", resp)

	rc.logger.Infof("about to call OnPacketRTP rc.client: %p", rc.client)
	rc.client.OnPacketRTP(media, forma, onPacketRTP)
	rc.logger.Infow("called it")

	return nil
}

// initH265 initializes the H265 decoder and sets up the client to receive H265 packets.
func (rc *rtspCamera) initH265(session *description.Session) (err error) {
	if rc.h264Passthrough {
		return errors.New("address reports to have only an h265 track but H264Passthrough was enabled")
	}
	var f *format.H265

	media := session.FindFormat(&f)
	if media == nil {
		rc.logger.Warn("tracks available")
		for _, x := range session.Medias {
			rc.logger.Warnf("\t %v", x)
		}
		return errors.New("h265 track not found")
	}

	_, err = rc.client.Setup(session.BaseURL, media, 0, 0)
	if err != nil {
		return err
	}

	rtpDec, err := f.CreateDecoder()
	if err != nil {
		rc.logger.Errorf("error creating H265 decoder %v", err)
		return err
	}

	rc.rawDecoder, err = newH265Decoder()
	if err != nil {
		return err
	}

	// For H.265, handle VPS, SPS, and PPS
	if f.VPS != nil {
		rc.rawDecoder.decode(f.VPS) // nolint:errcheck
	} else {
		rc.logger.Warn("no VPS found in H265 format")
	}

	if f.SPS != nil {
		rc.rawDecoder.decode(f.SPS) // nolint:errcheck
	} else {
		rc.logger.Warn("no SPS found in H265 format")
	}

	if f.PPS != nil {
		rc.rawDecoder.decode(f.PPS) // nolint:errcheck
	} else {
		rc.logger.Warnf("no PPS found in H265 format")
	}

	// On packet retreival, turn it into an image, and store it in shared memory
	rc.client.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
		// Extract access units from RTP packets
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
				rc.logger.Errorf("error decoding(1) h265 rstp stream %v", err)
			}
			return
		}

		for _, nalu := range au {
			lastImage, err := rc.rawDecoder.decode(nalu)
			if err != nil {
				rc.logger.Error("error decoding(2) h265 rtsp stream  %v", err)
				return
			}

			if lastImage != nil {
				rc.latestFrame.Store(&lastImage)
			}
		}
	})

	return nil
}

// SubscribeRTP registers the PacketCallback which will be called when there are new packets.
// NOTE: Packets may be dropped before calling packetsCB if the rate new packets are received by
// the VideoCodecStream is greater than the rate the subscriber consumes them.

// TODO: detect the codec in the constructor & reject SubscribeRTP calls if the codec is not h264

func (rc *rtspCamera) SubscribeRTP(ctx context.Context, bufferSize int, packetsCB camera.PacketCallback) (camera.StreamSubscriptionID, error) {
	if !rc.h264Passthrough {
		return uuid.Nil, ErrH264PassthroughNotEnabled
	}

	sub, err := camera.NewStreamSubscription(bufferSize, func(err error) { rc.logger.Errorw("stream subscription hit error", "err", err) })
	if err != nil {
		return uuid.Nil, err
	}
	webrtcPayloadMaxSize := 1188 // 1200 - 12 (RTP header)
	encoder := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: webrtcPayloadMaxSize,
	}

	if err := encoder.Init(); err != nil {
		return uuid.Nil, err
	}

	var firstReceived bool
	var lastPTS time.Duration
	// OnPacketRTP will call this unitSubscriberFunc for all subscribers.
	// unitSubscriberFunc will then convert the Unit into a slice of
	// WebRTC compliant RTP packets & call packetsCB, which will
	// allow the caller of SubscribeRTP to handle the packets.
	// This is intended to free the SubscribeRTP caller from needing
	// to care about how to transform RTSP compliant RTP packets into
	// WebRTC compliant RTP packets.
	unitSubscriberFunc := func(u unit.Unit) error {
		tunit, ok := u.(*unit.H264)
		if !ok {
			return errors.New("(*unit.H264) type conversion error")
		}

		// If we have no AUs we can't encode packets.
		if tunit.AU == nil {
			return nil
		}

		if !firstReceived {
			firstReceived = true
		} else if tunit.PTS < lastPTS {
			return errors.New("WebRTC doesn't support H264 streams with B-frames")
		}
		lastPTS = tunit.PTS

		pkts, err := encoder.Encode(tunit.AU)
		if err != nil {
			// If there is an Encode error we just drop the packets.
			return nil //nolint:nilerr
		}

		if len(pkts) == 0 {
			// If no packets can be encoded from the AU, there is no need to call the subscriber's callback.
			return nil
		}

		for _, pkt := range pkts {
			pkt.Timestamp += tunit.RTPPackets[0].Timestamp
		}

		return packetsCB(pkts)
	}

	rc.subsMu.Lock()
	defer rc.subsMu.Unlock()

	rc.subAndCBByID[sub.ID()] = subAndCB{cb: unitSubscriberFunc, sub: sub}
	sub.Start()
	return sub.ID(), nil
}

// Unsubscribe deregisters the StreamSubscription's callback.
func (rc *rtspCamera) Unsubscribe(ctx context.Context, id camera.StreamSubscriptionID) error {
	rc.subsMu.Lock()
	defer rc.subsMu.Unlock()
	subAndCB, ok := rc.subAndCBByID[id]
	if !ok {
		return errors.New("id not found")
	}
	subAndCB.sub.Close()
	delete(rc.subAndCBByID, id)
	return nil
}

func newRTSPCamera(ctx context.Context, name resource.Name, conf *Config, logger logging.Logger) (camera.Camera, error) {
	logger.Info("newRTSPCamera called")
	u, err := base.ParseURL(conf.Address)
	if err != nil {
		return nil, err
	}
	rtspCam := &rtspCamera{
		u:               u,
		h264Passthrough: conf.H264Passthrough,
		subAndCBByID:    make(map[camera.StreamSubscriptionID]subAndCB),
		logger:          logger,
	}
	err = rtspCam.reconnectClient()
	if err != nil {
		return nil, err
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	reader := gostream.VideoReaderFunc(func(ctx context.Context) (image.Image, func(), error) {
		latest := rtspCam.latestFrame.Load()
		if latest == nil {
			return nil, func() {}, errors.New("no frame yet")
		}
		return *latest, func() {}, nil
	})
	rtspCam.VideoReader = reader
	rtspCam.cancelCtx = cancelCtx
	rtspCam.cancelFunc = cancel
	cameraModel := camera.NewPinholeModelWithBrownConradyDistortion(conf.IntrinsicParams, conf.DistortionParams)
	rtspCam.clientReconnectBackgroundWorker()
	src, err := camera.NewVideoSourceFromReader(ctx, rtspCam, &cameraModel, camera.ColorStream)
	if err != nil {
		return nil, err
	}

	return camera.FromVideoSource(name, src, logger), nil
}

func (rc *rtspCamera) unsubscribeAll() {
	rc.subsMu.Lock()
	defer rc.subsMu.Unlock()
	for id, subAndCB := range rc.subAndCBByID {
		subAndCB.sub.Close()
		delete(rc.subAndCBByID, id)
	}
}
