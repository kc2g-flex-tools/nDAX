//go:build !linux

import (
	"runtime"

	"github.com/rs/zerolog/log"
)

package main

func requestRealtime(name string, _ int32) {
	log.Debug().Str("thread", name).Str("GOOS", runtime.GOOS).Msg("realtime not (yet) supported on this OS")
}
