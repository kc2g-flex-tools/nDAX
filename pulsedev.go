package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfreymuth/pulse/proto"
)

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

func createPipeSource(name, desc, icon string, latencyMs float64) (uint32, *os.File, error) {
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
				"source_properties", fmt.Sprintf("device.buffering.buffer_size=%d", bufferBits),
			),
		},
		&resp,
	)

	if err != nil {
		return 0, nil, fmt.Errorf("load-module module-pipe-source: %w", err)
	}

	pcli.Send("update-source-proplist " + name + " " + propList(
		"device.description", desc,
		"device.icon_name", icon,
	))

	if file, err = os.OpenFile(tmpFile, os.O_RDWR, 0755); err != nil {
		destroyModule(resp.ModuleIndex)
		return 0, nil, fmt.Errorf("OpenFile %s: %w", tmpFile, err)
	}

	return resp.ModuleIndex, file, nil
}

func createPipeSink(name, desc, icon string) (uint32, *os.File, error) {
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
			),
		},
		&resp,
	)

	if err != nil {
		return 0, nil, fmt.Errorf("load-module module-pipe-sink: %w", err)
	}

	pcli.Send("update-sink-proplist " + name + " " + propList(
		"device.description", desc,
		"device.icon_name", icon,
	))

	if file, err = os.OpenFile(tmpFile, os.O_RDONLY, 0755); err != nil {
		destroyModule(resp.ModuleIndex)
		return 0, nil, fmt.Errorf("OpenFile %s: %w", tmpFile, err)
	}

	return resp.ModuleIndex, file, nil
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

func ensureCLI() error {
	modules, err := getModules()
	if err != nil {
		return fmt.Errorf("get pulse module list: %w", err)
	}

	for _, module := range modules {
		if module.ModuleName == "module-cli-protocol-unix" {
			return nil // already loaded
		}
	}

	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-cli-protocol-unix",
			Args: "",
		},
		nil,
	)

	if err != nil {
		return fmt.Errorf("loadmodule module-cli-protocol-unix: %w", err)
	}
	return nil
}
