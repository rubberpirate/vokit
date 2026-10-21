#include <QClipboard>
#include <QCoreApplication>
#include <QDBusConnection>
#include <QDBusMessage>
#include <QDBusPendingCallWatcher>
#include <QGuiApplication>
#include <QJsonDocument>
#include <QJsonObject>
#include <QMimeData>
#include <QProcess>
#include <QStandardPaths>
#include <QSocketNotifier>
#include <QStringConverter>
#include <QVariantMap>

#include <cerrno>
#include <cstdlib>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/prctl.h>
#include <functional>
#include <memory>
#include <unistd.h>
#include <utility>
#include <vector>

namespace {
constexpr qsizetype maxLineBytes = 2 * 1024 * 1024;
constexpr qsizetype maxTextBytes = 64 * 1024;

void qtMessage(QtMsgType type, const QMessageLogContext &, const QString &)
{
    // Qt platform diagnostics may contain clipboard data; never forward them.
    std::fputs("desktop helper: Qt diagnostic\n", stderr);
    if (type == QtFatalMsg)
        std::abort();
}

bool validText(const QString &text)
{
    if (text.isEmpty() || text.size() > maxTextBytes)
        return false;
    QStringEncoder encoder(QStringEncoder::Utf8, QStringConverter::Flag::Stateless);
    const QByteArray bytes = encoder(text);
    return !encoder.hasError() && bytes.size() <= maxTextBytes;
}

bool writeEvent(const QJsonObject &event)
{
    QByteArray bytes = QJsonDocument(event).toJson(QJsonDocument::Compact);
    bytes.append('\n');
    qsizetype offset = 0;
    while (offset < bytes.size()) {
        const ssize_t written = ::write(STDOUT_FILENO, bytes.constData() + offset,
                                        size_t(bytes.size() - offset));
        if (written < 0 && errno == EINTR)
            continue;
        if (written <= 0)
            return false;
        offset += written;
    }
    return true;
}

int emitClipboardSelection()
{
    QByteArray data;
    if (qgetenv("CLIPBOARD_STATE") == "data") {
        char chunk[4096];
        while (data.size() <= maxTextBytes) {
            pollfd fd{STDIN_FILENO, POLLIN, 0};
            if (::poll(&fd, 1, 2000) <= 0)
                break;
            const ssize_t n = ::read(STDIN_FILENO, chunk, sizeof(chunk));
            if (n <= 0)
                break;
            data.append(chunk, n);
        }
    }
    QStringDecoder decoder(QStringDecoder::Utf8, QStringConverter::Flag::Stateless);
    const QString text = decoder(data);
    const bool valid = !decoder.hasError() && validText(text);
    return writeEvent({{"text", valid ? text : QString{}}}) ? 0 : 1;
}

class Bridge : public QObject {
public:
    explicit Bridge(bool clipboardEnabled = true,
                    std::function<bool(const QJsonObject &)> sink = writeEvent)
        : output(std::move(sink)),
          clipboard(clipboardEnabled ? QGuiApplication::clipboard() : nullptr),
          lastText(clipboard ? clipboardText() : QString{})
    {
        if (clipboard) {
            connect(clipboard, &QClipboard::dataChanged, this, [this] {
                const QString text = clipboardText();
                if (text == lastText)
                    return;
                lastText = text;
                if (!text.isEmpty())
                    emitEvent({{"event", "text"}, {"text", text}});
            });
            // Data-control observes copies while this app has no focused window.
            if (QGuiApplication::platformName() == "wayland"
                    && !QStandardPaths::findExecutable("wl-paste").isEmpty()) {
                observer = new QProcess(this);
                observer->setStandardErrorFile(QProcess::nullDevice());
                observer->setChildProcessModifier([] {
                    prctl(PR_SET_PDEATHSIG, SIGTERM);
                    if (getppid() == 1)
                        _exit(1);
                });
                connect(observer, &QProcess::readyReadStandardOutput, this, [this] {
                    observed.append(observer->readAllStandardOutput());
                    if (observed.size() > maxLineBytes) { observer->kill(); observed.clear(); return; }
                    while (true) {
                        const qsizetype newline = observed.indexOf('\n');
                        if (newline < 0) break;
                        const auto object = QJsonDocument::fromJson(observed.left(newline)).object();
                        observed.remove(0, newline + 1);
                        const QString text = object.value("text").toString();
                        // The initial selection is a baseline, never auto-shared.
                        if (!observerReady) { observerReady = true; lastText = text; continue; }
                        if (text == lastText) continue;
                        lastText = text;
                        if (validText(text)) emitEvent({{"event", "text"}, {"text", text}});
                    }
                });
                observer->start("wl-paste", {"--type", "text", "--watch",
                                           QCoreApplication::applicationFilePath(), "--clipboard-event"});
            }
        }
        emitEvent({{"event", "ready"}});
    }

    ~Bridge() override {
        if (observer && observer->state() != QProcess::NotRunning) {
            observer->terminate();
            if (!observer->waitForFinished(300)) { observer->kill(); observer->waitForFinished(300); }
        }
    }

    bool watchInput()
    {
        const int flags = fcntl(STDIN_FILENO, F_GETFL);
        if (flags < 0 || fcntl(STDIN_FILENO, F_SETFL, flags | O_NONBLOCK) < 0)
            return false;
        auto *notifier = new QSocketNotifier(STDIN_FILENO, QSocketNotifier::Read, this);
        connect(notifier, &QSocketNotifier::activated, this, [this, notifier] {
            char chunk[8192];
            // Yield to clipboard and DBus events even if the producer stays busy.
            for (int reads = 0; reads < 32; ++reads) {
                const ssize_t count = ::read(STDIN_FILENO, chunk, sizeof(chunk));
                if (count > 0) {
                    consume(QByteArray(chunk, count));
                } else if (count == 0) {
                    notifier->setEnabled(false);
                    QCoreApplication::quit();
                    return;
                } else if (errno != EINTR) {
                    if (errno != EAGAIN && errno != EWOULDBLOCK)
                        QCoreApplication::exit(1);
                    return;
                }
            }
        });
        return true;
    }

    void consume(const QByteArray &chunk)
    {
        qsizetype offset = 0;
        while (offset < chunk.size()) {
            const qsizetype newline = chunk.indexOf('\n', offset);
            const qsizetype end = newline < 0 ? chunk.size() : newline;
            if (!discarding) {
                if (end - offset > maxLineBytes - pending.size()) {
                    pending.clear();
                    discarding = true;
                    emitEvent({{"event", "error"}, {"error", "line_too_large"}});
                } else {
                    pending.append(chunk.constData() + offset, end - offset);
                }
            }
            if (newline < 0)
                return;
            if (!discarding)
                request(pending);
            pending.clear();
            discarding = false;
            offset = newline + 1;
        }
    }

private:
    QString clipboardText() const
    {
        const QMimeData *mime = clipboard->mimeData(QClipboard::Clipboard);
        if (!mime || !mime->hasText() || mime->hasImage())
            return {};
        const QString text = mime->text();
        return validText(text) ? text : QString{};
    }

    void emitEvent(const QJsonObject &event)
    {
        if (!output(event))
            QCoreApplication::exit(1);
    }

    void acknowledge(const char *event, bool ok, const QJsonValue &id)
    {
        QJsonObject reply{{"event", event}, {"ok", ok}};
        if (!id.isUndefined())
            reply.insert("id", id);
        emitEvent(reply);
    }

    void request(const QByteArray &line)
    {
        QStringDecoder decoder(QStringDecoder::Utf8, QStringConverter::Flag::Stateless);
        const QString decoded = decoder(line);
        Q_UNUSED(decoded);
        QJsonParseError error;
        const QJsonDocument doc = QJsonDocument::fromJson(line, &error);
        if (decoder.hasError() || error.error != QJsonParseError::NoError || !doc.isObject()) {
            emitEvent({{"event", "error"}, {"error", "invalid_request"}});
            return;
        }
        const QJsonObject object = doc.object();
        const QString op = object.value("op").toString();
        const QJsonValue id = object.value("id");
        if (op == "set_text") {
            const QJsonValue value = object.value("text");
            const QString text = value.toString();
            const bool ok = clipboard && value.isString() && validText(text);
            if (ok)
                clipboard->setText(text, QClipboard::Clipboard);
            acknowledge("set", ok, id);
        } else if (op == "notify") {
            notify(object.value("state").toString(), id);
        } else {
            emitEvent({{"event", "error"}, {"error", "invalid_request"}});
        }
    }

    void notify(const QString &state, const QJsonValue &id)
    {
        QString body;
        if (state == "connected")
            body = QStringLiteral("Phone connected.");
        else if (state == "disconnected")
            body = QStringLiteral("Phone disconnected.");
        else if (state == "connecting")
            body = QStringLiteral("Connecting to phone.");
        else {
            acknowledge("notification", false, id);
            return;
        }
        QDBusMessage message = QDBusMessage::createMethodCall(
            "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
            "org.freedesktop.Notifications", "Notify");
        message << QStringLiteral("Vivo Desktop") << quint32(0)
                << QStringLiteral("phone") << QStringLiteral("Vivo Desktop") << body
                << QStringList{} << QVariantMap{{"urgency", QVariant::fromValue(quint8(1))}}
                << qint32(4000);
        auto *watcher = new QDBusPendingCallWatcher(
            QDBusConnection::sessionBus().asyncCall(message, 3000), this);
        const auto finished = [this, id](QDBusPendingCallWatcher *call) {
            acknowledge("notification", !call->isError(), id);
            call->deleteLater();
        };
        if (watcher->isFinished())
            finished(watcher);
        else
            connect(watcher, &QDBusPendingCallWatcher::finished, this, finished);
    }

    std::function<bool(const QJsonObject &)> output;
    QClipboard *clipboard;
	QProcess *observer = nullptr;
	QByteArray observed;
	bool observerReady = false;
    QString lastText;
    QByteArray pending;
    bool discarding = false;
};

int selfTest()
{
    QClipboard *clipboard = QGuiApplication::clipboard();
    clipboard->setText(QStringLiteral("existing private clipboard"));
    std::vector<QJsonObject> events;
    Bridge bridge(true, [&events](const QJsonObject &event) {
        events.push_back(event);
        return true;
    });
    const auto check = [](bool condition) {
        if (!condition) {
            std::fputs("desktop helper self-test failed\n", stderr);
            std::exit(1);
        }
    };
    check(events.size() == 1 && events[0].value("event") == "ready");
    check(clipboard->text() == "existing private clipboard");
    bridge.consume("{\"op\":\"set_text\",\"text\":\"hello ");
    check(events.size() == 1);
    bridge.consume("\\u2603\",\"id\":\"set-1\"}\n");
    check(clipboard->text() == QString::fromUtf8("hello \xe2\x98\x83"));
    check(events.size() == 3 && events[1].value("event") == "text"
          && events[2].value("event") == "set" && events[2].value("ok") == true
          && events[2].value("id") == "set-1");
    bridge.consume("{\"op\":\"set_text\",\"text\":\"hello \\u2603\"}\n");
    check(events.size() == 4 && events.back().value("event") == "set");
    bridge.consume("not json\n{\"op\":\"set_text\",\"text\":\"\"}\n");
    check(events.size() == 6 && events[4].value("error") == "invalid_request"
          && events[5].value("ok") == false);
    bridge.consume("{\"op\":\"set_text\",\"text\":\"\\ud800\"}\n");
    check(events.back().value("event") == "set" && events.back().value("ok") == false);
    QJsonObject tooLarge{{"op", "set_text"}, {"text", QString(maxTextBytes + 1, 'x')}};
    bridge.consume(QJsonDocument(tooLarge).toJson(QJsonDocument::Compact) + '\n');
    check(events.back().value("event") == "set" && events.back().value("ok") == false);
    bridge.consume(QByteArray(maxLineBytes + 1, 'x'));
    check(events.back().value("error") == "line_too_large");
    const size_t count = events.size();
    bridge.consume("junk\n{\"op\":\"notify\",\"state\":\"arbitrary text\"}\n");
    check(events.size() == count + 1 && events.back().value("event") == "notification"
          && events.back().value("ok") == false);
    check(clipboard->text() == QString::fromUtf8("hello \xe2\x98\x83"));
    const size_t beforeImage = events.size();
    auto *mime = new QMimeData;
    mime->setData("image/png", QByteArray("not an image"));
    clipboard->setMimeData(mime);
    check(events.size() == beforeImage);
    clipboard->setText(QStringLiteral("hello \u2603"));
    check(events.size() == beforeImage + 1 && events.back().value("event") == "text");
    std::vector<QJsonObject> notificationEvents;
    Bridge notifications(false, [&notificationEvents](const QJsonObject &event) {
        notificationEvents.push_back(event);
        return true;
    });
    notifications.consume("{\"op\":\"set_text\",\"text\":\"disabled\"}\n");
    check(notificationEvents.size() == 2 && notificationEvents.back().value("ok") == false);
    check(clipboard->text() == QStringLiteral("hello \u2603"));
    notifications.consume("{\"op\":\"notify\",\"state\":\"invalid\",\"id\":\"notification-1\"}\n");
    check(notificationEvents.back().value("event") == "notification"
          && notificationEvents.back().value("ok") == false
          && notificationEvents.back().value("id") == "notification-1");
    std::fputs("desktop helper self-test passed\n", stderr);
    return 0;
}
} // namespace

int main(int argc, char **argv)
{
    if (argc == 2 && std::strcmp(argv[1], "--clipboard-event") == 0)
        return emitClipboardSelection();
    const bool testing = argc == 2 && std::strcmp(argv[1], "--self-test") == 0;
    const bool notificationsOnly = argc == 2
        && std::strcmp(argv[1], "--notifications-only") == 0;
    if (argc != 1 && !testing && !notificationsOnly) {
        std::fputs("desktop helper: invalid arguments\n", stderr);
        return 1;
    }
    if (testing)
        qputenv("QT_QPA_PLATFORM", "offscreen");
    qInstallMessageHandler(qtMessage);
    std::unique_ptr<QCoreApplication> app;
    if (notificationsOnly)
        app = std::make_unique<QCoreApplication>(argc, argv);
    else
        app = std::make_unique<QGuiApplication>(argc, argv);
    QCoreApplication::setApplicationName(QStringLiteral("vivo-desktop-helper"));
    if (testing)
        return selfTest();
    Bridge bridge(!notificationsOnly);
    if (!bridge.watchInput()) {
        std::fputs("desktop helper: cannot read input\n", stderr);
        return 1;
    }
    return app->exec();
}
