package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
	"github.com/kc2g-flex-tools/flexclient"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/smallnest/ringbuffer"
)

var cfg struct {
	RadioIP       string
	Station       string
	Slice         string
	Sink          string
	Source        string
	DaxCh         string
	LatencyTarget float64
	DebugTiming   bool
	TX            bool
	Realtime      bool
	LogLevel      string
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", ":discover:", "radio IP address or discovery spec")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Slice, "slice", "A", "Slice letter to use")
	flag.StringVar(&cfg.DaxCh, "daxch", "1", "DAX channel # to use")
	flag.StringVar(&cfg.Sink, "sink", "flexdax.rx", "PulseAudio sink to send audio to")
	flag.StringVar(&cfg.Source, "source", "flexdax.tx", "PulseAudio sink to receive from")
	flag.Float64Var(&cfg.LatencyTarget, "latency", 100, "Target RX latency (ms, higher = less sample rate variation)")
	flag.BoolVar(&cfg.DebugTiming, "debug-timing", false, "Print debug messages about buffer timing and resampling")
	flag.BoolVar(&cfg.TX, "tx", true, "Create a TX audio device")
	flag.BoolVar(&cfg.Realtime, "rt", true, "Attempt to acquire realtime priority")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "minimum level of messages to log to console (trace, debug, info, warn, error)")
}

var fc *flexclient.FlexClient
var pc *pulse.Client
var pcli *PulseCLI
var ClientID string
var ClientUUID string
var SliceIdx string
var RXStreamID string
var TXStreamID string

func bindClient() {
	log.Info().Str("station", cfg.Station).Msg("Waiting for station")

	clients := make(chan flexclient.StateUpdate)
	sub := fc.Subscribe(flexclient.Subscription{"client ", clients})
	cmdResult := fc.SendNotify("sub client all")

	var found, cmdComplete bool

	for !(found && cmdComplete) {
		select {
		case upd := <-clients:
			if upd.CurrentState["station"] == cfg.Station {
				ClientID = strings.TrimPrefix(upd.Object, "client ")
				ClientUUID = upd.CurrentState["client_id"]
				found = true
			}
		case <-cmdResult:
			cmdComplete = true
		}
	}

	fc.Unsubscribe(sub)

	log.Info().Str("id", ClientID).Str("uuid", ClientUUID).Msg("Found Client")

	fc.SendAndWait("client bind client_id=" + ClientUUID)
}

func findSlice() {
	log.Info().Str("slice_id", cfg.Slice).Msg("Looking for slice")
	slices := make(chan flexclient.StateUpdate)
	sub := fc.Subscribe(flexclient.Subscription{"slice ", slices})
	cmdResult := fc.SendNotify("sub slice all")

	var found, cmdComplete bool

	for !(found && cmdComplete) {
		select {
		case upd := <-slices:
			if upd.CurrentState["index_letter"] == cfg.Slice && upd.CurrentState["client_handle"] == ClientID {
				SliceIdx = strings.TrimPrefix(upd.Object, "slice ")
				found = true
			}
		case <-cmdResult:
			cmdComplete = true
		}
	}

	fc.Unsubscribe(sub)
	log.Info().Str("slice_idx", SliceIdx).Msg("Found slice")
}

func enableDax() {
	fc.SliceSet(SliceIdx, flexclient.Object{"dax": cfg.DaxCh})

	cmd := "dax audio set " + cfg.DaxCh + " slice=" + SliceIdx
	if cfg.TX {
		cmd += " tx=1"
	}

	fc.SendAndWait(cmd)

	res := fc.SendAndWait("stream create type=dax_rx dax_channel=" + cfg.DaxCh)
	if res.Error != 0 {
		log.Fatal().Str("code", fmt.Sprintf("%08X", res.Error)).Str("message", res.Message).Msg("Create dax_rx stream failed")
	}

	RXStreamID = res.Message
	log.Info().Str("stream_id", RXStreamID).Msg("Enabled RX DAX stream")

	fc.SendAndWait(fmt.Sprintf("audio stream 0x%s slice %s gain %d", RXStreamID, SliceIdx, 50))

	if cfg.TX {
		res = fc.SendAndWait("stream create type=dax_tx" + cfg.DaxCh)
		if res.Error != 0 {
			log.Fatal().Str("code", fmt.Sprintf("%08X", res.Error)).Str("message", res.Message).Msg("Create dax_tx stream failed")
		}

		TXStreamID = res.Message
		log.Info().Str("stream_id", TXStreamID).Msg("Enabled TX DAX stream")
	}
}

var lastPktNum = -1
var byteReader bytes.Reader

func decodePacket(pkt *flexclient.VitaPacket, streamID uint32, dst []float32, drop *int64) []float32 {
	if pkt.Preamble.Class_id.PacketClassCode == 0x03e3 && pkt.Preamble.Stream_id == streamID {
		pktNum := int(pkt.Preamble.Header.Packet_count)
		if lastPktNum != -1 {
			diff := (16 + pktNum - lastPktNum) % 16
			if diff != 1 {
				log.Warn().Int("gap", diff).Msg("VITA packet discontinuity")
			}
		}
		lastPktNum = pktNum

		samples := make([]float32, len(pkt.Payload)/4)
		byteReader.Reset(pkt.Payload)
		binary.Read(&byteReader, binary.BigEndian, samples)

		dst = append(dst, samples...)
		d := int(atomic.LoadInt64(drop))
		if d > len(dst) {
			d = len(dst)
		}
		if d > 0 {
			copy(dst, dst[d:])
			dst = dst[:len(dst)-d]
			atomic.AddInt64(drop, int64(-d))
		}
	}

	return dst
}

func streamToPulse() {
	tmp, err := strconv.ParseUint(RXStreamID, 16, 32)
	if err != nil {
		log.Fatal().Err(err).Msg("Parse RXStreamID failed")
	}

	StreamIDInt := uint32(tmp)

	sink, err := pc.SinkByID(cfg.Sink)
	if err != nil {
		log.Fatal().Err(err).Msg("pc.SinkByID failed")
	}

	lTargetMicros := uint64(cfg.LatencyTarget * 1000)
	var latency = lTargetMicros

	r := NewResampler(lTargetMicros, 42)
	var drop int64

	vitaPackets := make(chan flexclient.VitaPacket, int(cfg.LatencyTarget*48/256+100))
	var buf []float32
	var bufLock sync.RWMutex
	done := make(chan struct{})
	started := false
	startedChan := make(chan struct{})
	updateLatency := make(chan struct{}, 1)
	var updTimer, statsTimer int

	var stream *pulse.PlaybackStream

	stream, err = pc.NewPlayback(
		pulse.Float32Reader(func(out []float32) (int, error) {
			if !started {
				if cfg.Realtime {
					requestRealtime("rx thread", 19)
				}

				started = true
				startedChan <- struct{}{}

				// This is a hack... for reasons I can't quite explain, we almost always end up with more in the buffer
				// than we wanted to, just a moment after startup. Wait long enough for everything to settle, and then
				// do a jam sync, because it's preferable than having a wonky rate for several minutes to drag it
				// into sync.
				time.AfterFunc(5*time.Second, func() {
					lat := atomic.LoadUint64(&latency)
					excessSamples := ((int64(lat) - int64(lTargetMicros)) * 48000 / 1e6)
					log.Debug().Int64("excess_samples", excessSamples).Msg("Want to drop")

					if excessSamples > 0 {
						r.accum = 0
						r.fll = 0
						atomic.StoreInt64(&drop, excessSamples)
					}
				})
			}

		READPACKETS:
			for {
				select {
				case pkt, ok := <-vitaPackets:
					if !ok {
						done <- struct{}{}
						return 0, pulse.EndOfData
					}
					buf = decodePacket(&pkt, StreamIDInt, buf, &drop)
				default:
					if len(buf) < len(out)/2 {
						pkt, ok := <-vitaPackets
						if !ok {
							done <- struct{}{}
							return 0, pulse.EndOfData
						}
						buf = decodePacket(&pkt, StreamIDInt, buf, &drop)
					} else {
						break READPACKETS
					}
				}
			}

			lat := atomic.LoadUint64(&latency)
			bufLock.Lock()
			produced, consumed := r.Resample(buf, out, lat)
			copy(buf, buf[consumed:])
			buf = buf[:len(buf)-consumed]
			bufLock.Unlock()

			updTimer += produced
			if updTimer > 4800 {
				updateLatency <- struct{}{}
				updTimer -= 4800
			}

			statsTimer += produced
			if statsTimer > 48000 {
				msg := r.Stats(lat)
				log.Debug().Msg("timing " + msg)
				statsTimer -= 48000
			}

			return produced, nil
		}),
		pulse.PlaybackSink(sink),
		pulse.PlaybackSampleRate(48000),
		pulse.PlaybackMono,
		pulse.PlaybackLatency(cfg.LatencyTarget/1000),
		pulse.PlaybackMediaName("DAX RX "+cfg.Slice),
		pulse.PlaybackMediaIconName("radio"),
		pulse.PlaybackRawOption(func(c *proto.CreatePlaybackStream) {
			c.BufferMaxLength = c.BufferTargetLength * 4
			c.BufferPrebufferLength = c.BufferTargetLength
			if c.BufferPrebufferLength < 2048 {
				c.BufferPrebufferLength = 2048
			}
			c.AdjustLatency = false
			c.BufferMinimumRequest = 1024
		}),
	)

	if err != nil {
		log.Panic().Err(err).Msg("pc.NewPlayback failed")
	}

	go func() {
		for {
			select {
			case <-updateLatency:
				latRequest := proto.GetPlaybackLatency{
					StreamIndex: stream.StreamIndex(),
					Time:        proto.Time{0, 0},
				}
				var latReply proto.GetPlaybackLatencyReply
				pc.RawRequest(&latRequest, &latReply)
				deviceLat := uint64(latReply.Latency)
				pulseBufferSamples := (uint64(latReply.WriteIndex) - uint64(latReply.ReadIndex)) / 4
				bufLock.RLock()
				ourBufferSamples := uint64(len(vitaPackets)*256 + len(buf))
				bufLock.RUnlock()
				lat := deviceLat + uint64((pulseBufferSamples+ourBufferSamples)*1e6/48000)
				atomic.StoreUint64(&latency, lat)
			case <-done:
				return
			}
		}
	}()

	go stream.Start()
	<-startedChan
	fc.SetVitaChan(vitaPackets)
	<-done
	stream.Drain()
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func streamFromPulse(exit chan struct{}) {
	tmp, err := strconv.ParseUint(TXStreamID, 16, 32)
	if err != nil {
		log.Fatal().Err(err).Msg("Parse TXStreamID failed")
	}

	StreamIDInt := uint32(tmp)

	buf := ringbuffer.New(20 * 256 * 4)

	source, err := pc.SourceByID(cfg.Source + ".monitor")
	if err != nil {
		log.Fatal().Err(err).Msg("pc.SourceByID failed")
	}

	var pktCount uint16
	started := false

	var stream *pulse.RecordStream
	stream, err = pc.NewRecord(
		pulse.Float32Writer(func(in []float32) (int, error) {
			if !started {
				if cfg.Realtime {
					requestRealtime("tx thread", 19)
				}
				started = true
			}

			const pktSize = 256 * 4
			binary.Write(buf, binary.BigEndian, in)

			for buf.Length() >= pktSize {
				var rawSamples [pktSize]byte
				buf.Read(rawSamples[:])

				if allZero(rawSamples[:]) {
					pktCount += 1
					continue
				}

				var pkt bytes.Buffer
				pkt.WriteByte(0x18)
				pkt.WriteByte(0xd0 | byte(pktCount&0xf))
				pktCount += 1
				binary.Write(&pkt, binary.BigEndian, uint16(pktSize/4+7))
				binary.Write(&pkt, binary.BigEndian, StreamIDInt)
				binary.Write(&pkt, binary.BigEndian, uint64(0x00001c2d534c03e3))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				pkt.Write(rawSamples[:])
				fc.SendUdp(pkt.Bytes())
				time.Sleep(1 * time.Millisecond)
			}

			return len(in), nil
		}),
		pulse.RecordSource(source),
		pulse.RecordSampleRate(48000),
		pulse.RecordMono,
		pulse.RecordLatency(0.1),
		pulse.RecordMediaName("DAX TX "+cfg.Slice),
		pulse.RecordMediaIconName("audio-input-microphone"),
		pulse.RecordRawOption(func(c *proto.CreateRecordStream) {
			c.BufferFragSize = 256 * 4 // req 1 packet at a time exactly, we hope
		}),
	)

	if err != nil {
		log.Fatal().Err(err).Msg("pc.NewRecord failed")
	}

	stream.Start()
	defer stream.Close()

	<-exit
}

func main() {
	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{
			Out: os.Stderr,
		},
	).With().Timestamp().Logger()

	flag.Parse()

	logLevel, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		log.Fatal().Str("level", cfg.LogLevel).Msg("Unknown log level")
	}

	if cfg.DebugTiming && logLevel > zerolog.DebugLevel {
		logLevel = zerolog.DebugLevel
	}

	zerolog.SetGlobalLevel(logLevel)

	fc, err = flexclient.NewFlexClient(cfg.RadioIP)
	if err != nil {
		log.Fatal().Err(err).Msg("NewFlexClient failed")
	}

	pc, err = pulse.NewClient(
		pulse.ClientApplicationName("nDAX"),
	)

	if err != nil {
		log.Fatal().Err(err).Msg("pulse.NewClient failed")
	}

	err = ensureCLI()
	if err != nil {
		log.Fatal().Err(err).Msg("Ensuring Pulse CLI failed")
	}

	pcli, err = NewPulseCLI()
	if err != nil {
		log.Fatal().Err(err).Msg("NewPulseCLI failed")
	}
	defer pcli.Close()

	sinkIdx, err := createLoopback(cfg.Sink, "[INTERNAL] Flex RX Loopback", "emblem-symbolic-link", "Flex RX", "radio")
	if err != nil {
		log.Fatal().Err(err).Msg("Create RX loopback failed")
	}
	defer destroyLoopback(sinkIdx)

	if cfg.TX {
		sourceIdx, err := createLoopback(cfg.Source, "Flex TX", "radio", "[INTERNAL] Flex TX Loopback", "emblem-symbolic-link")
		if err != nil {
			log.Fatal().Err(err).Msg("Create TX loopback failed")
		}
		defer destroyLoopback(sourceIdx)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	stopTx := make(chan struct{})

	go func() {
		fc.Run()
		close(stopTx)
		wg.Done()
	}()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		log.Info().Msg("Exit on SIGINT")
		fc.Close()
	}()

	err = fc.InitUDP()
	if err != nil {
		log.Fatal().Err(err).Msg("fc.InitUDP failed")
	}

	go func() {
		if cfg.Realtime {
			requestRealtime("udp thread", 20)
		}
		fc.RunUDP()
	}()

	bindClient()
	findSlice()
	enableDax()

	go streamToPulse()

	if cfg.TX {
		go streamFromPulse(stopTx)
	}

	wg.Wait()
}
