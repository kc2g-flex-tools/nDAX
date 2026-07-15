//go:build linux && cgo

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <pipewire/pipewire.h>
#include <spa/param/audio/format-utils.h>
#include <spa/utils/result.h>

#include "shim.h"
#include "_cgo_export.h"

static struct pw_thread_loop *ndax_loop;

int ndax_pw_init(void)
{
	pw_init(NULL, NULL);
	ndax_loop = pw_thread_loop_new("nDAX-pipewire", NULL);
	if (ndax_loop == NULL)
		return -1;
	if (pw_thread_loop_start(ndax_loop) < 0)
		return -1;
	return 0;
}

struct ndax_stream {
	struct pw_stream *stream;
	uintptr_t id;
	uint32_t stride;
};

static void on_process_source(void *data)
{
	struct ndax_stream *s = data;
	struct pw_buffer *b = pw_stream_dequeue_buffer(s->stream);
	if (b == NULL)
		return;
	struct spa_data *d = &b->buffer->datas[0];
	if (d->data != NULL) {
		uint32_t n_frames = d->maxsize / s->stride;
		if (b->requested != 0 && b->requested < n_frames)
			n_frames = b->requested;
		uint32_t size = n_frames * s->stride;
		goPWFill(s->id, d->data, size);
		d->chunk->offset = 0;
		d->chunk->stride = s->stride;
		d->chunk->size = size;
	}
	pw_stream_queue_buffer(s->stream, b);
}

static void on_process_sink(void *data)
{
	struct ndax_stream *s = data;
	struct pw_buffer *b = pw_stream_dequeue_buffer(s->stream);
	if (b == NULL)
		return;
	struct spa_data *d = &b->buffer->datas[0];
	if (d->data != NULL && d->chunk->size > 0)
		goPWDrain(s->id, SPA_PTROFF(d->data, d->chunk->offset, void), d->chunk->size);
	pw_stream_queue_buffer(s->stream, b);
}

static const struct pw_stream_events source_events = {
	PW_VERSION_STREAM_EVENTS,
	.process = on_process_source,
};

static const struct pw_stream_events sink_events = {
	PW_VERSION_STREAM_EVENTS,
	.process = on_process_sink,
};

void *ndax_pw_stream_new(uintptr_t id, int is_sink,
                         const char *name, const char *desc,
                         int rate, int channels, int is_float,
                         int latency_frames,
                         const char **prop_kv, int n_props,
                         char *errbuf, size_t errlen)
{
	struct pw_properties *props = pw_properties_new(
		PW_KEY_MEDIA_TYPE, "Audio",
		PW_KEY_MEDIA_CLASS, is_sink ? "Audio/Sink" : "Audio/Source",
		PW_KEY_NODE_NAME, name,
		PW_KEY_NODE_DESCRIPTION, desc,
		PW_KEY_NODE_VIRTUAL, "true",
		NULL);
	if (props == NULL) {
		snprintf(errbuf, errlen, "pw_properties_new failed");
		return NULL;
	}
	pw_properties_setf(props, PW_KEY_NODE_LATENCY, "%d/%d", latency_frames, rate);
	for (int i = 0; i + 1 < 2 * n_props; i += 2)
		pw_properties_set(props, prop_kv[i], prop_kv[i + 1]);

	struct ndax_stream *s = calloc(1, sizeof(*s));
	if (s == NULL) {
		pw_properties_free(props);
		snprintf(errbuf, errlen, "out of memory");
		return NULL;
	}
	s->id = id;
	s->stride = (is_float ? 4 : 2) * channels;

	pw_thread_loop_lock(ndax_loop);

	// pw_stream_new_simple takes ownership of props.
	s->stream = pw_stream_new_simple(pw_thread_loop_get_loop(ndax_loop),
	                                 name, props,
	                                 is_sink ? &sink_events : &source_events, s);
	if (s->stream == NULL) {
		pw_thread_loop_unlock(ndax_loop);
		free(s);
		snprintf(errbuf, errlen, "pw_stream_new_simple failed");
		return NULL;
	}

	uint8_t buffer[1024];
	struct spa_pod_builder bld = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	struct spa_audio_info_raw info = {
		.format = is_float ? SPA_AUDIO_FORMAT_F32_BE : SPA_AUDIO_FORMAT_S16_BE,
		.rate = rate,
		.channels = channels,
	};
	if (channels == 1) {
		info.position[0] = SPA_AUDIO_CHANNEL_MONO;
	} else {
		info.position[0] = SPA_AUDIO_CHANNEL_FL;
		info.position[1] = SPA_AUDIO_CHANNEL_FR;
	}
	const struct spa_pod *params[1];
	params[0] = spa_format_audio_raw_build(&bld, SPA_PARAM_EnumFormat, &info);

	int res = pw_stream_connect(s->stream,
	                            is_sink ? PW_DIRECTION_INPUT : PW_DIRECTION_OUTPUT,
	                            PW_ID_ANY,
	                            PW_STREAM_FLAG_MAP_BUFFERS,
	                            params, 1);
	if (res < 0) {
		pw_stream_destroy(s->stream);
		pw_thread_loop_unlock(ndax_loop);
		free(s);
		snprintf(errbuf, errlen, "pw_stream_connect: %s", spa_strerror(res));
		return NULL;
	}

	pw_thread_loop_unlock(ndax_loop);
	return s;
}

void ndax_pw_stream_destroy(void *handle)
{
	struct ndax_stream *s = handle;
	pw_thread_loop_lock(ndax_loop);
	pw_stream_destroy(s->stream);
	pw_thread_loop_unlock(ndax_loop);
	free(s);
}
