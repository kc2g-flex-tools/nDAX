package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/jfreymuth/pulse/proto"
	log "github.com/rs/zerolog/log"
)

type PulseSource struct {
	Index  uint32
	Handle *os.File
}

type PulseSink struct {
	Index  uint32
	Handle *os.File
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

	bufferBits := int(48000 * 4 * 1 * latencyMs / 1000)

	tmpFile := "/tmp/nDAX-" + name + ".pipe"

	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-source",
			Args: propList(
				"source_name", name,
				"file", tmpFile,
				"rate", "48000",
				"format", "float32be",
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
	}, nil
}

func createPipeSink(name, desc, icon string) (*PulseSink, error) {
	var err error
	var resp proto.LoadModuleReply
	var file *os.File

	tmpFile := "/tmp/nDAX-" + name + ".pipe"

	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-sink",
			Args: propList(
				"sink_name", name,
				"file", tmpFile,
				"rate", "48000",
				"format", "float32be",
				"channels", "1",
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

func getModules() ([]*proto.GetModuleInfoReply, error) {
	var ret proto.GetModuleInfoListReply
	err := pc.RawRequest(
		&proto.GetModuleInfoList{},
		&ret,
	)
	if err != nil {
		return nil, err
	} else {
		return []*proto.GetModuleInfoReply(ret), nil
	}
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

			err = pc.RawRequest(&proto.UnloadModule{source.ModuleIndex}, nil)
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

			err = pc.RawRequest(&proto.UnloadModule{sink.ModuleIndex}, nil)
			if err != nil {
				return fmt.Errorf("unloading module %d failed", sink.ModuleIndex)
			}
		}
	}

	return nil
}
