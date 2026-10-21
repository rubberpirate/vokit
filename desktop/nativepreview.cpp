#include "nativepreview.h"
#include "phoneaudio.h"

#include <QCheckBox>
#include <QComboBox>
#include <QDir>
#include <QFocusEvent>
#include <QCoreApplication>
#include <QFrame>
#include <QElapsedTimer>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QImage>
#include <QInputMethodEvent>
#include <QJsonDocument>
#include <QJsonObject>
#include <QKeyEvent>
#include <QLabel>
#include <QLocalSocket>
#include <QLocalServer>
#include <QMouseEvent>
#include <QMutex>
#include <QMutexLocker>
#include <QPainter>
#include <QPushButton>
#include <QRegularExpression>
#include <QStandardPaths>
#include <QSaveFile>
#include <QTemporaryDir>
#include <QThread>
#include <QTimer>
#include <QVBoxLayout>
#include <QWheelEvent>
#include <QtEndian>
#include <atomic>
#include <cstring>
#include <deque>
#include <functional>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <libavutil/mem.h>
#include <libswscale/swscale.h>
}

#include <fcntl.h>
#include <linux/videodev2.h>
#include <sys/ioctl.h>
#include <unistd.h>

namespace {
constexpr quint32 recordLimit = 16 * 1024 * 1024;
constexpr int maxDimension = 4096;
constexpr qint64 maxPixels = 16 * 1024 * 1024;

QRectF pictureRect(const QSize &area, const QSize &image)
{
    if (area.isEmpty() || image.isEmpty()) return {};
    const QSize fitted = image.scaled(area, Qt::KeepAspectRatio);
    return QRectF((area.width() - fitted.width()) / 2.0,
                  (area.height() - fitted.height()) / 2.0, fitted.width(), fitted.height());
}

bool picturePoint(const QPointF &point, const QSize &area, const QSize &image, bool clamp, QPoint &result)
{
    const QRectF rect = pictureRect(area, image);
    if (rect.isEmpty() || (!clamp && !rect.contains(point))) return false;
    const qreal x = (point.x() - rect.left()) * image.width() / rect.width();
    const qreal y = (point.y() - rect.top()) * image.height() / rect.height();
    result = QPoint(qBound(0, int(x), image.width() - 1), qBound(0, int(y), image.height() - 1));
    return true;
}

class Surface : public QWidget {
public:
    explicit Surface(QWidget *parent) : QWidget(parent)
    {
        setMinimumSize(220, 200);
        setFocusPolicy(Qt::StrongFocus);
        setAttribute(Qt::WA_InputMethodEnabled);
        setAccessibleName("Live phone picture. Enable remote control to interact.");
    }
    std::function<void(QJsonObject)> send;
    QImage image;
    bool input = false;

    void setInput(bool enabled) { if (!enabled) release(); input = enabled; }
    void release()
    {
        if (!pressed) return;
        emitPointer("up", lastPoint);
        pressed = false;
    }
    void navigate(int code)
    {
        if (!input || !send) return;
        send({{"kind", "key"}, {"action", "down"}, {"keycode", code}});
        send({{"kind", "key"}, {"action", "up"}, {"keycode", code}});
    }
protected:
    void paintEvent(QPaintEvent *) override
    {
        QPainter painter(this);
        painter.fillRect(rect(), Qt::black);
        if (!image.isNull()) painter.drawImage(pictureRect(size(), image.size()), image);
        else { painter.setPen(Qt::white); painter.drawText(rect(), Qt::AlignCenter, "Live preview"); }
    }
    void mousePressEvent(QMouseEvent *event) override
    {
        QPoint p;
        if (!input || event->button() != Qt::LeftButton || !picturePoint(event->position(), size(), image.size(), false, p)) return;
        setFocus(Qt::MouseFocusReason);
        pressed = true;
        lastPoint = p;
        emitPointer("down", p);
    }
    void mouseMoveEvent(QMouseEvent *event) override
    {
        QPoint p;
        if (input && pressed && picturePoint(event->position(), size(), image.size(), true, p)) {
            lastPoint = p;
            emitPointer("move", p);
        }
    }
    void mouseReleaseEvent(QMouseEvent *event) override
    {
        if (event->button() != Qt::LeftButton || !pressed) return;
        QPoint p;
        if (picturePoint(event->position(), size(), image.size(), true, p)) lastPoint = p;
        release();
    }
    void focusOutEvent(QFocusEvent *event) override { release(); QWidget::focusOutEvent(event); }
    void wheelEvent(QWheelEvent *event) override
    {
        QPoint p;
        if (!input || !send || !picturePoint(event->position(), size(), image.size(), false, p)) return;
        wheel += event->angleDelta();
        const int horizontal = wheel.x() / 120;
        const int vertical = wheel.y() / 120;
        wheel -= QPoint(horizontal * 120, vertical * 120);
        for (int i = 0; i < qMin(8, qMax(qAbs(horizontal), qAbs(vertical))); ++i)
            send({{"kind", "scroll"}, {"hscroll", i < qAbs(horizontal) ? (horizontal > 0 ? 1 : -1) : 0},
                  {"vscroll", i < qAbs(vertical) ? (vertical > 0 ? 1 : -1) : 0},
                  {"x", p.x()}, {"y", p.y()}, {"width", image.width()}, {"height", image.height()}});
        event->accept();
    }
    void keyPressEvent(QKeyEvent *event) override
    {
        if (!input) return;
        if (event->key() == Qt::Key_Escape) { if (!event->isAutoRepeat()) navigate(4); event->accept(); return; }
        if (event->modifiers() & (Qt::ControlModifier | Qt::AltModifier | Qt::MetaModifier)) return;
        QString text = event->text();
        for (QChar c : text) if (c.category() == QChar::Other_Control && c != '\n' && c != '\r') return;
        if (text == "\r") text = "\n";
        sendText(text);
        event->accept();
    }
    void inputMethodEvent(QInputMethodEvent *event) override
    {
        if (input && !event->commitString().isEmpty()) sendText(event->commitString());
        event->accept();
    }
    QVariant inputMethodQuery(Qt::InputMethodQuery query) const override
    {
        if (query == Qt::ImEnabled) return input;
        if (query == Qt::ImCursorRectangle) return QRect(0, height() / 2, 1, 1);
        return QWidget::inputMethodQuery(query);
    }
private:
    bool pressed = false;
    QPoint lastPoint, wheel;
    void emitPointer(const QString &action, QPoint p)
    {
        if (send) send({{"kind", "pointer"}, {"action", action}, {"buttons", 1}, {"x", p.x()}, {"y", p.y()},
                        {"width", image.width()}, {"height", image.height()}});
    }
    void sendText(const QString &text)
    {
        if (!text.isEmpty() && text.toUtf8().size() <= 4096 && !text.contains(QChar(0)) && send)
            send({{"kind", "text"}, {"text", text}});
    }
};

class WebcamSink {
public:
    ~WebcamSink() { if (fd >= 0) ::close(fd); sws_freeContext(scale); }
    bool open(const QString &path)
    {
        if (!QRegularExpression("^/dev/video[0-9]{1,3}$").match(path).hasMatch()) return false;
        fd = ::open(path.toLocal8Bit().constData(), O_WRONLY | O_NONBLOCK | O_CLOEXEC | O_NOFOLLOW);
        if (fd < 0) return false;
        v4l2_capability cap{};
        if (ioctl(fd, VIDIOC_QUERYCAP, &cap) < 0 || QByteArray(reinterpret_cast<char *>(cap.driver), strnlen(reinterpret_cast<char *>(cap.driver), sizeof cap.driver)) != "v4l2 loopback") {
            // Kernel versions use either spelling for the same loopback driver.
            if (QByteArray(reinterpret_cast<char *>(cap.driver), strnlen(reinterpret_cast<char *>(cap.driver), sizeof cap.driver)) != "v4l2loopback") return false;
        }
        const quint32 caps = cap.capabilities & V4L2_CAP_DEVICE_CAPS ? cap.device_caps : cap.capabilities;
        if (!(caps & V4L2_CAP_VIDEO_OUTPUT) || !(caps & V4L2_CAP_READWRITE)) return false;
        v4l2_format format{};
        format.type = V4L2_BUF_TYPE_VIDEO_OUTPUT;
        format.fmt.pix.width = 1280;
        format.fmt.pix.height = 720;
        format.fmt.pix.pixelformat = V4L2_PIX_FMT_YUYV;
        format.fmt.pix.field = V4L2_FIELD_NONE;
        if (ioctl(fd, VIDIOC_S_FMT, &format) < 0 || format.fmt.pix.pixelformat != V4L2_PIX_FMT_YUYV ||
            !format.fmt.pix.width || !format.fmt.pix.height || format.fmt.pix.width > 1920 || format.fmt.pix.height > 1080 ||
            format.fmt.pix.bytesperline < format.fmt.pix.width * 2 || format.fmt.pix.bytesperline > 8192 || format.fmt.pix.sizeimage > 16 * 1024 * 1024) return false;
        width = int(format.fmt.pix.width);
        height = int(format.fmt.pix.height);
        stride = int(format.fmt.pix.bytesperline);
        writeBytes = qMax(int(format.fmt.pix.sizeimage), stride * height);
        bytes.resize(writeBytes + AV_INPUT_BUFFER_PADDING_SIZE);
        bytes.fill(0);
        return true;
    }
    void frame(const AVFrame *frame)
    {
        if (fd < 0 || !width) return;
        scale = sws_getCachedContext(scale, frame->width, frame->height, AVPixelFormat(frame->format), width, height,
                                     AV_PIX_FMT_YUYV422, SWS_FAST_BILINEAR, nullptr, nullptr, nullptr);
        if (!scale) return;
        uint8_t *planes[4]{reinterpret_cast<uint8_t *>(bytes.data()), nullptr, nullptr, nullptr};
        int strides[4]{stride, 0, 0, 0};
        if (sws_scale(scale, frame->data, frame->linesize, 0, frame->height, planes, strides) <= 0) return;
        // A nonblocking loopback sink can drop a frame when its consumer is slow.
        (void)::write(fd, bytes.constData(), size_t(writeBytes));
    }
private:
    int fd = -1, width = 0, height = 0, stride = 0, writeBytes = 0;
    QByteArray bytes;
    SwsContext *scale = nullptr;
};

QImage videoImage(int width, int height)
{
    if (width <= 0 || height <= 0 || width > maxDimension || height > maxDimension || qint64(width) * height > maxPixels) return {};
    // swscale SIMD needs aligned rows and writable padding after the last row.
    const int stride = (width * 3 + 63) & ~63;
    auto *pixels = static_cast<uchar *>(av_mallocz(size_t(stride) * height + AV_INPUT_BUFFER_PADDING_SIZE));
    if (!pixels) return {};
    QImage image(pixels, width, height, stride, QImage::Format_RGB888,
                 [](void *buffer) { av_free(buffer); }, pixels);
    if (image.isNull()) av_free(pixels);
    return image;
}

class Decoder {
public:
    ~Decoder() { sws_freeContext(scale); av_frame_free(&frame); av_packet_free(&packet); avcodec_free_context(&context); }
    bool open()
    {
        const AVCodec *codec = avcodec_find_decoder(AV_CODEC_ID_H264);
        if (!codec) return false;
        context = avcodec_alloc_context3(codec);
        frame = av_frame_alloc();
        packet = av_packet_alloc();
        if (!context || !frame || !packet) return false;
        context->flags |= AV_CODEC_FLAG_LOW_DELAY;
        context->thread_count = 2;
        context->thread_type = FF_THREAD_SLICE;
        context->max_pixels = maxPixels;
        return avcodec_open2(context, codec, nullptr) >= 0;
    }
    bool push(QByteArray bytes, const std::function<void(QImage)> &publish, WebcamSink *webcam, bool preview = true)
    {
        const int count = int(bytes.size());
        if (count <= 0 || quint32(count) > recordLimit) return false;
        bytes.append(QByteArray(AV_INPUT_BUFFER_PADDING_SIZE, 0));
        // split_frame:false preserves one encoded access unit per FRAME record.
        // Parsing this as a byte stream holds the last picture until another AU
        // arrives, which adds seconds of latency when the phone picture is still.
        packet->data = reinterpret_cast<uint8_t *>(bytes.data());
        packet->size = count;
        auto drain = [&]() {
            for (;;) {
                const int received = avcodec_receive_frame(context, frame);
                if (received == AVERROR(EAGAIN) || received == AVERROR_EOF || received == AVERROR_INVALIDDATA) return true;
                if (received < 0) return false;
                if (frame->width <= 0 || frame->height <= 0 || frame->width > maxDimension || frame->height > maxDimension ||
                    qint64(frame->width) * frame->height > maxPixels) return false;
                if (webcam) webcam->frame(frame);
                if (!preview) { av_frame_unref(frame); continue; }
                QImage image = videoImage(frame->width, frame->height);
                if (image.isNull()) return false;
                scale = sws_getCachedContext(scale, frame->width, frame->height, AVPixelFormat(frame->format), frame->width, frame->height,
                                             AV_PIX_FMT_RGB24, SWS_FAST_BILINEAR, nullptr, nullptr, nullptr);
                if (!scale) return false;
                uint8_t *out[4]{image.bits(), nullptr, nullptr, nullptr};
                int strides[4]{int(image.bytesPerLine()), 0, 0, 0};
                if (sws_scale(scale, frame->data, frame->linesize, 0, frame->height, out, strides) <= 0) return false;
                publish(std::move(image));
                av_frame_unref(frame);
            }
        };
        int sent = avcodec_send_packet(context, packet);
        if (sent == AVERROR(EAGAIN)) {
            if (!drain()) return false;
            sent = avcodec_send_packet(context, packet);
        }
        av_packet_unref(packet);
        // Standalone parameter-set records initialize the decoder without a
        // picture; FFmpeg can report INVALIDDATA after retaining those sets.
        if (sent == AVERROR_INVALIDDATA) return true;
        return sent >= 0 && drain();
    }
private:
    AVCodecContext *context = nullptr;
    AVFrame *frame = nullptr;
    AVPacket *packet = nullptr;
    SwsContext *scale = nullptr;
};

class MediaWorker : public QThread {
public:
    QJsonObject init;
    QString screenQuality = "720p";
    QString webcamPath;
    void setPreviewEnabled(bool enabled) { previewEnabled.store(enabled); if (!enabled) { QMutexLocker lock(&mutex); latest = {}; } }
    void setWebcamPath(QString path)
    {
        QMutexLocker lock(&mutex);
        requestedWebcamPath = std::move(path);
        webcamUpdatePending = true;
    }
    QString socketPath = QStandardPaths::writableLocation(QStandardPaths::RuntimeLocation) + "/vivolinux/media.sock";
    void finish() { stopping.store(true); }
    void input(QJsonObject event)
    {
        QByteArray bytes = QJsonDocument(event).toJson(QJsonDocument::Compact);
        if (bytes.size() > 8192) return;
        bytes.append('\n');
        const bool move = event.value("kind") == "pointer" && event.value("action") == "move";
        QMutexLocker lock(&mutex);
        if (move && !outgoing.empty() && outgoing.back().second) { outgoing.back().first = std::move(bytes); return; }
        if (outgoing.size() >= 64) { stopping.store(true); return; }
        outgoing.emplace_back(std::move(bytes), move);
    }
    bool takeImage(QImage &out)
    {
        QMutexLocker lock(&mutex);
        if (latest.isNull()) return false;
        out = std::move(latest);
        ++rendered;
        presentedAgeMs = clock.elapsed() - publishedMs;
        return true;
    }
    QString state() { QMutexLocker lock(&mutex); return status; }
    QJsonObject metrics()
    {
        QMutexLocker lock(&mutex);
        return {{"process_id", double(QCoreApplication::applicationPid())}, {"source", init.value("source")},
                {"elapsed_ms", clock.isValid() ? clock.elapsed() : 0}, {"video_records", double(records)},
                {"decoded_frames", double(decoded)}, {"gui_renders", double(rendered)}, {"video_bytes", double(byteCount)},
                {"queued_bytes", double(queuedBytes)}, {"peak_queued_bytes", double(peakQueuedBytes)},
                {"last_decode_us", double(lastDecodeUs)}, {"max_decode_us", double(maxDecodeUs)},
                {"total_decode_us", double(totalDecodeUs)}, {"last_gui_handoff_ms", presentedAgeMs},
                {"width", geometry.width()}, {"height", geometry.height()}, {"webcam_active", webcamActive}, {"preview_enabled", previewEnabled.load()}, {"microphone_active", microphoneActive}};
    }
protected:
    void run() override
    {
        clock.start();
        saveMetrics(true);
        Decoder decoder;
        PhoneAudio audio;
        const bool microphoneRequested = init.value("microphone").toBool();
        if (!decoder.open()) { setState("H.264 decoder unavailable."); return; }
        std::unique_ptr<WebcamSink> webcam;
        auto configureWebcam = [&](const QString &path) {
            webcam.reset();
            bool failed = false;
            if (!path.isEmpty()) {
                auto candidate = std::make_unique<WebcamSink>();
                if (candidate->open(path)) webcam = std::move(candidate);
                else failed = true;
            }
            QMutexLocker lock(&mutex);
            webcamActive = bool(webcam);
            webcamFailure = failed;
        };
        configureWebcam(webcamPath);
        QLocalSocket socket;
        socket.setReadBufferSize(recordLimit + 5);
        socket.connectToServer(socketPath);
        for (int i = 0; i < 30 && socket.state() == QLocalSocket::ConnectingState && !stopping.load(); ++i) socket.waitForConnected(100);
        if (stopping.load()) return;
        if (socket.state() != QLocalSocket::ConnectedState) { setState("Phone media service unavailable. Reconnect with the current native build."); return; }
        socket.write(QJsonDocument(init).toJson(QJsonDocument::Compact) + '\n');
        if (!socket.waitForBytesWritten(500)) { setState("Cannot start phone media."); return; }
        setState("Waiting for phone permission and video…");
        QByteArray buffer;
        bool qualitySent = false;
        int idle = 0;
        while (!stopping.load()) {
            QString updatedWebcam;
            bool updateWebcam = false;
            {
                QMutexLocker lock(&mutex);
                if (webcamUpdatePending) {
                    updatedWebcam = requestedWebcamPath;
                    webcamUpdatePending = false;
                    updateWebcam = true;
                }
            }
            if (updateWebcam) configureWebcam(updatedWebcam);
            flush(socket);
            if (socket.state() != QLocalSocket::ConnectedState) break;
            if (!socket.bytesAvailable()) {
                socket.waitForReadyRead(16);
                if (++idle > 3750) { setState("Phone video timed out."); return; }
                continue;
            }
            idle = 0;
            buffer.append(socket.read(recordLimit + 5 - buffer.size()));
            while (buffer.size() >= 5) {
                const quint8 kind = quint8(buffer[0]);
                const quint32 size = qFromBigEndian<quint32>(reinterpret_cast<const uchar *>(buffer.constData() + 1));
                if (size > recordLimit || (kind < 1 || kind > 4) || (kind == 4 && size > 512 * 1024) || ((kind == 1 || kind == 3) && size > 65536)) { setState("Invalid phone media response."); return; }
                if (buffer.size() < 5 + qsizetype(size)) break;
                QByteArray body = buffer.mid(5, size);
                buffer.remove(0, 5 + size);
                if (kind == 2) {
                    int kernelBytes = 0;
                    (void)::ioctl(int(socket.socketDescriptor()), FIONREAD, &kernelBytes);
                    {
                        QMutexLocker lock(&mutex);
                        ++records;
                        byteCount += size;
                        queuedBytes = quint64(buffer.size() + socket.bytesAvailable() + qMax(0, kernelBytes));
                        peakQueuedBytes = qMax(peakQueuedBytes, queuedBytes);
                    }
                    QElapsedTimer decodeTimer;
                    decodeTimer.start();
                    if (!decoder.push(std::move(body), [this](QImage image) {
                        QMutexLocker lock(&mutex);
                        geometry = image.size();
                        latest = std::move(image);
                        ++decoded;
                        publishedMs = clock.elapsed();
                    }, webcam.get(), previewEnabled.load())) {
                        setState("Cannot decode phone video."); return;
                    }
                    {
                        QMutexLocker lock(&mutex);
                        lastDecodeUs = quint64(decodeTimer.nsecsElapsed() / 1000);
                        maxDecodeUs = qMax(maxDecodeUs, lastDecodeUs);
                        totalDecodeUs += lastDecodeUs;
                        microphoneActive = audio.active();
                        if (!statusError) {
                            status = webcamFailure ? "Camera live · webcam device unavailable" : webcamActive ? "Camera live · webcam output enabled" : "Live";
                            if (!previewEnabled.load()) status += " · preview hidden";
                            if (microphoneRequested) status += "\n" + (microphoneUnavailable ? QString("Phone microphone unavailable on this connection.") :
                                !microphoneActive && clock.elapsed() > 10000 ? QString("No microphone audio received from the phone. Webcam sharing continues.") : audio.status());
                        }
                    }
                    saveMetrics();
                } else if (kind == 4) {
                    if (microphoneRequested && !microphoneUnavailable) (void)audio.push(body);
                } else if (kind == 3) {
                    const auto object = QJsonDocument::fromJson(body).object();
                    const QString error = object.value("error").toString();
                    const QString stage = object.value("stage").toString();
                    if (!error.isEmpty()) setState(error.left(240), true);
                    else if (stage.contains("denied")) setState("Phone permission was denied. Approve it on the phone and retry.", true);
                    else if (stage.contains("error") || stage.contains("failed")) setState("Phone media stopped. Check the phone and retry.", true);
                } else {
                    const auto info = QJsonDocument::fromJson(body).object();
                    if (info.contains("microphone_available") && !info.value("microphone_available").toBool()) { microphoneUnavailable = true; audio.stop(); }
                    const QString stage = info.value("stage").toString();
                    if (!qualitySent && init.value("source") == "screen" && info.value("video_width").toInt() > 0 && info.value("video_height").toInt() > 0) {
                        input({{"kind", "quality"}, {"quality", screenQuality}});
                        qualitySent = true;
                    }
                    if (stage == "permission_wait") setState("Approve screen or camera access on the phone.");
                    else if (stage == "starting") setState("Starting phone video…");
                }
            }
        }
        flush(socket);
        socket.disconnectFromServer();
        setState(stopping.load() ? "Preview stopped." : "Phone media disconnected.");
        saveMetrics(true);
    }
private:
    std::atomic_bool stopping{false};
    std::atomic_bool previewEnabled{true};
    QMutex mutex;
    std::deque<std::pair<QByteArray, bool>> outgoing;
    QImage latest;
    QString status = "Starting…";
    bool statusError = false;
    bool webcamActive = false, webcamFailure = false, webcamUpdatePending = false;
    bool microphoneActive = false, microphoneUnavailable = false;
    QString requestedWebcamPath;
    QElapsedTimer clock;
    quint64 records = 0, decoded = 0, rendered = 0, byteCount = 0;
    quint64 queuedBytes = 0, peakQueuedBytes = 0, lastDecodeUs = 0, maxDecodeUs = 0, totalDecodeUs = 0;
    qint64 publishedMs = 0, presentedAgeMs = 0, savedMetricsMs = 0;
    QSize geometry;
    void saveMetrics(bool force = false)
    {
        if (!qEnvironmentVariableIsSet("VIVOLINUX_PREVIEW_DIAGNOSTICS") || (!force && clock.elapsed() - savedMetricsMs < 1000)) return;
        savedMetricsMs = clock.elapsed();
        QSaveFile file(QFileInfo(socketPath).dir().filePath("preview-metrics.json"));
        if (!file.open(QIODevice::WriteOnly)) return;
        file.setPermissions(QFileDevice::ReadOwner | QFileDevice::WriteOwner);
        file.write(QJsonDocument(metrics()).toJson(QJsonDocument::Compact));
        (void)file.commit();
    }
    void setState(QString value, bool error = false)
    {
        QMutexLocker lock(&mutex);
        if (statusError && !error) return;
        statusError = error;
        status = std::move(value);
    }
    void flush(QLocalSocket &socket)
    {
        std::deque<std::pair<QByteArray, bool>> queue;
        { QMutexLocker lock(&mutex); queue.swap(outgoing); }
        for (const auto &record : queue) {
            if (socket.write(record.first) != record.first.size()) { stopping.store(true); break; }
        }
        if (socket.bytesToWrite()) socket.waitForBytesWritten(100);
    }
};
} // namespace

struct NativePreview::Impl {
    NativePreview *owner;
    Surface *surface;
    QComboBox *source, *device, *quality, *cameraQuality;
    QLabel *qualityLabel, *cameraQualityLabel;
    QCheckBox *control, *webcam;
    QCheckBox *preview, *microphone;
    QFrame *phoneFrame;
    QWidget *navigation;
    NativePreview::Mode mode;
    QPushButton *start, *stop;
    QLabel *status;
    QTimer timer;
    MediaWorker *worker = nullptr;
    bool connected = false;
    explicit Impl(NativePreview *widget, NativePreview::Mode selectedMode) : owner(widget), mode(selectedMode)
    {
        auto *layout = new QVBoxLayout(widget);
        auto *row = new QHBoxLayout;
        source = new QComboBox(widget);
        source->addItems({"Phone screen", "Rear camera", "Front camera"});
        source->setCurrentIndex(mode == NativePreview::Mode::Screen ? 0 : 1);
        if (mode == NativePreview::Mode::Camera) source->removeItem(0);
        source->setAccessibleName("Live source");
        start = new QPushButton("Start preview", widget);
        stop = new QPushButton("Stop", widget);
        row->addWidget(source, 1); row->addWidget(start); row->addWidget(stop);
        layout->addLayout(row);
        auto *qualityRow = new QHBoxLayout;
        qualityLabel = new QLabel("Screen quality", widget);
        qualityRow->addWidget(qualityLabel);
        quality = new QComboBox(widget);
        quality->addItem("480p", "480p");
        quality->addItem("720p", "720p");
        quality->addItem("1080p", "1080p");
        quality->addItem("Phone automatic", "auto");
        quality->setCurrentIndex(1);
        quality->setAccessibleName("Screen stream quality");
        qualityRow->addWidget(quality, 1);
        cameraQualityLabel = new QLabel("Camera quality", widget);
        qualityRow->addWidget(cameraQualityLabel);
        cameraQuality = new QComboBox(widget);
        cameraQuality->addItem("Phone default", "");
        cameraQuality->addItem("720p · Low latency", "720p");
        cameraQuality->addItem("1080p", "1080p");
        cameraQuality->setCurrentIndex(1);
        cameraQuality->setAccessibleName("Camera stream quality. Stop preview to change.");
        qualityRow->addWidget(cameraQuality, 1);
        layout->addLayout(qualityRow);
        phoneFrame = new QFrame(widget);
        phoneFrame->setObjectName("phoneFrame");
        auto *pictureLayout = new QVBoxLayout(phoneFrame);
        pictureLayout->setContentsMargins(mode == NativePreview::Mode::Screen ? 7 : 0, mode == NativePreview::Mode::Screen ? 12 : 0, mode == NativePreview::Mode::Screen ? 7 : 0, mode == NativePreview::Mode::Screen ? 8 : 0);
        if (mode == NativePreview::Mode::Screen) {
            phoneFrame->setStyleSheet("QFrame#phoneFrame {background:#14171c;border:1px solid #343a46;border-radius:28px;}");
            auto *notch = new QLabel("", phoneFrame);
            notch->setFixedSize(70, 7); notch->setStyleSheet("background:#363d48;border-radius:3px;");
            pictureLayout->addWidget(notch, 0, Qt::AlignHCenter);
        }
        surface = new Surface(phoneFrame);
        pictureLayout->addWidget(surface, 1);
        layout->addWidget(phoneFrame, 1);
        preview = new QCheckBox("Show camera preview", widget);
        preview->setChecked(true);
        preview->setVisible(mode == NativePreview::Mode::Camera);
        layout->addWidget(preview);
        auto *inputRow = new QHBoxLayout;
        navigation = new QWidget(widget);
        navigation->setLayout(inputRow);
        control = new QCheckBox("Touch & typing", widget);
        inputRow->addWidget(control);
        for (const auto &pair : {std::make_pair("Back", 4), std::make_pair("Home", 3), std::make_pair("Recents", 187)}) {
            auto *button = new QPushButton(pair.first, widget);
            QObject::connect(button, &QPushButton::clicked, widget, [this, code=pair.second] { surface->navigate(code); });
            inputRow->addWidget(button);
        }
        layout->addWidget(navigation);
        auto *cameraRow = new QHBoxLayout;
        webcam = new QCheckBox("Use camera as webcam", widget);
        device = new QComboBox(widget);
        device->setEditable(true);
        device->addItem("/dev/video10");
        device->setAccessibleName("Virtual webcam output device");
        cameraRow->addWidget(webcam); cameraRow->addWidget(device, 1);
        layout->addLayout(cameraRow);
        microphone = new QCheckBox("Share phone microphone", widget);
        microphone->setToolTip("Creates the Vivo Phone Microphone audio source when the phone provides microphone audio. Phone support must be verified.");
        layout->addWidget(microphone);
        status = new QLabel("Connect the phone to start preview.", widget);
        status->setWordWrap(true); status->setTextFormat(Qt::PlainText);
        layout->addWidget(status);
        surface->send = [this](QJsonObject event) { if (worker && worker->isRunning()) worker->input(std::move(event)); };
        QObject::connect(control, &QCheckBox::toggled, widget, [this](bool enabled) { surface->setInput(enabled && mode == NativePreview::Mode::Screen && worker); });
        QObject::connect(preview, &QCheckBox::toggled, widget, [this](bool shown) { owner->setPreviewVisible(shown); });
        QObject::connect(start, &QPushButton::clicked, widget, [this] { begin(); });
        QObject::connect(quality, &QComboBox::currentIndexChanged, widget, [this] {
            if (worker && worker->isRunning() && mode == NativePreview::Mode::Screen)
                worker->input({{"kind", "quality"}, {"quality", quality->currentData().toString()}});
        });
        QObject::connect(stop, &QPushButton::clicked, widget, [this] { owner->stop(); });
        QObject::connect(source, &QComboBox::currentIndexChanged, widget, [this] {
            if (mode == NativePreview::Mode::Camera && worker && worker->isRunning()) {
                worker->input({{"kind", "camera"}, {"facing", source->currentIndex() == 1 ? "front" : "rear"}});
                status->setText("Switching phone camera…");
                return;
            }
            const bool active = worker;
            owner->stop();
            update();
            if (active && connected) begin();
        });
        QObject::connect(webcam, &QCheckBox::toggled, widget, [this](bool enabled) {
            if (worker && worker->isRunning() && mode == NativePreview::Mode::Camera)
                worker->setWebcamPath(enabled ? device->currentText() : QString());
            update();
        });
        timer.setInterval(16);
        QObject::connect(&timer, &QTimer::timeout, widget, [this] {
            if (!worker) return;
            QImage image;
            if (worker->takeImage(image)) { if (surface->image.size() != image.size()) surface->release(); surface->image = std::move(image); surface->update(); }
            status->setText(worker->state());
            if (!worker->isRunning()) { surface->setInput(false); worker->wait(); delete worker; worker=nullptr; update(); }
        });
        timer.start();
        update();
    }
    void update()
    {
        start->setEnabled(connected && !worker);
        stop->setEnabled(worker);
        const bool camera = mode == NativePreview::Mode::Camera;
        source->setVisible(camera);
        navigation->setVisible(!camera);
        webcam->setVisible(camera);
        device->setVisible(camera);
        microphone->setVisible(camera);
        microphone->setEnabled(camera && !worker);
        qualityLabel->setVisible(!camera);
        quality->setVisible(!camera);
        cameraQualityLabel->setVisible(camera);
        cameraQuality->setVisible(camera);
        cameraQuality->setEnabled(camera && !worker);
        quality->setEnabled(!camera);
        control->setEnabled(!camera && worker);
        webcam->setEnabled(camera);
        device->setEnabled(camera && webcam->isChecked() && !worker);
    }
    void begin()
    {
        if (!connected || worker) return;
        worker = new MediaWorker;
        worker->screenQuality = quality->currentData().toString();
        const bool camera = mode == NativePreview::Mode::Camera;
        worker->init = !camera ? QJsonObject{{"source", "screen"}} : QJsonObject{{"source", "camera"}, {"facing", source->currentIndex() == 1 ? "front" : "rear"}};
        if (camera && !cameraQuality->currentData().toString().isEmpty()) worker->init.insert("quality", cameraQuality->currentData().toString());
        if (camera && microphone->isChecked()) worker->init.insert("microphone", true);
        if (camera && webcam->isChecked()) worker->webcamPath = device->currentText();
        worker->setPreviewEnabled(!camera || preview->isChecked());
        surface->image = {};
        surface->update();
        surface->setInput(!camera && control->isChecked());
        worker->start();
        update();
    }
};

NativePreview::NativePreview(QWidget *parent, bool test, Mode mode) : QWidget(parent), d(new Impl(this, mode)) { Q_UNUSED(test); }
NativePreview::~NativePreview() { stop(); delete d; }
void NativePreview::start() { d->begin(); }
bool NativePreview::isRunning() const { return d->worker && d->worker->isRunning(); }
void NativePreview::setPreviewVisible(bool visible)
{
    d->phoneFrame->setVisible(visible);
    if (!visible) { d->surface->setInput(false); d->surface->image = {}; }
    if (d->worker) d->worker->setPreviewEnabled(visible);
    if (d->preview->isChecked() != visible) d->preview->setChecked(visible);
}
void NativePreview::setConnected(bool connected)
{
    if (!connected) stop();
    d->connected = connected;
    d->update();
}
void NativePreview::stop()
{
    d->surface->setInput(false);
    if (!d->worker) return;
    d->worker->finish();
    if (d->worker->wait(3000)) delete d->worker;
    else QObject::connect(d->worker, &QThread::finished, d->worker, &QObject::deleteLater);
    d->worker = nullptr;
    d->status->setText("Preview stopped.");
    d->update();
}
bool NativePreview::selfTest()
{
    if (!PhoneAudio::selfTest()) return false;
    for (const int width : {64, 718, 720, 760, 1216}) {
        QImage padded = videoImage(width, 96);
        if (padded.isNull() || padded.width() != width || padded.bytesPerLine() < width * 3 || padded.bytesPerLine() % 64 != 0) return false;
        QImage shared = padded;
        padded = {};
        shared.fill(Qt::red);
        if (shared.pixelColor(width - 1, 95).red() != 255) return false;
    }
    QPoint point;
    const QSize area(800,600), portrait(720,1560);
    if (picturePoint({10,300},area,portrait,false,point)) return false;
    if (!picturePoint({400,300},area,portrait,false,point) || point != QPoint(360,780)) return false;
    if (!picturePoint({-10,700},area,portrait,true,point) || point != QPoint(0,1559)) return false;
    if (!picturePoint({799,599},{800,600},{800,600},false,point) || point != QPoint(799,599)) return false;
    Decoder decoder;
    if (!decoder.open()) return false;
    // Four synthetic red 64×96 frames: libx264 ultrafast/zerolatency, aud=1,
    // keyint=1, with SEI removed. This includes SPS/PPS and actual IDR slices.
    const QByteArray fixture = QByteArray::fromHex(
        "000000010910000000016742c00adc436c0440000003004000000523c489e00000000168ce0fc800"
        "00016588843a118a000218f1c00040f63800087949c9c9d75d75d75d75d75d75d75d75d75d75e000"
        "0000010910000000016742c00adc436c0440000003004000000523c489e00000000168ce0fc80000"
        "0165888203e846280009bec700011518e00028512727275d75d75d75d75d75d75d75d75d75d78000"
        "0000010910000000016742c00adc436c0440000003004000000523c489e00000000168ce0fc80000"
        "016588840fa118a00026fb1c000454638000a1449c9c9d75d75d75d75d75d75d75d75d75d75e0000"
        "00010910000000016742c00adc436c0440000003004000000523c489e00000000168ce0fc8000001"
        "65888203e846280009bec700011518e00028512727275d75d75d75d75d75d75d75d75d75d780");
    const QByteArray delimiter = QByteArray::fromHex("000000010910");
    QList<QByteArray> accessUnits;
    for (qsizetype offset = 0; offset < fixture.size();) {
        qsizetype end = fixture.indexOf(delimiter, offset + delimiter.size());
        if (end < 0) end = fixture.size();
        accessUnits.append(fixture.mid(offset, end - offset));
        offset = end;
    }
    if (accessUnits.size() != 4) return false;
    int decoded = 0;
    bool valid = true;
    for (const auto &unit : accessUnits) {
        const int before = decoded;
        if (!decoder.push(unit, [&](QImage image) {
            ++decoded;
            const QColor color = image.pixelColor(image.width() / 2, image.height() / 2);
            valid = valid && image.size() == QSize(64, 96) && image.format() == QImage::Format_RGB888 &&
                    color.red() > 200 && color.green() < 50 && color.blue() < 50;
        }, nullptr)) return false;
        // Every complete access unit is visible before the next record arrives.
        if (decoded != before + 1) return false;
    }
    if (!valid || decoded != 4) return false;
    // Synthetic 760-pixel crop reproduces the phone's non-aligned RGB stride.
    const QByteArray wideFixture = QByteArray::fromHex(
        "000000010910000000016742c00ddc0c0de5f0110000030001000003003c8f142b800000000168ce0fc80000016588843a11"
        "8a000218f1c00040f63800087949c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9"
        "c9c9c9c9c9c9c9c9c9c9d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d7"
        "5d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75"
        "d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d"
        "75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75d75e0");
    Decoder changingDecoder;
    if (!changingDecoder.open()) return false;
    for (int n = 0; n < 20; ++n) {
        for (const auto &unit : {accessUnits.first(), wideFixture}) {
            int images = 0;
            const int expectedWidth = unit == wideFixture ? 760 : 64;
            bool correct = true;
            if (!changingDecoder.push(unit, [&](QImage image) {
                ++images;
                const QColor color = image.pixelColor(image.width() - 1, 95);
                correct = correct && image.size() == QSize(expectedWidth, 96) && image.bytesPerLine() % 64 == 0 &&
                          color.red() > 200 && color.green() < 50 && color.blue() < 50;
            }, nullptr) || images != 1 || !correct) return false;
        }
    }
    int hiddenImages = 0;
    if (!changingDecoder.push(wideFixture, [&](QImage) { ++hiddenImages; }, nullptr, false) || hiddenImages != 0 ||
        !changingDecoder.push(wideFixture, [&](QImage) { ++hiddenImages; }, nullptr, true) || hiddenImages != 1) return false;
    Decoder configDecoder;
    if (!configDecoder.open()) return false;
    const qsizetype idr = accessUnits.first().indexOf(QByteArray::fromHex("00000165"));
    int configFrames = 0;
    auto configImage = [&](QImage image) { if (image.size() == QSize(64, 96)) ++configFrames; };
    if (idr <= 0 || !configDecoder.push(accessUnits.first().left(idr), configImage, nullptr) || configFrames != 0 ||
        !configDecoder.push(accessUnits.first().mid(idr), configImage, nullptr) || configFrames != 1) return false;

    // Exercise fragmented private records and sparse-frame delivery without
    // connecting to the real media socket or saving any phone pixels.
    QTemporaryDir temporary;
    if (!temporary.isValid()) return false;
    QLocalServer server;
    server.setSocketOptions(QLocalServer::UserAccessOption);
    const QString path = temporary.filePath("media.sock");
    if (!server.listen(path)) return false;
    MediaWorker worker;
    worker.socketPath = path;
    worker.init = {{"source", "screen"}};
    worker.start();
    struct StopWorker { MediaWorker &worker; ~StopWorker() { worker.finish(); worker.wait(3000); } } cleanup{worker};
    if (!server.waitForNewConnection(1000)) return false;
    std::unique_ptr<QLocalSocket> peer(server.nextPendingConnection());
    if (!peer || (!peer->bytesAvailable() && !peer->waitForReadyRead(1000)) ||
        QJsonDocument::fromJson(peer->readLine(8192)).object().value("source") != "screen") return false;
    auto sendRecord = [&](quint8 kind, QByteArray body) {
        QByteArray record(5, 0);
        record[0] = char(kind);
        qToBigEndian<quint32>(quint32(body.size()), reinterpret_cast<uchar *>(record.data() + 1));
        record += body;
        // Fragment the header and payload at boundaries unrelated to H.264.
        for (qsizetype offset = 0; offset < record.size(); offset += 17) {
            if (peer->write(record.mid(offset, 17)) < 0) return false;
            if (peer->bytesToWrite() && !peer->waitForBytesWritten(100)) return false;
        }
        return true;
    };
    auto waitFrame = [&](int frames) {
        QElapsedTimer deadline;
        deadline.start();
        while (deadline.elapsed() < 500 && worker.metrics().value("decoded_frames").toInt() < frames) QThread::msleep(1);
        QImage image;
        return worker.metrics().value("decoded_frames").toInt() == frames && worker.takeImage(image) && image.size() == QSize(64, 96);
    };
    QElapsedTimer sent;
    sent.start();
    if (!sendRecord(1, QJsonDocument(QJsonObject{{"video_width", 64}, {"video_height", 96}}).toJson(QJsonDocument::Compact))) return false;
    if (!peer->bytesAvailable() && !peer->waitForReadyRead(1000)) return false;
    const auto qualityRequest = QJsonDocument::fromJson(peer->readLine(8192)).object();
    if (qualityRequest.value("kind") != "quality" || qualityRequest.value("quality") != "720p") return false;
    if (!sendRecord(1, QJsonDocument(QJsonObject{{"stage", "running"}, {"synthetic_sent_ms", sent.elapsed()}}).toJson(QJsonDocument::Compact)) ||
        !sendRecord(2, accessUnits.first()) || !waitFrame(1) || sent.elapsed() >= 500) return false;
    QThread::msleep(100);
    if (worker.metrics().value("decoded_frames").toInt() != 1 || !sendRecord(2, accessUnits.at(1)) || !waitFrame(2)) return false;
    worker.setWebcamPath("/not/a/video-device");
    QThread::msleep(30);
    if (!sendRecord(2, accessUnits.at(2)) || !waitFrame(3) || worker.metrics().value("webcam_active").toBool() || !worker.state().contains("webcam device unavailable")) return false;
    worker.setWebcamPath({});
    QThread::msleep(30);
    if (!sendRecord(2, accessUnits.at(3)) || !waitFrame(4) || worker.metrics().value("webcam_active").toBool() || worker.state() != "Live") return false;
    if (!sendRecord(3, "{\"error\":\"Synthetic permission denied.\"}")) return false;
    QElapsedTimer errorDeadline;
    errorDeadline.start();
    while (errorDeadline.elapsed() < 500 && worker.state() != "Synthetic permission denied.") QThread::msleep(1);
    peer->disconnectFromServer();
    worker.finish();
    if (!worker.wait(1000) || worker.state() != "Synthetic permission denied.") return false;
    WebcamSink sink;
    if (sink.open("/tmp/not-a-camera") || sink.open("/dev/../tmp/not-a-camera")) return false;
    setConnected(true);
    if (!d->start->isEnabled()) return false;
    setConnected(false);
    return !d->start->isEnabled() && !d->stop->isEnabled();
}
