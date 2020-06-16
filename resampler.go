package main

import (
	"fmt"
	"math"
)

type Resampler struct {
	latencyTarget float64
	tolerance     float64
	latHist       [60]uint64
	histIndex     int
	wrapped       bool
	accum         float64
	dropped       int
	padded        int
	minLatency    uint64
	maxLatency    uint64
	holdoff       int
	fll           float64
	runningFor    int
}

// TODO: parametrize this on sample rate and packet size, so the control loop isn't powered by magic numbers.
func NewResampler(target, tolerance uint64) *Resampler {
	return &Resampler{
		latencyTarget: float64(target),
		tolerance:     float64(tolerance) * 48000 / 1e6,
		minLatency:    ^uint64(0),
	}
}

func interpolateSample(prev, next float32) float32 {
	return (prev + next) / 2
}

func (r *Resampler) Resample(in, out []float32, latency uint64) (produced int, consumed int) {
	if latency < r.minLatency {
		r.minLatency = latency
	}
	if latency > r.maxLatency {
		r.maxLatency = latency
	}

	err := (float64(latency) - r.latencyTarget) / 1e6
	pll := err / 300 // Slope to zero phase error within 5 minutes

CHUNK:
	for {
		availOut := len(out) - produced
		availIn := len(in) - consumed
		avail := availOut
		if avail > availIn {
			avail = availIn
		}
		if avail <= 0 {
			break CHUNK
		}

		nCopy := avail
		var add, drop bool

		rate := pll + 0.9*r.fll

		const ratelimit = 500e-6 // max skew: 500ppm

		if rate > ratelimit {
			rate = ratelimit
		} else if rate < -ratelimit {
			rate = -ratelimit
		}

		proj := r.accum + float64(avail)*rate

		if proj < -r.tolerance {
			x := int(math.Ceil((r.tolerance + r.accum) / -rate))

			if x < r.holdoff {
				x = r.holdoff
			}

			if x == 0 {
				x = 1
			}

			if x <= avail-1 {
				nCopy = x
				add = true
			}
		} else if r.accum+proj > r.tolerance {
			x := int(math.Ceil((r.tolerance - r.accum) / rate))

			if x < r.holdoff {
				x = r.holdoff
			}

			if x <= avail-1 {
				nCopy = x
				drop = true
			}
		}

		copy(out[produced:produced+nCopy], in[consumed:consumed+nCopy])
		p := nCopy
		produced += nCopy
		consumed += nCopy
		r.holdoff -= nCopy
		if r.holdoff < 0 {
			r.holdoff = 0
		}

		r.accum += rate * float64(nCopy)

		if add {
			out[produced] = (out[produced-1] + in[consumed]) / 2
			produced += 1
			p += 1
			r.padded += 1
			r.accum += 1
			r.holdoff = 1000
		} else if drop {
			consumed += 1
			r.dropped += 1
			r.accum -= 1
			r.holdoff = 1000
		}

		var frac float64
		if r.runningFor < 10*48000 {
			frac = 0
			r.runningFor += p
		} else if r.runningFor < 120*48000 {
			frac = float64(p) / 48000
			r.runningFor += p
		} else {
			frac = (float64(p) / 48000) / 5
		}
		r.fll = (1-frac)*r.fll + frac*rate
	}

	return
}

func (r *Resampler) Stats(latency uint64) string {
	diff := int64(latency - r.latHist[r.histIndex])

	r.latHist[r.histIndex] = latency
	r.histIndex = (r.histIndex + 1) % len(r.latHist)
	if r.histIndex == 0 && !r.wrapped {
		r.wrapped = true
	}

	msg := fmt.Sprintf("%7d %7d %7.3f %8.6f +%-3d -%-3d", (r.minLatency+r.maxLatency)/2, r.maxLatency-r.minLatency, r.accum, -48000*r.fll, r.padded, r.dropped)
	if r.wrapped {
		rate := float64(diff) / float64(len(r.latHist))
		msg += fmt.Sprintf(" %8.3f %11.5f", rate, (1+rate/1e6)*48000)
	}

	// Reset stats for next time
	r.minLatency = ^uint64(0)
	r.maxLatency = 0
	r.padded = 0
	r.dropped = 0

	return msg
}
