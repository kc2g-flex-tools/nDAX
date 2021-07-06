package main

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	PacketBuffer  int
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
	flag.IntVar(&cfg.PacketBuffer, "packet-buffer", 3, "Buffer n (max 6) packets against reordering and loss")
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

var byteReader bytes.Reader

func readPacketsBuffered(pktIn chan flexclient.VitaPacket, payloadsOut chan []byte) {
	var readPoint = -1
	var buf [16]*flexclient.VitaPacket
	var ct, reordered, lost int
	lastPayload := make([]byte, 1024)

	if cfg.Realtime {
		requestRealtime("rx thread", 19)
	}

	for pkt := range pktIn {
		pktNum := int(pkt.Preamble.Header.Packet_count)
		buf[pktNum] = &pkt
		if buf[(pktNum+1)%16] != nil {
			reordered += 1
		}

		if readPoint == -1 {
			readPoint = pktNum
		}
		ahead := (16 + pktNum - readPoint) % 16
		for ahead >= cfg.PacketBuffer {
			if buf[readPoint] != nil {
				payloadsOut <- buf[readPoint].Payload
				lastPayload = buf[readPoint].Payload
				buf[readPoint] = nil
			} else {
				lost += 1
				payloadsOut <- lastPayload
			}
			readPoint = (readPoint + 1) % 16
			ahead = (16 + pktNum - readPoint) % 16
		}

		ct += 1

		if ct == 187 || ct == 375 {
			if reordered > 0 || lost > 0 {
				log.Debug().Int("reordered", reordered).Int("lost", lost).Msg("packet buffer")
			}
			reordered, lost = 0, 0
			if ct == 375 {
				ct = 0
			}
		}
	}
	close(payloadsOut)
}

func readPacketsUnbuffered(pktIn chan flexclient.VitaPacket, payloadsOut chan []byte) {
	if cfg.Realtime {
		requestRealtime("rx thread", 19)
	}

	for pkt := range pktIn {
		payloadsOut <- pkt.Payload
	}
	close(payloadsOut)
}

func streamToPulse(source *PulseSource) {
	vitaPackets := make(chan flexclient.VitaPacket, int(cfg.LatencyTarget*48/256+100))
	fc.SetVitaChan(vitaPackets)
	payloads := make(chan []byte)

	if cfg.PacketBuffer > 0 {
		go readPacketsBuffered(vitaPackets, payloads)
	} else {
		go readPacketsUnbuffered(vitaPackets, payloads)
	}

	for {
		select {
		case payload, ok := <-payloads:
			if !ok {
				source.Close()
				return
			}
			_, err := source.Handle.Write(payload)
			if errors.Is(err, os.ErrClosed) {
				return
			} else if err != nil {
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

func streamFromPulse(sink *PulseSink, exit chan struct{}) {
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
		n, err := sink.Handle.Read(readBuf[:])
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				log.Error().Err(err).Msg("pipe read")
			}
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

	if cfg.PacketBuffer < 0 || cfg.PacketBuffer > 14 {
		log.Fatal().Msgf("-packet-buffer must be between 0 and 14")
	}

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

	err = checkPulseVersion()
	if err != nil {
		log.Fatal().Err(err).Send()
	}

	err = checkPulseConflicts()
	if err != nil {
		log.Fatal().Err(err).Send()
	}

	source, err := createPipeSource(cfg.Source, "Flex RX", "radio", cfg.LatencyTarget)
	if err != nil {
		log.Fatal().Err(err).Msg("Create RX pipe failed")
	}
	defer source.Close()

	var sink *PulseSink

	if cfg.TX {
		sink, err = createPipeSink(cfg.Sink, "Flex TX", "radio")
		if err != nil {
			log.Fatal().Err(err).Msg("Create TX pipe failed")
		}
		defer sink.Close()
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

	go streamToPulse(source)

	if cfg.TX {
		go streamFromPulse(sink, stopTx)
	}

	wg.Wait()
}
