// The device sent unencrypted video RTP and RTCP from one UDP port.
// Starting a second stream of display 1 stopped the first stream after its
// final frame at 3.09 s (observed on iPad17,1, build 24B5089g).
package display

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"
	"github.com/swipt-automation/swipt/internal/coredevice"

	"github.com/google/uuid"
)

const (
	serviceName = "com.apple.coredevice.displayservice"

	featureStart = "com.apple.coredevice.feature.startmediastream"
	actionStart  = "com.apple.coredevice.action.mediastreamstart"
	featureStop  = "com.apple.coredevice.feature.stopmediastream"
	actionStop   = "com.apple.coredevice.action.mediastreamstop"

	// These values and the start request fields match pymobiledevice3
	// (pymobiledevice3 1146717,
	// pymobiledevice3/remote/core_device/display_service.py:40-45, 141-164, 197-213).
	clientSupportedFeatures = uint64(140)
	accessNetworkType       = int64(1)
	transportProtocolType   = int64(2)

	// Display ID 1 produced the main-screen stream (observed on iPad17,1,
	// build 24B5089g).
	DefaultDisplayID = 1
	rtcpTimeout      = 30 * time.Second
)

type Client struct {
	owner            context.Context
	device           *coredevice.Device
	logger           logger.ContextLogger
	deviceIdentifier string

	infoTurn  chan struct{}
	info      *Info
	infoAsked time.Time
}

func New(owner context.Context, device *coredevice.Device, logger logger.ContextLogger) *Client {
	return &Client{
		owner:            owner,
		device:           device,
		logger:           logger,
		deviceIdentifier: uuid.NewString(),
		infoTurn:         make(chan struct{}, 1),
	}
}

func (c *Client) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if err := c.device.Err(); err != nil {
		cancel(err)
	} else if err := context.Cause(c.owner); err != nil {
		cancel(err)
	} else {
		go func() {
			select {
			case <-c.device.Done():
				cancel(c.device.Err())
			case <-c.owner.Done():
				cancel(context.Cause(c.owner))
			case <-ctx.Done():
			}
		}()
	}
	return ctx, func() { cancel(context.Canceled) }
}

// The device returns stoppedStreams in the client-offered SSRC namespace
// (observed on iPad17,1, iPadOS 27.2 24B5099f).
func (c *Client) stop(ssrc uint32) (func() ([]uint32, error), error) {
	ctx, cancel := c.operationContext(c.owner)
	if err := context.Cause(ctx); err != nil {
		cancel()
		return nil, err
	}
	conn, err := c.device.DialXPC(ctx, serviceName)
	if err != nil {
		cancel()
		return nil, err
	}
	err = conn.SendContext(ctx, coredevice.FeatureRequest(c.deviceIdentifier, featureStop, actionStop, map[string]any{
		"stopAll":     false,
		"identifiers": []any{uint64(ssrc)},
	}), true)
	if ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		conn.Close()
		cancel()
		return nil, E.Cause(err, featureStop)
	}
	return func() ([]uint32, error) {
		defer cancel()
		defer conn.Close()
		stopReceive := context.AfterFunc(ctx, func() { conn.Close() })
		reply, err := conn.Receive()
		stopReceive()
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		if err != nil {
			return nil, E.Cause(err, featureStop)
		}
		output, err := coredevice.FeatureOutput(featureStop, reply)
		if err != nil {
			return nil, err
		}
		return parseStopReply(output)
	}, nil
}

type StreamOptions struct {
	// A probe started audio then video with one session UUID. Video returned
	// a nonzero SyncStreamToken; the video-only probe returned zero (observed
	// on iPad17,1, build 24B5089g).
	SessionID uuid.UUID
	// The device echoes the offered SSRC as RemoteSSRC (observed on
	// iPad17,1, build 24B5089g).
	SSRC           uint32
	Mode           Mode
	MaximumBitrate uint64
	TilesPerFrame  int
	PixelFormats   uint32
	ReadOptions    N.ReadWaitOptions
}

func (c *Client) StartVideo(ctx context.Context, options StreamOptions) (*Stream, error) {
	return c.start(ctx, Video, options, map[string]any{
		"CoreDeviceVideoDisplayMode": map[string]any{"string": "DisplayByID"},
		"VideoStreamForDisplayID":    map[string]any{"int": int64(DefaultDisplayID)},
	})
}

func (c *Client) StartAudio(ctx context.Context, options StreamOptions) (*Stream, error) {
	return c.start(ctx, Audio, options, nil)
}

func startInput(streamType StreamType, sessionID uuid.UUID, offer []byte, receiverIP string, receiverPort int, senderIP string, extraOptions map[string]any) map[string]any {
	streamOptions := map[string]any{
		"AVCMediaStreamNegotiatorAccessNetworkType":     map[string]any{"int": accessNetworkType},
		"AVCMediaStreamNegotiatorTransportProtocolType": map[string]any{"int": transportProtocolType},
		"avcMediaStreamOptionClientSessionID":           map[string]any{"uuid": sessionID},
	}
	maps.Copy(streamOptions, extraOptions)
	return map[string]any{
		"clientSupportedFeatures": clientSupportedFeatures,
		"direction":               "output",
		"negotiatorOffer":         offer,
		"options":                 streamOptions,
		"receiverIP":              receiverIP,
		"receiverPort":            uint64(receiverPort),
		"senderIP":                senderIP,
		// The device answers with this value as RTCPTimeoutInterval: a probe
		// sent 3600 and got 3600 back (observed on iPad17,1, build
		// 24B5089g).
		"timeout": uint64(rtcpTimeout / time.Second),
		"type":    string(streamType),
	}
}

func (c *Client) start(ctx context.Context, streamType StreamType, options StreamOptions, extraOptions map[string]any) (*Stream, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	for options.SSRC == 0 {
		options.SSRC = rand.Uint32()
	}
	conn, err := c.device.ListenUDP(0, options.ReadOptions)
	if err != nil {
		return nil, E.Cause(err, "start ", string(streamType), ": bind receiver")
	}
	input := startInput(streamType, options.SessionID, encodeOffer(streamType, options, time.Now()),
		c.device.LocalAddress().String(), int(conn.Port()),
		c.device.Address().String(), extraOptions)

	began := time.Now()
	control, err := c.device.DialXPC(ctx, serviceName)
	if err != nil {
		conn.Close()
		return nil, E.Cause(err, "start ", string(streamType))
	}
	connected := time.Now()
	output, err := coredevice.InvokeFeature(ctx, control, c.deviceIdentifier, featureStart, actionStart, input)
	answered := time.Now()
	if err != nil {
		control.Close()
		conn.Close()
		if _, isFeatureError := errors.AsType[*coredevice.FeatureError](err); !isFeatureError {
			err = E.Errors(err, c.stopOffered(options.SSRC))
		}
		return nil, E.Cause(err, "start ", string(streamType))
	}
	info, err := parseStartReply(output)
	if err != nil {
		control.Close()
		conn.Close()
		err = E.Errors(err, c.stopOffered(options.SSRC))
		return nil, E.Cause(err, "start ", string(streamType))
	}
	stream := newStream(c, info, conn, control)
	c.logger.Debug("display stream started: ", string(info.Type), ", SSRC ", info.SSRC, ", device SSRC ", info.DeviceSSRC,
		", device ", info.DeviceAddress, ", ", info.Width, "x", info.Height, ", ", info.TilesPerFrame, " tiles, LTRP ", info.LTRP,
		", sync token ", info.SyncToken, ", RTCP timeout ", info.RTCPTimeout,
		", connected in ", connected.Sub(began), ", replied in ", answered.Sub(connected))
	err = stream.startKeepalive()
	if err != nil {
		return nil, E.Cause(E.Errors(err, stream.Close()), "start ", string(streamType))
	}
	return stream, nil
}

func (c *Client) stopOffered(ssrc uint32) error {
	reply, err := c.stop(ssrc)
	if err != nil {
		return E.Cause(err, "stop after a failed start")
	}
	go func() {
		if _, err := reply(); err != nil {
			c.logger.Debug(E.Cause(err, "stop stream ", ssrc, " after a failed start"))
		}
	}()
	return nil
}
