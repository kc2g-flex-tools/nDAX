//go:build !linux

package main

import (
	"runtime"

	"github.com/rs/zerolog/log"
)

func requestRealtime(name string, _ int32) {
	log.Debug().Str("thread", name).Str("GOOS", runtime.GOOS).Msg("realtime not (yet) supported on this OS")
}
