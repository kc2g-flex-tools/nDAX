//go:build !linux || !cgo

package main

import "errors"

const pipewireNativeSupported = false

var errNoNative = errors.New("the native PipeWire backend is not available in this build")

func createNativeSource(name, desc, icon string, latencyMs float64) (rxDevice, error) {
	return nil, errNoNative
}

func createNativeSink(name, desc, icon string, channel int) (txDevice, error) {
	return nil, errNoNative
}
