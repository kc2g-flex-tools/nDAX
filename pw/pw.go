//go:build linux && cgo

// Package pw creates PipeWire source and sink nodes directly in the node
// graph via libpipewire, instead of going through module-pipe-source/sink
// and a FIFO. A stream with media.class Audio/Source (output direction) is
// a virtual source; media.class Audio/Sink (input direction) is a virtual
// sink. Both are scheduled by the graph driver like any other device node.
package pw

/*
// Not pkg-config: libpipewire-0.3's .pc emits -fno-strict-overflow, which
// cgo's flag allowlist rejects. These paths are what the .pc file resolves
// to on all major distros.
#cgo CFLAGS: -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2 -D_REENTRANT
#cgo LDFLAGS: -lpipewire-0.3
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const Supported = true

type Format int

const (
	S16BE Format = iota
	F32BE
)

func (f Format) bytes() int {
	if f == F32BE {
		return 4
	}
	return 2
}

type Config struct {
	Name          string            // node.name
	Description   string            // node.description
	Rate          int               // sample rate in Hz
	Channels      int               // 1 or 2
	Format        Format            // sample format (big-endian, matching DAX)
	LatencyFrames int               // requested quantum (node.latency numerator)
	BufferBytes   int               // ring buffer capacity between Go and the graph
	Props         map[string]string // extra node properties
}

var (
	initOnce sync.Once
	initErr  error

	regMu  sync.Mutex
	nextID uintptr = 1
	rings          = map[uintptr]*ring{}
)

func ensureInit() error {
	initOnce.Do(func() {
		if C.ndax_pw_init() < 0 {
			initErr = errors.New("initializing PipeWire thread loop failed")
		}
	})
	return initErr
}

type stream struct {
	handle unsafe.Pointer
	id     uintptr
	ring   *ring
}

// Source is a virtual source node. Audio written with Write is delivered to
// applications recording from the source.
type Source struct{ stream }

// Sink is a virtual sink node. Audio played into it by applications is
// returned by Read, which blocks until data is available.
type Sink struct{ stream }

func newStream(cfg Config, isSink bool) (stream, error) {
	if err := ensureInit(); err != nil {
		return stream{}, err
	}

	minBuf := 4 * cfg.LatencyFrames * cfg.Format.bytes() * cfg.Channels
	if cfg.BufferBytes < minBuf {
		cfg.BufferBytes = minBuf
	}
	r := newRing(cfg.BufferBytes)

	regMu.Lock()
	id := nextID
	nextID++
	rings[id] = r
	regMu.Unlock()

	name := C.CString(cfg.Name)
	desc := C.CString(cfg.Description)
	defer C.free(unsafe.Pointer(name))
	defer C.free(unsafe.Pointer(desc))

	var kv []*C.char
	for k, v := range cfg.Props {
		kv = append(kv, C.CString(k), C.CString(v))
	}
	defer func() {
		for _, p := range kv {
			C.free(unsafe.Pointer(p))
		}
	}()
	var kvPtr **C.char
	if len(kv) > 0 {
		kvPtr = &kv[0]
	}

	isFloat := 0
	if cfg.Format == F32BE {
		isFloat = 1
	}
	sinkFlag := 0
	if isSink {
		sinkFlag = 1
	}

	errbuf := make([]byte, 256)
	handle := C.ndax_pw_stream_new(
		C.uintptr_t(id), C.int(sinkFlag),
		name, desc,
		C.int(cfg.Rate), C.int(cfg.Channels), C.int(isFloat),
		C.int(cfg.LatencyFrames),
		kvPtr, C.int(len(kv)/2),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)),
	)
	if handle == nil {
		regMu.Lock()
		delete(rings, id)
		regMu.Unlock()
		msg := errbuf[:]
		if i := indexByte(msg, 0); i >= 0 {
			msg = msg[:i]
		}
		return stream{}, fmt.Errorf("creating PipeWire stream: %s", msg)
	}

	return stream{handle: handle, id: id, ring: r}, nil
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func (s *stream) Close() {
	if s.handle == nil {
		return
	}
	C.ndax_pw_stream_destroy(s.handle)
	s.handle = nil
	s.ring.Close()
	regMu.Lock()
	delete(rings, s.id)
	regMu.Unlock()
}

func NewSource(cfg Config) (*Source, error) {
	st, err := newStream(cfg, false)
	if err != nil {
		return nil, err
	}
	return &Source{st}, nil
}

func (s *Source) Write(p []byte) (int, error) {
	return s.ring.Write(p)
}

func NewSink(cfg Config) (*Sink, error) {
	st, err := newStream(cfg, true)
	if err != nil {
		return nil, err
	}
	return &Sink{st}, nil
}

func (s *Sink) Read(p []byte) (int, error) {
	return s.ring.ReadBlock(p)
}

func lookupRing(id uintptr) *ring {
	regMu.Lock()
	r := rings[id]
	regMu.Unlock()
	return r
}

//export goPWFill
func goPWFill(id C.uintptr_t, buf unsafe.Pointer, n C.uint32_t) {
	p := unsafe.Slice((*byte)(buf), int(n))
	if r := lookupRing(uintptr(id)); r != nil {
		r.ReadZero(p)
	} else {
		clear(p)
	}
}

//export goPWDrain
func goPWDrain(id C.uintptr_t, buf unsafe.Pointer, n C.uint32_t) {
	if r := lookupRing(uintptr(id)); r != nil {
		r.Write(unsafe.Slice((*byte)(buf), int(n)))
	}
}
