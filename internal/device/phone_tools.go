package device

import (
	"context"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/instruments"
)

func (c *conn) closeLocation() {
	if c.location == nil {
		return
	}
	if err := c.location.Close(); err != nil {
		c.logger.Debug(E.Cause(err, "close the location simulation"))
	}
	c.location = nil
}

func (c *conn) setLocation(ctx context.Context, latitude, longitude float64) error {
	c.locationAccess.Lock()
	defer c.locationAccess.Unlock()
	if c.location != nil && c.location.Err() != nil {
		c.closeLocation()
	}
	if c.location == nil {
		location, err := instruments.DialLocation(ctx, c.owner, c.device)
		if err != nil {
			return err
		}
		c.location = location
	}
	if err := c.location.Set(ctx, latitude, longitude); err != nil {
		c.closeLocation()
		return err
	}
	return nil
}

func (p *Phone) SetLocation(ctx context.Context, latitude, longitude float64) error {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	defer c.tree.act()()
	p.locationAccess.Lock()
	defer p.locationAccess.Unlock()
	if err = c.setLocation(ctx, latitude, longitude); err != nil {
		p.attached(c)
		return err
	}
	p.keepLocation(latitude, longitude)
	return nil
}

func (p *Phone) RestoreLocation(latitude, longitude float64) {
	p.locationAccess.Lock()
	p.keepLocation(latitude, longitude)
	p.locationAccess.Unlock()
	if c := p.connection.currentConn(); c != nil {
		p.attached(c)
	}
}

func (p *Phone) keepLocation(latitude, longitude float64) {
	if p.location == nil {
		p.location = &simulatedLocation{release: p.connection.hold()}
	}
	p.location.latitude, p.location.longitude = latitude, longitude
}

func (p *Phone) ClearLocation(ctx context.Context) error {
	p.locationAccess.Lock()
	defer p.locationAccess.Unlock()
	if p.location != nil {
		defer p.location.release()
		p.location = nil
	}
	// DTLocationSimulationService stops the simulation when its channel disconnects
	// (DVTInstrumentsFoundation 24B5110a, -[DTLocationSimulationService initWithChannel:]).
	c := p.connection.currentConn()
	if c == nil {
		return nil
	}
	defer c.tree.act()()
	ctx, cancel := c.operation(ctx)
	defer cancel()
	c.locationAccess.Lock()
	defer c.locationAccess.Unlock()
	if c.location == nil || c.location.Err() != nil {
		c.closeLocation()
		return nil
	}
	err := c.location.Clear(ctx)
	c.closeLocation()
	return err
}

func (p *Phone) attached(c *conn) {
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		p.locationAccess.Lock()
		defer p.locationAccess.Unlock()
		if p.location == nil {
			return
		}
		if err := c.setLocation(ctx, p.location.latitude, p.location.longitude); err != nil && c.ctx.Err() == nil {
			p.logger.Warn(E.Cause(err, "simulate the location again"))
		}
	}()
}
