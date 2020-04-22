package main

import (
	"encoding/binary"
	"fmt"
	"math"
)

type Resampler struct {
	latencyTarget float64
	latHist       [60]uint64
	histIndex     int
	wrapped       bool
	accum         float64
	dropped       int
	padded        int
	minLatency    uint64
	maxLatency    uint64
	lastSample    [4]byte
}

// TODO: parametrize this on sample rate and packet size, so the control loop isn't powered by magic numbers.
func NewResampler(target float64) *Resampler {
	return &Resampler{
		latencyTarget: target,
		minLatency:    ^uint64(0),
	}
}

func interpolateSample(prev, next []byte) []byte {
	prevFloat := math.Float32frombits(binary.BigEndian.Uint32(prev))
	nextFloat := math.Float32frombits(binary.BigEndian.Uint32(next))
	interpolated := (prevFloat + nextFloat) / 2
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], math.Float32bits(interpolated))
	return out[:]
}

func (r *Resampler) ResamplePacket(in []byte, latency uint64) []byte {
	if latency < r.minLatency {
		r.minLatency = latency
	}
	if latency > r.maxLatency {
		r.maxLatency = latency
	}

	out := make([]byte, len(in))
	copy(out, in)

	r.accum += float64(latency) - r.latencyTarget

	if r.accum > 12*r.latencyTarget { // Drop one sample
		out = out[4:]
		r.dropped += 1
		r.accum -= 10 * r.latencyTarget
	} else if r.accum < -12*r.latencyTarget { // Interpolate one sample
		samp := interpolateSample(r.lastSample[:], out[:4])
		out = append(samp, out...)
		r.padded += 1
		r.accum += 10 * r.latencyTarget
	}

	r.accum *= 0.9999 // Let the integrator leak
	copy(r.lastSample[:], out[len(out)-4:])

	return out
}

func (r *Resampler) Stats(latency uint64) string {
	diff := int64(latency - r.latHist[r.histIndex])

	r.latHist[r.histIndex] = latency
	r.histIndex = (r.histIndex + 1) % len(r.latHist)
	if r.histIndex == 0 && !r.wrapped {
		r.wrapped = true
	}

	msg := fmt.Sprintf("%7d %7d %11.1f +%-3d -%-3d", r.minLatency, r.maxLatency, r.accum, r.padded, r.dropped)
	if r.wrapped {
		rate := float64(diff) / float64(len(r.latHist))
		msg += fmt.Sprintf(" %6.3f %11.5f", rate, (1+rate/1e6)*48000)
	}

	// Reset stats for next time
	r.minLatency = ^uint64(0)
	r.maxLatency = 0
	r.padded = 0
	r.dropped = 0

	return msg
}
