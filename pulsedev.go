package main

import (
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

func createLoopback(sinkName, desc, icon, monitorDesc, monitorIcon string) (uint32, error) {
	var err error
	var resp proto.LoadModuleReply

	err = pc.RawRequest(
		&proto.LoadModule{
			Name: "module-null-sink",
			Args: propList("sink_name", sinkName, "rate", "48000", "format", "float32be"),
		},
		&resp,
	)

	if err != nil {
		return 0, err
	}

	// Yes, there's really no other way to do this; these two commands
	// are *not* part of the native protocol.
	pcli.Send("update-sink-proplist " + sinkName + " " + propList(
		"device.description", desc,
		"device.icon_name", icon,
	))

	pcli.Send("update-source-proplist " + sinkName + ".monitor " + propList(
		"device.description", monitorDesc,
		"device.icon_name", monitorIcon,
	))

	return resp.ModuleIndex, nil
}

func destroyLoopback(index uint32) error {
	err := pc.RawRequest(
		&proto.UnloadModule{
			ModuleIndex: index,
		},
		nil,
	)

	return err
}
