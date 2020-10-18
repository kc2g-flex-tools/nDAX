package main

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
	//	log "github.com/rs/zerolog/log"
)

type Resampler struct {
	sync.RWMutex
	Filter        *LatencyFilter
	latencyTarget float64
	tolerance     float64
	accum         float64
	dropped       int
	padded        int
	holdoff       int
	fll           float64
	runningFor    int
}

// TODO: parametrize this on sample rate and packet size, so the control loop isn't powered by magic numbers.
func NewResampler(target, tolerance float64, filter *LatencyFilter) *Resampler {
	return &Resampler{
		latencyTarget: target,
		tolerance:     tolerance,
		Filter:        filter,
	}
}

func (r *Resampler) Resample(in, out []float32) (produced int, consumed int) {
	r.Lock()
	defer r.Unlock()

	latency, slope := r.Filter.Get(0, 0)

	if slope > 100e-6 {
		slope = 100e-6
	} else if slope < -100e-6 {
		slope = -100e-6
	}

	err := (latency + 180*slope - r.latencyTarget) / 1e6
	knee := 0.02e-6*r.latencyTarget + 1e-3
	factor := math.Abs(err) / (math.Abs(err) + knee)
	err *= factor
	pll := err / 180

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

		const ratelimit = 500e-6 // max skew: 500ppm

		if pll > ratelimit {
			pll = ratelimit
		} else if pll < -ratelimit {
			pll = -ratelimit
		}

		if r.fll > ratelimit {
			r.fll = ratelimit
		} else if r.fll < -ratelimit {
			r.fll = -ratelimit
		}

		rate := pll + 0.98*r.fll
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
		} else if proj > r.tolerance {
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
			frac = (float64(p) / 48000) / 30
			r.runningFor += p
		} else {
			frac = (float64(p) / 48000) / 240
		}

		r.fll = (1-frac)*r.fll + frac*(48000*slope/1e6+rate)
	}

	return
}

func (r *Resampler) Stats() string {
	r.RLock()
	defer r.RUnlock()
	avg, slope := r.Filter.Get(0, 0)

	msg := fmt.Sprintf("%7.1f %7.1f %7.3f %7.3f +%-3d -%-3d %7.3f %11.5f", avg, avg+60*slope, r.accum, -48000*r.fll, r.padded, r.dropped, slope, (1+slope/1e6)*48000)

	// Reset stats for next time
	r.padded = 0
	r.dropped = 0

	return msg
}

func (r *Resampler) Reset() {
	r.Lock()
	defer r.Unlock()
	r.Filter.Reset()
	r.accum = 0
	r.fll = 0
}

type LatencyMeasurement struct {
	Latency    float64
	MeasuredAt time.Time
}

type SortByLatency []LatencyMeasurement

func (s SortByLatency) Len() int {
	return len(s)
}

func (s SortByLatency) Less(i, j int) bool {
	return s[i].Latency < s[j].Latency
}

func (s SortByLatency) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type LatencyFilter struct {
	sync.RWMutex
	AvgWindow   time.Duration
	SlopeWindow time.Duration

	history []LatencyMeasurement
}

func (f *LatencyFilter) Put(meas LatencyMeasurement) {
	f.Lock()
	defer f.Unlock()
	f.history = append(f.history, meas)
	biggerWindow := f.SlopeWindow
	if f.AvgWindow > biggerWindow {
		biggerWindow = f.AvgWindow
	}

	i := 0
	for i < len(f.history) && f.history[len(f.history)-1].MeasuredAt.Sub(f.history[i].MeasuredAt) > biggerWindow {
		i++
	}

	if i > 0 {
		copy(f.history, f.history[i:])
		f.history = f.history[:len(f.history)-i]
	}
}

func (f *LatencyFilter) Get(avgWindow, slopeWindow time.Duration) (avg, slope float64) {
	f.RLock()
	defer f.RUnlock()

	if avgWindow == 0 || avgWindow > f.AvgWindow {
		avgWindow = f.AvgWindow
	}

	if slopeWindow == 0 || slopeWindow > f.SlopeWindow {
		slopeWindow = f.SlopeWindow
	}

	i := 0
	for i < len(f.history) && f.history[len(f.history)-1].MeasuredAt.Sub(f.history[i].MeasuredAt) > avgWindow {
		i++
	}

	n := len(f.history) - i
	tmp := make([]LatencyMeasurement, n)
	copy(tmp, f.history[i:])
	sort.Sort(SortByLatency(tmp))

	lo := n / 8
	hi := n - lo

	var sum, ct float64

	for i := lo; i < hi; i++ {
		sum += tmp[i].Latency
		ct += 1
	}

	if ct > 0 {
		avg = sum / ct
	}

	var sumx, sumy, sumxx, sumxy float64

	i = 0
	for i < len(f.history) && f.history[len(f.history)-1].MeasuredAt.Sub(f.history[i].MeasuredAt) > slopeWindow {
		i++
	}

	n = len(f.history) - i

	if n == 0 {
		return
	}

	tmp = make([]LatencyMeasurement, n)
	copy(tmp, f.history[i:])
	sort.Sort(SortByLatency(tmp))

	lo = n / 8
	hi = n - lo

	loLat := tmp[lo].Latency
	hiLat := tmp[hi-1].Latency

	n = 0
	for j := i; j < len(f.history); j++ {
		y := f.history[j].Latency
		if y >= loLat && y <= hiLat {
			x := f.history[j].MeasuredAt.Sub(f.history[i].MeasuredAt).Seconds()
			sumx += x
			sumy += y
			sumxx += x * x
			sumxy += x * y
			n++
		}
	}

	if n > 0 && sumx > 0 {
		slope = (sumxy - sumx*sumy/float64(n)) / (sumxx - sumx*sumx/float64(n))
	}

	return
}

func (f *LatencyFilter) Reset() {
	f.Lock()
	defer f.Unlock()

	f.history = f.history[:0]
}
