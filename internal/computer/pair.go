package computer

import (
	"context"

	"github.com/swipt-automation/swipt/internal/coredevice/lockdown"
	"github.com/swipt-automation/swipt/internal/coredevice/pairing"
	"github.com/swipt-automation/swipt/internal/log"
)

type PairOptions struct {
	Device string
	Logs   *log.Factory
}

func Pair(ctx context.Context, options PairOptions) ([]byte, error) {
	paired, err := chooseDevice(ctx, options.Device)
	if err != nil {
		return nil, err
	}
	peer, err := findLockdownPeer(ctx, paired.UDID)
	if err != nil {
		return nil, err
	}
	logger := options.Logs.NewLogger("pairing")
	conn, err := lockdown.DialService(ctx, peer.record, dialer(peer.device.ID), lockdown.RemotePairing, logger)
	if err != nil {
		return nil, err
	}
	record, err := pairing.Setup(ctx, conn, pairing.SetupOptions{HostName: Name(), Logger: logger})
	if err != nil {
		return nil, err
	}
	return record.Marshal()
}
