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
	"time"

	"github.com/arodland/flexclient"
	"github.com/mesilliac/pulse-simple"
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
	flag.StringVar(&cfg.RadioIP, "radio", "192.168.1.67", "radio IP address")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Slice, "slice", "A", "Slice letter to use")
	flag.StringVar(&cfg.DaxCh, "daxch", "1", "DAX channel # to use")
	flag.StringVar(&cfg.Sink, "sink", "flexdax.rx", "PulseAudio sink to send audio to")
	flag.StringVar(&cfg.Source, "source", "flexdax.tx.monitor", "PulseAudio sink to receive from")
	flag.Float64Var(&cfg.LatencyTarget, "latency", 100, "Target RX latency (ms, higher = less sample rate variation)")
	flag.BoolVar(&cfg.DebugTiming, "debug-timing", false, "Print debug messages about buffer timing and resampling")
}

var fc *flexclient.FlexClient
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

	lTargetSamples := 2 * 48000 * (cfg.LatencyTarget / 1000)

	stream, err := pulse.NewStream(
		"",
		"nDAX",
		pulse.STREAM_PLAYBACK,
		cfg.Sink,
		"DAX RX "+cfg.Slice,
		&pulse.SampleSpec{
			Format:   pulse.SAMPLE_FLOAT32BE,
			Rate:     48000,
			Channels: 1,
		},
		nil,
		&pulse.BufferAttr{
			Tlength:   uint32(lTargetSamples * 4),
			Maxlength: ^uint32(0),
			Prebuf:    uint32(lTargetSamples) * 4,
			Minreq:    ^uint32(0),
			Fragsize:  ^uint32(0),
		},
	)

	if err != nil {
		panic(err)
	}

	defer stream.Free()

	vitaPackets := make(chan flexclient.VitaPacket, 20)
	fc.SetVitaChan(vitaPackets)

	r := NewResampler(cfg.LatencyTarget * 1000)
	lastPktNum := -1
	i := 0

	for pkt := range vitaPackets {
		if pkt.Preamble.Class_id.PacketClassCode == 0x03e3 && pkt.Preamble.Stream_id == StreamIDInt {
			lat, _ := stream.Latency()
			lat += 5333 * uint64(len(vitaPackets))

			pktNum := int(pkt.Preamble.Header.Packet_count)
			if lastPktNum != -1 {
				diff := (16 + pktNum - lastPktNum) % 16
				if diff != 1 {
					log.Println("discontinuity:", diff)
				}
			}
			lastPktNum = pktNum

			bytes := r.ResamplePacket(pkt.Payload, lat)

			wrote, err := stream.Write(bytes)
			if err != nil {
				log.Println("pulse write error:", err.Error())
			}
			if wrote < len(bytes) {
				log.Println("Short write to pulse, wanted ", len(bytes), "got", wrote)
			}

			i = (i + 1) % 375
			if cfg.DebugTiming && (i == 0 || i == 187) { /* once a second */
				msg := r.Stats(lat)
				log.Println(msg)
			}
		}
	}

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

	stream, err := pulse.NewStream(
		"",
		"nDAX",
		pulse.STREAM_RECORD,
		cfg.Source,
		"DAX TX "+cfg.Slice,
		&pulse.SampleSpec{
			Format:   pulse.SAMPLE_FLOAT32BE,
			Rate:     48000,
			Channels: 1,
		},
		nil,
		&pulse.BufferAttr{
			Maxlength: 20 * 256 * 4, // 20 packets, about 100ms
			Tlength:   ^uint32(0),
			Prebuf:    ^uint32(0),
			Minreq:    ^uint32(0),
			Fragsize:  256 * 4, // req 1 packet at a time exactly, we hope :)
		},
	)

	if err != nil {
		panic(err)
	}

	defer stream.Free()

	var pktCount int16

READ:
	for {
		select {
		case <-exit:
			break READ
		default:
		}
		const pktSize = 256 * 4
		var readBuf [pktSize]byte
		n, err := stream.Read(readBuf[:])
		if err != nil {
			log.Println(err)
			break
		}
		buf.Write(readBuf[:n])

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
			binary.Write(&pkt, binary.BigEndian, uint16(n/4+7))
			binary.Write(&pkt, binary.BigEndian, StreamIDInt)
			binary.Write(&pkt, binary.BigEndian, uint64(0x00001c2d534c03e3))
			binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
			binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
			binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
			pkt.Write(rawSamples[:])
			fc.SendUdp(pkt.Bytes())
			time.Sleep(1 * time.Millisecond)
		}
	}
}

func main() {
	flag.Parse()

	var err error
	fc, err = flexclient.NewFlexClient(cfg.RadioIP)
	if err != nil {
		panic(err)
	}

	if err != nil {
		panic(err)
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
