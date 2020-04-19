package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arodland/flexclient"
	"github.com/jfreymuth/pulse"
)

var cfg struct {
	RadioIP string
	Station string
	Slice   string
	Sink    string
	Source  string
	DaxCh   string
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", "192.168.1.67", "radio IP address")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Slice, "slice", "A", "Slice letter to use")
	flag.StringVar(&cfg.DaxCh, "daxch", "1", "DAX channel # to use")
	flag.StringVar(&cfg.Sink, "sink", "flexdax.rx", "PulseAudio sink to send audio to")
	flag.StringVar(&cfg.Source, "source", "flexdax.tx.monitor", "PulseAudio sink to receive from")
}

var fc *flexclient.FlexClient
var pc *pulse.Client
var ClientID string
var ClientUUID string
var SliceIdx string
var RXStreamID string
var TXStreamID string

func bindClient() {
	fmt.Println("Waiting for station:", cfg.Station)

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

	fmt.Println("Found client ID", ClientID, "UUID", ClientUUID)

	fc.SendAndWait("client bind client_id=" + ClientUUID)
}

func findSlice() {
	fmt.Println("Looking for slice:", cfg.Slice)
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
	fmt.Println("Found slice", SliceIdx)
}

func enableDax() {
	fc.SliceSet(SliceIdx, flexclient.Object{"dax": cfg.DaxCh})
	fc.SendAndWait("dax audio set " + cfg.DaxCh + " slice=" + SliceIdx + " tx=1")

	res := fc.SendAndWait("stream create type=dax_rx dax_channel=" + cfg.DaxCh)
	if res.Error != 0 {
		panic(res)
	}

	RXStreamID = res.Message
	fmt.Println("enabled RX DAX stream", RXStreamID)

	fc.SendAndWait(fmt.Sprintf("audio stream 0x%s slice %s gain %d", RXStreamID, SliceIdx, 50))

	res = fc.SendAndWait("stream create type=dax_tx" + cfg.DaxCh)
	if res.Error != 0 {
		panic(res)
	}

	TXStreamID = res.Message

	fmt.Println("enabled TX DAX stream", TXStreamID)
}

func decodeFloat32(data []byte) []float32 {
	var sample float32
	out := make([]float32, len(data)/4)
	i := 0

	b := bytes.NewReader(data)

	for {
		err := binary.Read(b, binary.BigEndian, &sample)
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		out[i] = sample
		i += 1
	}
	return out
}

func encodeFloat32(samples []float32) []byte {
	var out bytes.Buffer
	for _, sample := range samples {
		binary.Write(&out, binary.BigEndian, sample)
	}
	return out.Bytes()
}

func streamToPulse() {
	sink, err := pc.SinkByID(cfg.Sink)
	if err != nil {
		panic(err)
	}

	vitaPackets := make(chan flexclient.VitaPacket, 10)
	fc.SetVitaChan(vitaPackets)

	buf := make([]float32, 0, 4096)
	var stream *pulse.PlaybackStream
	var start time.Time
	var sampIn, sampOut float64
	var m sync.RWMutex

	stream, err = pc.NewPlayback(
		func(out []float32) int {
			m.RLock()
			defer m.RUnlock()

			n := len(out)
			if len(buf) < n {
				n = len(buf)
			}

			copy(out, buf[:n])
			copy(buf, buf[n:])
			buf = buf[:len(buf)-n]
			sampOut += float64(n)
			elapsed := time.Now().Sub(start)
			_ = elapsed
			//			fmt.Printf("%.3f\t%.3f\t%d\t%d\t%d\n", sampIn/elapsed.Seconds(), sampOut/elapsed.Seconds(), len(out), n, len(buf))
			return n
		},
		pulse.PlaybackSink(sink),
		pulse.PlaybackMono,
		pulse.PlaybackSampleRate(48000),
		pulse.PlaybackLatency(0.1),
	)

	if err != nil {
		panic(err)
	}

	start = time.Now()
	for pkt := range vitaPackets {
		if pkt.Preamble.Class_id.PacketClassCode == 0x03e3 {
			m.Lock()
			rawSamples := decodeFloat32(pkt.Payload)
			buf = append(buf, rawSamples...)
			sampIn += float64(len(rawSamples))
			l := len(buf)
			m.Unlock()
			if l >= 4800 {
				stream.Start()
			}
		}
	}

	stream.Stop()
	stream.Close()
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func streamFromPulse(wg *sync.WaitGroup) {
	tmp, err := strconv.ParseUint(TXStreamID, 16, 32)
	if err != nil {
		panic(err)
	}

	StreamIDInt := uint32(tmp)
	source, err := pc.SourceByID(cfg.Source)
	if err != nil {
		panic(err)
	}

	buf := make([]float32, 0, 4096)
	var pktCount int16

	var stream *pulse.RecordStream

	stream, err = pc.NewRecord(
		func(in []float32) {
			const n = 256

			buf = append(buf, in...)
			for len(buf) >= n {
				rawSamples := encodeFloat32(buf[:n])
				copy(buf, buf[n:])
				buf = buf[:len(buf)-n]
				if allZero(rawSamples) {
					continue
				}

				var pkt bytes.Buffer
				pkt.WriteByte(0x18)
				pkt.WriteByte(0xd0 | byte(pktCount&0xf))
				pktCount += 1
				binary.Write(&pkt, binary.BigEndian, uint16(n+7))
				binary.Write(&pkt, binary.BigEndian, StreamIDInt)
				binary.Write(&pkt, binary.BigEndian, uint64(0x00001c2d534c03e3))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				binary.Write(&pkt, binary.BigEndian, uint32(0x00000000))
				pkt.Write(rawSamples)
				fc.SendUdp(pkt.Bytes())
			}
		},
		pulse.RecordSource(source),
		pulse.RecordMono,
		pulse.RecordSampleRate(48000),
		pulse.RecordLatency(0.150),
	)

	if err != nil {
		panic(err)
	}

	stream.Start()
	defer stream.Close()
	wg.Wait()
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
		pulse.ClientApplicationIconName("radio"),
	)

	if err != nil {
		panic(err)
	}

	defer pc.Close()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		fc.Run()
		wg.Done()
	}()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		_ = <-c
		fmt.Println("Exit on SIGINT")
		fc.Close()
	}()

	fc.StartUDP()

	bindClient()
	findSlice()
	enableDax()
	go streamToPulse()
	time.Sleep(100 * time.Millisecond)
	go streamFromPulse(&wg)

	wg.Wait()
}
