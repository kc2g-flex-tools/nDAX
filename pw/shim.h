#pragma once

#include <stdint.h>
#include <stddef.h>

int ndax_pw_init(void);

// Creates a PipeWire stream that appears in the graph as a device node
// (media.class Audio/Source when is_sink=0, Audio/Sink when is_sink=1).
// prop_kv is n_props alternating key/value pairs of extra node properties.
// Returns an opaque handle, or NULL with a message in errbuf.
void *ndax_pw_stream_new(uintptr_t id, int is_sink,
                         const char *name, const char *desc,
                         int rate, int channels, int is_float,
                         int latency_frames,
                         const char **prop_kv, int n_props,
                         char *errbuf, size_t errlen);

void ndax_pw_stream_destroy(void *handle);
