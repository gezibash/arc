//go:build !linux

package ble

import (
	"context"
	"errors"
)

func OpenDirect(context.Context, DirectConfig) (*Direct, error) {
	return nil, directSupported()
}

func directSupported() error {
	return errors.New("bluetooth: direct radio connections require Linux with BlueZ")
}
