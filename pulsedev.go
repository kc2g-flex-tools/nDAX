package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/hashicorp/go-version"
	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
	log "github.com/rs/zerolog/log"
)

type PulseSource struct {
	Index  uint32
	Handle *os.File
	Name   string
}

type PulseSink struct {
	Index  uint32
	Handle *os.File
	Name   string
}

var quoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quote(val string) string {
	return `"` + quoter.Replace(val) + `"`
}

func propList(kv ...string) string {
	out := ""
	for i := 0; i < len(kv)-1; i += 2 {
		if out != "" {
			out += " "
		}
		out += kv[i] + "=" + quote(kv[i+1])
	}
	return out
}

func createPipeSource(name, desc, icon string, latencyMs float64) (*PulseSource, error) {
	var err error
	var resp proto.LoadModuleReply
	var file *os.File

	bufferBits := int(float64(audioCfg.sampleRate*audioCfg.bytesPerSample) * latencyMs / 1000)

	tmpFile := "/tmp/nDAX-" + name + ".pipe"

	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-source",
			Args: propList(
				"source_name", name,
				"file", tmpFile,
				"rate", fmt.Sprintf("%d", audioCfg.sampleRate),
				"format", audioCfg.format,
				"channels", "1",
				"source_properties", fmt.Sprintf("device.buffering.buffer_size=%d device.icon_name=%s device.description='%s' nDAX.pid=%d", bufferBits, icon, desc, os.Getpid()),
			),
		},
		&resp,
	)

	if err != nil {
		return nil, fmt.Errorf("load-module module-pipe-source: %w", err)
	}

	if file, err = os.OpenFile(tmpFile, os.O_RDWR, 0755); err != nil {
		destroyModule(resp.ModuleIndex)
		return nil, fmt.Errorf("OpenFile %s: %w", tmpFile, err)
	}

	return &PulseSource{
		Index:  resp.ModuleIndex,
		Handle: file,
		Name:   name,
	}, nil
}

func createPipeSink(name, desc, icon string, channel int) (*PulseSink, error) {
	var err error
	var resp proto.LoadModuleReply
	var file *os.File

	tmpFile := "/tmp/nDAX-" + name + ".pipe"

	channels := "1"
	if channel != 0 {
		channels = "2"
	}
	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-sink",
			Args: propList(
				"sink_name", name,
				"file", tmpFile,
				"rate", fmt.Sprintf("%d", audioCfg.sampleRate),
				"format", audioCfg.format,
				"channels", channels,
				"use_system_clock_for_timing", "yes",
				"sink_properties", fmt.Sprintf("device.icon_name=%s device.description='%s' nDAX.pid=%d", icon, desc, os.Getpid()),
			),
		},
		&resp,
	)

	if err != nil {
		return nil, fmt.Errorf("load-module module-pipe-sink: %w", err)
	}

	if file, err = os.OpenFile(tmpFile, os.O_RDONLY, 0755); err != nil {
		destroyModule(resp.ModuleIndex)
		return nil, fmt.Errorf("OpenFile %s: %w", tmpFile, err)
	}

	return &PulseSink{
		Index:  resp.ModuleIndex,
		Handle: file,
		Name:   name,
	}, nil
}

func destroyModule(index uint32) error {
	err := pc.RawRequest(
		&proto.UnloadModule{
			ModuleIndex: index,
		},
		nil,
	)

	return err
}

func (s *PulseSource) Close() {
	if s.Handle != nil {
		s.Handle.Close()
	}
	destroyModule(s.Index)
}

func (s *PulseSink) Close() {
	if s.Handle != nil {
		s.Handle.Close()
	}
	destroyModule(s.Index)
}

func (s *PulseSource) Consume() (*pulse.RecordStream, error) {
	writer := pulse.NewWriter(io.Discard, proto.FormatInt16LE)
	psource, err := pc.SourceByID(s.Name)
	if err != nil {
		return nil, fmt.Errorf("looking up source: %w", err)
	}
	rec, err := pc.NewRecord(writer,
		pulse.RecordSampleRate(audioCfg.sampleRate),
		pulse.RecordSource(psource),
		pulse.RecordMediaName("self-consume"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating stream: %w", err)
	}
	rec.Start()
	return rec, nil
}


func processRunning(pid int) (bool, error) {
	sigErr := syscall.Kill(pid, syscall.Signal(0))
	if sigErr == nil {
		// No error means process was found
		return true, nil
	}
	if sigErr == syscall.ESRCH {
		// Process not found
		return false, nil
	}
	// Other error is an error.
	return false, sigErr
}

func checkPulseVersion() (bool, error) {
	var serverInfo proto.GetServerInfoReply
	err := pc.RawRequest(
		&proto.GetServerInfo{},
		&serverInfo,
	)
	log.Debug().Interface("serverInfo", serverInfo).Send()
	if err != nil {
		return false, err
	}
	ver, err := version.NewVersion(serverInfo.PackageVersion)
	if err != nil {
		return false, err
	}
	log.Debug().Str("version", ver.String()).Msg("found PulseAudio server version")

	if ver.LessThan(version.Must(version.NewVersion("12.0"))) {
		return false, fmt.Errorf("PulseAudio 12.0 or newer is required, server is version %s", serverInfo.PackageVersion)
	}

	if strings.Contains(serverInfo.PackageName, "PipeWire") {
		return true, nil
	} else {
		return false, nil
	}
}

func checkPulseConflicts() error {
	var sourceInfoList proto.GetSourceInfoListReply
	var sinkInfoList proto.GetSinkInfoListReply

	err := pc.RawRequest(
		&proto.GetSourceInfoList{},
		&sourceInfoList,
	)
	if err != nil {
		return err
	}

	for _, source := range sourceInfoList {
		if source.SourceName == cfg.Source {
			pidProp, ok := source.Properties["nDAX.pid"]
			if !ok {
				return fmt.Errorf("source %s already exists and doesn't have an nDAX.pid property", cfg.Source)
			}
			pid, err := strconv.Atoi(pidProp.String())
			if err != nil {
				return fmt.Errorf("parsing nDAX.pid: %w", err)
			}
			running, err := processRunning(pid)
			if err != nil {
				return fmt.Errorf("processrunning: %w", err)
			}
			if running {
				return fmt.Errorf("source %s was created by pid %d, which still appears to be running", cfg.Source, pid)
			}
			log.Warn().
				Int("pid", pid).
				Uint32("module_index", source.ModuleIndex).
				Msgf("source %s exists but was created by an nDAX process that no longer appears to be running. Will try to unload...", cfg.Source)

			err = pc.RawRequest(&proto.UnloadModule{ModuleIndex: source.ModuleIndex}, nil)
			if err != nil {
				return fmt.Errorf("unloading module %d failed", source.ModuleIndex)
			}
		}
	}

	err = pc.RawRequest(
		&proto.GetSinkInfoList{},
		&sinkInfoList,
	)
	if err != nil {
		return err
	}

	for _, sink := range sinkInfoList {
		if sink.SinkName == cfg.Sink {
			pidProp, ok := sink.Properties["nDAX.pid"]
			if !ok {
				return fmt.Errorf("sink %s already exists and doesn't have an nDAX.pid property", cfg.Sink)
			}
			pid, err := strconv.Atoi(pidProp.String())
			if err != nil {
				return fmt.Errorf("parsing nDAX.pid: %w", err)
			}
			running, err := processRunning(pid)
			if err != nil {
				return fmt.Errorf("processrunning: %w", err)
			}
			if running {
				return fmt.Errorf("sink %s was created by pid %d, which still appears to be running", cfg.Sink, pid)
			}
			log.Warn().
				Int("pid", pid).
				Uint32("module_index", sink.ModuleIndex).
				Msgf("sink %s exists but was created by an nDAX process that no longer appears to be running. Will try to unload...", cfg.Sink)

			err = pc.RawRequest(&proto.UnloadModule{ModuleIndex: sink.ModuleIndex}, nil)
			if err != nil {
				return fmt.Errorf("unloading module %d failed", sink.ModuleIndex)
			}
		}
	}

	return nil
}
