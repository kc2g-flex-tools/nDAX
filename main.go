package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"

	"github.com/arodland/flexclient"
)

var cfg struct {
	RadioIP string
	Station string
	Slice   string
	Sink    string
	DaxCh   string
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", "192.168.1.67", "radio IP address")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Slice, "slice", "A", "Slice letter to use")
	flag.StringVar(&cfg.DaxCh, "daxch", "1", "DAX channel # to use")
	flag.StringVar(&cfg.Sink, "sink", "flexdax.rx", "PulseAudio sink to send audio to")
}

var fc *flexclient.FlexClient
var ClientID string
var ClientUUID string
var SliceIdx string
var StreamID string

func bindClient() {
	fmt.Println("Waiting for station:", cfg.Station)

	clients := make(chan flexclient.StateUpdate, 10)
	sub := fc.Subscribe(flexclient.Subscription{"client ", clients})
	fc.SendAndWait("sub client all")

	for upd := range clients {
		if upd.CurrentState["station"] == cfg.Station {
			ClientID = strings.TrimPrefix(upd.Object, "client ")
			ClientUUID = upd.CurrentState["client_id"]
			break
		}
	}

	fc.Unsubscribe(sub)

	fmt.Println("Found client ID", ClientID, "UUID", ClientUUID)

	fc.SendAndWait("client bind client_id=" + ClientUUID)
}

func findSlice() {
	fmt.Println("Looking for slice:", cfg.Slice)
	slices := make(chan flexclient.StateUpdate, 10)
	sub := fc.Subscribe(flexclient.Subscription{"slice ", slices})
	fc.SendAndWait("sub slice all")

	for upd := range slices {
		if upd.CurrentState["index_letter"] == cfg.Slice && upd.CurrentState["client_handle"] == ClientID {
			SliceIdx = strings.TrimPrefix(upd.Object, "slice ")
			break
		}
	}

	fc.Unsubscribe(sub)
	fmt.Println("Found slice", SliceIdx)

}

func enableDax() {
	fc.SendAndWait("slice set " + SliceIdx + "dax=" + cfg.DaxCh)
	res := fc.SendAndWait("stream create type=dax_rx dax_channel=" + cfg.DaxCh)
	if res.Error != 0 {
		panic(res)
	}
	StreamID = res.Message
	fmt.Println("enabled DAX stream", StreamID)
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

func main() {
	flag.Parse()

	var err error
	fc, err = flexclient.NewFlexClient(cfg.RadioIP + ":4992")
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
	streamToPulse()

	wg.Wait()
}
