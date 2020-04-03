package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	"github.com/arodland/flexclient"
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

func rescale(data []byte, scale float32) []byte {
	b := bytes.NewReader(data)
	var out bytes.Buffer

	var sample float32
	for {
		err := binary.Read(b, binary.BigEndian, &sample)
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}

		binary.Write(&out, binary.BigEndian, sample*scale)
	}

	return out.Bytes()
}

func rescaleToInt(data []byte, scale float32) []byte {
	b := bytes.NewReader(data)
	var out bytes.Buffer

	var sample float32
	for {
		err := binary.Read(b, binary.BigEndian, &sample)
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}

		binary.Write(&out, binary.BigEndian, uint16(sample*scale))
	}

	return out.Bytes()
}

func streamToPulse() {
	cmd := exec.Command("pacat", "--device", cfg.Sink, "--rate=48000", "--channels=1", "--format=float32be", "--latency-msec=25", "/dev/stdin")
	pipe, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		panic(err)
	}

	vitaPackets := make(chan flexclient.VitaPacket, 3)
	fc.SetVitaChan(vitaPackets)
	for pkt := range vitaPackets {
		if pkt.Preamble.Class_id.PacketClassCode == 0x03e3 {
			pipe.Write(pkt.Payload)
		}
	}

	pipe.Close()
	cmd.Wait()
}

func streamFromPulse() {
	tmp, err := strconv.ParseUint(TXStreamID, 16, 32)
	if err != nil {
		panic(err)
	}

	StreamIDInt := uint32(tmp)

	cmd := exec.Command("pacat", "-r", "--device", cfg.Source, "--rate=48000", "--channels=1", "--format=float32be", "--latency-msec=25", "/dev/stdout")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	cmd.Stderr = os.Stderr

	err = cmd.Start()
	if err != nil {
		panic(err)
	}

	var buf [1024]byte
	var pktCount int16
	for {
		n, err := io.ReadFull(pipe, buf[:])
		if err != nil {
			fmt.Println(err)
			break
		}
		//		rescaled := rescale(buf[:n], 32767)

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
		pkt.Write(buf[:n])
		fc.SendUdp(pkt.Bytes())
	}
}

func main() {
	flag.Parse()

	var err error
	fc, err = flexclient.NewFlexClient(cfg.RadioIP)
	if err != nil {
		panic(err)
	}

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
	go streamFromPulse()

	wg.Wait()
}
