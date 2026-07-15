//go:build linux && cgo

package main

import (
	"fmt"
	"os"

	"github.com/kc2g-flex-tools/nDAX/pw"
)

const pipewireNativeSupported = true

func nativeFormat() pw.Format {
	if audioCfg.bytesPerSample == 4 {
		return pw.F32BE
	}
	return pw.S16BE
}

func nativeProps(icon string) map[string]string {
	return map[string]string{
		"device.icon_name": icon,
		"device.icon-name": icon,
		"nDAX.pid":         fmt.Sprintf("%d", os.Getpid()),
	}
}

func createNativeSource(name, desc, icon string, latencyMs float64) (rxDevice, error) {
	bufferBytes := int(float64(audioCfg.sampleRate*audioCfg.bytesPerSample) * latencyMs / 1000)

	return pw.NewSource(pw.Config{
		Name:          name,
		Description:   desc,
		Rate:          audioCfg.sampleRate,
		Channels:      1,
		Format:        nativeFormat(),
		LatencyFrames: audioCfg.samplesPerPacket,
		BufferBytes:   bufferBytes,
		Props:         nativeProps(icon),
	})
}

func createNativeSink(name, desc, icon string, channel int) (txDevice, error) {
	channels := 1
	if channel != 0 {
		channels = 2
	}

	return pw.NewSink(pw.Config{
		Name:          name,
		Description:   desc,
		Rate:          audioCfg.sampleRate,
		Channels:      channels,
		Format:        nativeFormat(),
		LatencyFrames: audioCfg.samplesPerPacket,
		Props:         nativeProps(icon),
	})
}
