package device

import (
	"context"
	"sync"

	"github.com/swipt-automation/swipt/internal/coredevice/screencapture"
)

// The capture service answered a request made while another capture was in
// progress with CoreDeviceError 25003 (iPhone18,2, iOS 27.2).
func (p *Phone) Capture(ctx context.Context) ([]byte, float64, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer done()
	var geometry phoneGeometry
	var geometryErr error
	var read sync.WaitGroup
	read.Go(func() { geometry, geometryErr = c.readGeometry(ctx) })
	p.captureAccess.Lock()
	data, err := screencapture.Capture(ctx, c.device)
	p.captureAccess.Unlock()
	read.Wait()
	if geometryErr != nil {
		return nil, 0, geometryErr
	}
	return data, geometry.Scale, err
}
