package coredevice

import (
	"context"
	"fmt"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/rsd"
)

// Lockdown reports only the first of the preferred languages (iPhone18,2, iOS
// 27.2 24B115).
func (d *Device) Language(ctx context.Context) (string, error) {
	conn, err := d.DialShim(ctx, remoteLockdownService)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return readLanguage(ctx, conn)
}

// Language reads the device's language on the lock state's connection.
func (c *LockStateConn) Language(ctx context.Context) (string, error) {
	return readLanguage(ctx, c.conn)
}

func readLanguage(ctx context.Context, conn *rsd.PlistConn) (string, error) {
	reply, err := conn.Request(ctx, map[string]any{
		"Label":           checkinLabel,
		"ProtocolVersion": "2",
		"Request":         "GetValue",
		"Domain":          "com.apple.international",
		"Key":             "Language",
	})
	if err != nil {
		return "", E.Cause(err, "read the device's language")
	}
	if message, failed := reply["Error"]; failed {
		return "", E.New("read the device's language: ", fmt.Sprint(message))
	}
	language, ok := reply["Value"].(string)
	if !ok || language == "" {
		return "", E.New("the device did not report its language")
	}
	return language, nil
}
