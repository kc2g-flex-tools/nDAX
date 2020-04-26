package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arodland/flexclient"
	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
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
}

var fc *flexclient.FlexClient
var pc *pulse.Client
var ClientID string
var ClientUUID string
var SliceIdx string
var RXStreamID string
var TXStreamID string

func bindClient() {
	log.Println("Waiting for station:", cfg.Station)

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

	log.Println("Found client ID", ClientID, "UUID", ClientUUID)

	fc.SendAndWait("client bind client_id=" + ClientUUID)
}

func findSlice() {
	log.Println("Looking for slice:", cfg.Slice)
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
	log.Println("Found slice", SliceIdx)
}

func enableDax() {
	fc.SliceSet(SliceIdx, flexclient.Object{"dax": cfg.DaxCh})
	fc.SendAndWait("dax audio set " + cfg.DaxCh + " slice=" + SliceIdx + " tx=1")

	res := fc.SendAndWait("stream create type=dax_rx dax_channel=" + cfg.DaxCh)
	if res.Error != 0 {
		panic(res)
	}

	RXStreamID = res.Message
	log.Println("enabled RX DAX stream", RXStreamID)

	fc.SendAndWait(fmt.Sprintf("audio stream 0x%s slice %s gain %d", RXStreamID, SliceIdx, 50))

	res = fc.SendAndWait("stream create type=dax_tx" + cfg.DaxCh)
	if res.Error != 0 {
		panic(res)
	}

	TXStreamID = res.Message

	log.Println("enabled TX DAX stream", TXStreamID)
}

func streamToPulse() {
	tmp, err := strconv.ParseUint(RXStreamID, 16, 32)
	if err != nil {
		panic(err)
	}

	StreamIDInt := uint32(tmp)

	sink, err := pc.SinkByID(cfg.Sink)
	if err != nil {
		panic(err)
	}

	lTargetMicros := uint64(cfg.LatencyTarget * 1000)
	var latency = lTargetMicros

	r := NewResampler(lTargetMicros)
	lastPktNum := -1
	i := 0

	vitaPackets := make(chan flexclient.VitaPacket, int(0.5*cfg.LatencyTarget*48/256+1))
	var buf []float32
	var bufLock sync.RWMutex
	done := make(chan struct{})
	started := false
	startedChan := make(chan struct{})

	var stream *pulse.PlaybackStream

	stream, err = pc.NewPlayback(
		pulse.Float32Reader(func(out []float32) (int, error) {
			if !started {
				started = true
				startedChan <- struct{}{}
			}
			availToWrite := len(out)
			written := 0

			// First, copy out any bits of packet that may have been left over from last time.
			bufLock.Lock()
			if len(buf) > 0 {
				n := len(buf)
				if n > availToWrite {
					n = availToWrite
				}

				copy(out[:n], buf)
				copy(buf, buf[n:])
				buf = buf[:len(buf)-n]

				availToWrite -= n
				written += n
			}
			bufLock.Unlock()

			// Then, if there's still space, read and decode more packets, and fill the buffer.
			for availToWrite > 0 {
				// A hack. I don't know why the buffer gets over-full on startup
				// with all that's going on... probably has something to do with the
				// round-trip time to pulseaudio or something. In any case, be
				// willing to drop entire packets if we're far ahead (but not if
				// behind... if we have lots of un-decoded packets that pulse really
				// *wants* then let's decode them ASAP.
				lat := atomic.LoadUint64(&latency)
				for len(vitaPackets) > cap(vitaPackets)/3 && lat > lTargetMicros {
					<-vitaPackets
					lat -= 5333
				}

				pkt, ok := <-vitaPackets
				if !ok {
					done <- struct{}{}
					return written, pulse.EndOfData
				}

				if pkt.Preamble.Class_id.PacketClassCode == 0x03e3 && pkt.Preamble.Stream_id == StreamIDInt {
					pktNum := int(pkt.Preamble.Header.Packet_count)
					if lastPktNum != -1 {
						diff := (16 + pktNum - lastPktNum) % 16
						if diff != 1 {
							log.Println("discontinuity:", diff)
						}
					}
					lastPktNum = pktNum

					lat := atomic.LoadUint64(&latency)
					samples := r.ResamplePacket(pkt.Payload, lat)

					n := len(samples)
					if n > availToWrite {
						n = availToWrite
					}

					copy(out[written:written+n], samples[:n])
					written += n
					availToWrite -= n

					// If the last packet overfills the output space, then store the remainder
					if availToWrite == 0 {
						bufLock.Lock()
						buf = samples[n:]
						bufLock.Unlock()
					}

					i = (i + 1) % 375
					if cfg.DebugTiming && (i == 0 || i == 187) { /* once a second */
						msg := r.Stats(lat)
						log.Println(msg, len(vitaPackets), "/", cap(vitaPackets))
					}
				}
			}

			return written, nil
		}),
		pulse.PlaybackSink(sink),
		pulse.PlaybackSampleRate(48000),
		pulse.PlaybackMono,
		pulse.PlaybackLatency(cfg.LatencyTarget/1000),
		pulse.PlaybackMediaName("DAX RX "+cfg.Slice),
		pulse.PlaybackMediaIconName("radio"),
		pulse.PlaybackRawOption(func(c *proto.CreatePlaybackStream) {
			c.BufferMaxLength = c.BufferTargetLength * 4
			c.BufferPrebufferLength = c.BufferTargetLength - 9600
			if c.BufferPrebufferLength < 2048 {
				c.BufferPrebufferLength = 2048
			}
			c.AdjustLatency = false
			c.BufferMinimumRequest = 1024
		}),
	)

	if err != nil {
		panic(err)
	}

	go func() {
		updateLatency := time.NewTicker(100 * time.Millisecond)

		for {
			select {
			case <-updateLatency.C:
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
				//				log.Println(deviceLat, pulseBufferSamples, ourBufferSamples)
				lat := deviceLat + uint64(1e6*float64(pulseBufferSamples+ourBufferSamples)/48000)
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
		panic(err)
	}

	StreamIDInt := uint32(tmp)

	buf := ringbuffer.New(20 * 256 * 4)

	source, err := pc.SourceByID(cfg.Source + ".monitor")
	if err != nil {
		panic(err)
	}

	var pktCount uint16

	var stream *pulse.RecordStream
	stream, err = pc.NewRecord(
		pulse.Float32Writer(func(in []float32) (int, error) {
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
		panic(err)
	}

	stream.Start()
	defer stream.Close()

	<-exit
}

func main() {
	flag.Parse()

	var err error
	fc, err = flexclient.NewFlexClient(cfg.RadioIP)
	if err != nil {
		panic(err)
	}

	pc, err = pulse.NewClient(
		pulse.ClientApplicationName("nDAX"),
	)

	if err != nil {
		panic(err)
	}

	sinkIdx, err := createLoopback(cfg.Sink, "[INTERNAL] Flex RX Loopback", "emblem-symbolic-link", "Flex RX", "radio")
	if err != nil {
		panic(err)
	}
	defer destroyLoopback(sinkIdx)

	sourceIdx, err := createLoopback(cfg.Source, "Flex TX", "radio", "[INTERNAL] Flex TX Loopback", "emblem-symbolic-link")
	if err != nil {
		panic(err)
	}
	defer destroyLoopback(sourceIdx)

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
		_ = <-c
		log.Println("Exit on SIGINT")
		fc.Close()
	}()

	fc.StartUDP()

	bindClient()
	findSlice()
	enableDax()

	go streamToPulse()
	go streamFromPulse(stopTx)

	wg.Wait()
}
