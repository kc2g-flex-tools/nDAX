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
	"time"

	"github.com/jfreymuth/pulse"
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
	TX            bool
	Realtime      bool
	LogLevel      string
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", ":discover:", "radio IP address or discovery spec")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Slice, "slice", "A", "Slice letter to use")
	flag.StringVar(&cfg.DaxCh, "daxch", "1", "DAX channel # to use")
	flag.StringVar(&cfg.Source, "source", "flexdax.rx", "PulseAudio source for received audio")
	flag.StringVar(&cfg.Sink, "sink", "flexdax.tx", "PulseAudio sink for audio to transmit")
	flag.Float64Var(&cfg.LatencyTarget, "latency", 100, "Target RX latency (ms, higher = less sample rate variation)")
	flag.BoolVar(&cfg.TX, "tx", true, "Create a TX audio device")
	flag.BoolVar(&cfg.Realtime, "rt", true, "Attempt to acquire realtime priority")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "minimum level of messages to log to console (trace, debug, info, warn, error)")
}

var fc *flexclient.FlexClient
var pc *pulse.Client
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

func streamToPulse(pipe *os.File) {
	vitaPackets := make(chan flexclient.VitaPacket, int(cfg.LatencyTarget*48/256+100))
	fc.SetVitaChan(vitaPackets)

	if cfg.Realtime {
		requestRealtime("rx thread", 19)
	}

	for {
		select {
		case pkt, ok := <-vitaPackets:
			if !ok {
				pipe.Close()
				return
			}
			_, err := pipe.Write(pkt.Payload)
			if err != nil {
				log.Warn().Err(err).Msg("pipe write")
			}
		}
	}
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func streamFromPulse(pipe *os.File, exit chan struct{}) {
	tmp, err := strconv.ParseUint(TXStreamID, 16, 32)
	if err != nil {
		log.Fatal().Err(err).Msg("Parse TXStreamID failed")
	}

	StreamIDInt := uint32(tmp)

	const pktSize = 256 * 4

	buf := ringbuffer.New(20 * pktSize)

	var readBuf [pktSize]byte

	if cfg.Realtime {
		requestRealtime("tx thread", 19)
	}

	var pktCount uint16

	for {
		n, err := pipe.Read(readBuf[:])
		if err != nil {
			log.Error().Err(err).Msg("pipe read")
			return
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
	}
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

	sourceIdx, sourcePipe, err := createPipeSource(cfg.Source, "Flex RX", "radio", cfg.LatencyTarget)
	if err != nil {
		log.Fatal().Err(err).Msg("Create RX pipe failed")
	}
	defer destroyModule(sourceIdx)

	var sinkIdx uint32
	var sinkPipe *os.File

	if cfg.TX {
		sinkIdx, sinkPipe, err = createPipeSink(cfg.Sink, "Flex TX", "radio")
		if err != nil {
			log.Fatal().Err(err).Msg("Create TX pipe failed")
		}
		defer destroyModule(sinkIdx)
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

	go streamToPulse(sourcePipe)

	if cfg.TX {
		go streamFromPulse(sinkPipe, stopTx)
	}

	wg.Wait()
}
