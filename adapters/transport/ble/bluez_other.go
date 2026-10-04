//go:build !linux

package ble

import (
	"context"
	"errors"
)

func Run(context.Context, Options) error {
	return errors.New("the Bluetooth probe currently supports Linux with BlueZ only")
}
