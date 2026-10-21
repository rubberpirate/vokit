#include "phoneaudio.h"
#include <QProcess>
#include <QStandardPaths>
#include <QThread>
#include <QUuid>
#include <functional>
#include <cstring>
extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/channel_layout.h>
#include <libswresample/swresample.h>
}

namespace {
class AudioDecoder {
public:
    ~AudioDecoder() { swr_free(&resampler); av_frame_free(&frame); av_packet_free(&packet); avcodec_free_context(&context); }
    bool push(QByteArray data, const std::function<bool(const QByteArray &)> &consume)
    {
        if (data.isEmpty() || data.size() > 512 * 1024) return false;
        if (!context) {
            const AVCodec *codec = avcodec_find_decoder(AV_CODEC_ID_AAC);
            context = avcodec_alloc_context3(codec);
            frame = av_frame_alloc(); packet = av_packet_alloc();
            if (!codec || !context || !frame || !packet || avcodec_open2(context, codec, nullptr) < 0) return false;
        }
        qsizetype offset = 0;
        while (offset < data.size()) {
            const auto *bytes = reinterpret_cast<const uchar *>(data.constData() + offset);
            const qsizetype remaining = data.size() - offset;
            if (remaining < 7 || bytes[0] != 0xff || (bytes[1] & 0xf6) != 0xf0 || ((bytes[2] >> 2) & 15) >= 13) return false;
            const int size = ((bytes[3] & 3) << 11) | (bytes[4] << 3) | (bytes[5] >> 5);
            if (size <= ((bytes[1] & 1) ? 7 : 9) || size > remaining) return false;
            if (av_new_packet(packet, size) < 0) return false;
            std::memcpy(packet->data, bytes, size);
            const int sent = avcodec_send_packet(context, packet);
            av_packet_unref(packet);
            if (sent < 0) return false;
            for (;;) {
                const int result = avcodec_receive_frame(context, frame);
                if (result == AVERROR(EAGAIN) || result == AVERROR_EOF) break;
                if (result < 0 || frame->sample_rate < 8000 || frame->sample_rate > 192000 || frame->ch_layout.nb_channels < 1 || frame->ch_layout.nb_channels > 8 || frame->nb_samples > 8192) return false;
                if (!resampler || rate != frame->sample_rate || format != frame->format || channels != frame->ch_layout.nb_channels) {
                    swr_free(&resampler);
                    AVChannelLayout mono = AV_CHANNEL_LAYOUT_MONO;
                    if (swr_alloc_set_opts2(&resampler, &mono, AV_SAMPLE_FMT_S16, 48000,
                                            &frame->ch_layout, AVSampleFormat(frame->format), frame->sample_rate, 0, nullptr) < 0 || swr_init(resampler) < 0) return false;
                    rate = frame->sample_rate; format = frame->format; channels = frame->ch_layout.nb_channels;
                }
                const int count = int(av_rescale_rnd(swr_get_delay(resampler, rate) + frame->nb_samples, 48000, rate, AV_ROUND_UP));
                if (count < 0 || count > 65536) return false;
                QByteArray pcm(count * 2, 0);
                uint8_t *out[]{reinterpret_cast<uint8_t *>(pcm.data())};
                const int converted = swr_convert(resampler, out, count, const_cast<const uint8_t **>(frame->extended_data), frame->nb_samples);
                av_frame_unref(frame);
                if (converted < 0) return false;
                pcm.resize(converted * 2);
                if (!pcm.isEmpty() && !consume(pcm)) return false;
            }
            offset += size;
        }
        return true;
    }
private:
    AVCodecContext *context = nullptr;
    AVPacket *packet = nullptr;
    AVFrame *frame = nullptr;
    SwrContext *resampler = nullptr;
    int rate = 0, format = -1, channels = 0;
};
}

struct PhoneAudio::Impl {
    AudioDecoder decoder;
    QProcess loop, feed;
    QString message = "Waiting for phone microphone audio…";
    bool failed = false;
    void stop()
    {
        for (auto *process : {&feed, &loop}) {
            process->closeWriteChannel();
            if (process->state() != QProcess::NotRunning) {
                process->terminate();
                if (!process->waitForFinished(500)) { process->kill(); process->waitForFinished(500); }
            }
        }
    }
    bool start()
    {
        if (feed.state() == QProcess::Running && loop.state() == QProcess::Running) return true;
        if (failed) return false;
        const QString loopTool = QStandardPaths::findExecutable("pw-loopback"), feedTool = QStandardPaths::findExecutable("pw-cat");
        if (loopTool.isEmpty() || feedTool.isEmpty()) { message = "Install PipeWire tools to share phone microphone audio."; failed = true; return false; }
        const QString id = "vivo-phone-" + QUuid::createUuid().toString(QUuid::Id128);
        for (auto *process : {&loop, &feed}) { process->setStandardOutputFile(QProcess::nullDevice()); process->setStandardErrorFile(QProcess::nullDevice()); }
        loop.start(loopTool, {"-n", id, "-c", "1", "-m", "MONO", "-l", "10",
            "-i", "{ node.name = " + id + "-input node.description = \"Vivo Phone Microphone Input\" media.class = Audio/Sink node.virtual = true node.autoconnect = false }",
            "-o", "{ node.name = " + id + "-source node.description = \"Vivo Phone Microphone\" media.class = Audio/Source node.virtual = true node.autoconnect = false }"});
        if (!loop.waitForStarted(1000)) { message = "Cannot create the PipeWire microphone source."; failed = true; return false; }
        QThread::msleep(200);
        feed.start(feedTool, {"--playback", "--raw", "--target", id + "-input", "--rate", "48000", "--channels", "1", "--format", "s16", "--latency", "20ms", "-"});
        if (!feed.waitForStarted(1000)) { message = "Cannot connect the phone microphone audio stream."; failed = true; stop(); return false; }
        message = "Microphone: Vivo Phone Microphone";
        return true;
    }
};
PhoneAudio::PhoneAudio() : d(new Impl) {}
PhoneAudio::~PhoneAudio() { stop(); }
void PhoneAudio::stop() { d->stop(); }
QString PhoneAudio::status() const { return d->message; }
bool PhoneAudio::active() const { return d->feed.state() == QProcess::Running && d->loop.state() == QProcess::Running; }
bool PhoneAudio::push(const QByteArray &adts)
{
    if (d->failed) return false;
    const bool ok = d->decoder.push(adts, [this](const QByteArray &pcm) {
        if (!d->start()) return false;
        if (d->feed.bytesToWrite() > 9600) return true;
        if (d->feed.write(pcm) != pcm.size()) return false;
        d->feed.waitForBytesWritten(1);
        return true;
    });
    if (!ok) { if (!d->failed) d->message = "Phone microphone audio is unavailable."; d->failed = true; d->stop(); }
    return ok;
}

bool PhoneAudio::selfTest()
{
    const QByteArray fixture = QByteArray::fromHex(
        "fff14c40127ffcdc004c61766336332e312e313032000268a359a1288d35874a995f6f8a711e697b92243c2249fffeae4813"
        "74c37362b4f5b36d748babb83b668efc37b8b056ca9ad3252255cdb519ccb44a44a4489929128181819103031b366c181912"
        "2446cd9b368912296596cdc012a2e237044aa9c8d64d8d708cf6d15748ce5d056c8cd392aa1c069929c9a26b9296aefff14c"
        "4014dffc013494dac95d964bab25d414e9bfdfceadb5a97ff6f1fbf9d6b8d5ebffe2f5fbf9d6b8d5ebffec5fe3ceb5ad6ac6"
        "d4ecdbae79e60d65b2cf70a2f7fb28183740fa50afa503e9f4d6603e9f4fa5007d3e9f4d607d3e9f4d741801f4fa7d3599b0"
        "06b9eb458b549fcdb748843133e5133e5133e5f288983e5f2e31307cbe5c4983e5f2f94511003e5f2f9718a2005973d67024"
        "242424242424242424242424e7fff14c4010bffc013895b2c95d944b9d364bab25ce973faf5c69c3abd7fe93f5eb8be35aaf"
        "fe9ebdbad5f1abd78ff5f8eb57ad5ec31fb97bfa6a8de5030232b8995f2a15f2a04e3e5f21351d3194d979031335933593df"
        "420c1f46c060fa7d00799f4fa0039d83e9f4fa481f1c462ea23698f4c7747747747faae1f129f69f53ea7d4fa9fefff14c40"
        "019ffc011881b470");
    AudioDecoder decoder;
    int samples = 0; bool nonzero = false;
    if (!decoder.push(fixture, [&](const QByteArray &pcm) {
        samples += int(pcm.size()/2);
        for (char byte : pcm) nonzero = nonzero || byte != 0;
        return true;
    }) || samples < 2000 || !nonzero) return false;
    AudioDecoder invalid;
    return !invalid.push(QByteArray("invalid"), [](const QByteArray &) { return true; });
}
