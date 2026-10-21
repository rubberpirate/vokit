#include "gallery.h"

#include <QAudioOutput>
#include <QBuffer>
#include <QCoreApplication>
#include <QDialog>
#include <QDir>
#include <QFileInfo>
#include <QHBoxLayout>
#include <QImageReader>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLabel>
#include <QMediaPlayer>
#include <QMessageBox>
#include <QProcess>
#include <QPushButton>
#include <QScrollArea>
#include <QSet>
#include <QSlider>
#include <QTemporaryDir>
#include <QTimer>
#include <QTreeWidget>
#include <QUrl>
#include <QVBoxLayout>
#include <QVideoWidget>
#include <limits>

namespace {
constexpr qsizetype jsonLimit = 1024 * 1024;
constexpr qsizetype thumbnailLimit = 512 * 1024;
constexpr int maximumJobs = 200;

bool mediaName(const QString &path)
{
    static const QSet<QString> extensions = {
        "jpg", "jpeg", "png", "gif", "mp4", "m4v", "mov", "webm", "mkv", "avi", "3gp",
        "heic", "heif", "avif", "webp", "bmp"
    };
    return extensions.contains(QFileInfo(path).suffix().toLower());
}

bool previewName(const QString &path)
{
    static const QSet<QString> extensions = {
        "jpg", "jpeg", "png", "gif", "mp4", "m4v", "mov", "webm", "mkv", "avi", "3gp"
    };
    return extensions.contains(QFileInfo(path).suffix().toLower());
}

QString executable()
{
    return QCoreApplication::applicationDirPath() + "/vivolinux";
}

bool collectOutput(QProcess *process, QByteArray &output)
{
    // Never retain more than one bounded JSON response per child process.
    process->setReadChannel(QProcess::StandardOutput);
    const qsizetype remaining = jsonLimit - output.size();
    if (process->bytesAvailable() > remaining) return false;
    output += process->read(remaining);
    return output.size() <= jsonLimit;
}

bool safePath(const QString &path)
{
    return !path.isEmpty() && path.startsWith('/') && !path.contains(QChar::Null)
        && !path.contains('\\') && QDir::cleanPath(path) == path;
}
}

GalleryController::GalleryController(QTreeWidget *fileTree, QWidget *parent, bool testMode)
    : QObject(parent), tree(fileTree), window(parent), testing(testMode)
{
    if (tree) tree->setIconSize(QSize(96, 96));
}

GalleryController::~GalleryController()
{
    clear();
}

void GalleryController::clear()
{
    ++generation;
    pending.clear();
    const auto processes = thumbnails.keys();
    thumbnails.clear();
    for (auto *process : processes) {
        process->disconnect(this);
        process->kill();
        process->waitForFinished(100);
        process->deleteLater();
    }
    if (preview) {
        preview->disconnect(this);
        preview->kill();
        preview->waitForFinished(100);
        preview->deleteLater();
        preview = nullptr;
    }
    previewOutput.clear();
    previewFailed = false;
    previewCache.reset();
    if (viewer) {
        delete viewer.data();
        viewer = nullptr;
    }
}

void GalleryController::setEntries()
{
    // Existing item pointers may already have been deleted by a list refresh.
    ++generation;
    pending.clear();
    if (!tree || testing) return;
    for (int i = 0; i < tree->topLevelItemCount() && pending.size() < maximumJobs; ++i) {
        auto *item = tree->topLevelItem(i);
        const QString path = item->data(0, Qt::UserRole).toString();
        if (!item->data(0, Qt::UserRole + 1).toBool() && safePath(path) && mediaName(path)) {
            Job job;
            job.path = path;
            job.page = item->data(0, Qt::UserRole + 2).toInt();
            job.generation = generation;
            if (job.page >= 0) pending.enqueue(job);
        }
    }
    pumpThumbnails();
}

void GalleryController::pumpThumbnails()
{
    while (!testing && tree && thumbnails.size() < 2 && !pending.isEmpty()) {
        const Job job = pending.dequeue();
        auto *process = new QProcess(this);
        thumbnails.insert(process, job);
        process->setStandardErrorFile(QProcess::nullDevice());
        connect(process, &QProcess::readyReadStandardOutput, this, [this, process] {
            auto it = thumbnails.find(process);
            if (it != thumbnails.end() && !collectOutput(process, it->output)) {
                it->failed = true;
                process->kill();
            }
        });
        connect(process, &QProcess::finished, this, [this, process](int code, QProcess::ExitStatus) {
            finishThumbnail(process, code);
        });
        connect(process, &QProcess::errorOccurred, this, [this, process](QProcess::ProcessError error) {
            if (error == QProcess::FailedToStart) finishThumbnail(process, -1);
        });
        QTimer::singleShot(90000, process, [this, process] {
            auto it = thumbnails.find(process);
            if (it != thumbnails.end()) {
                it->failed = true;
                process->kill();
            }
        });
        process->start(executable(), {"thumbnail", "--remote", job.path, "--page", QString::number(job.page)});
    }
}

void GalleryController::finishThumbnail(QProcess *process, int code)
{
    auto it = thumbnails.find(process);
    if (it == thumbnails.end()) return;
    if (!collectOutput(process, it->output)) it->failed = true;
    const Job job = it.value();
    thumbnails.erase(it);
    process->deleteLater();
    if (!job.failed && code == 0 && job.generation == generation && tree) {
        const auto object = QJsonDocument::fromJson(job.output).object();
        const QByteArray encoded = object.value("data").toString().toLatin1();
        const QByteArray data = QByteArray::fromBase64(encoded, QByteArray::AbortOnBase64DecodingErrors);
        if (!data.isEmpty() && data.size() <= thumbnailLimit) {
            QBuffer buffer;
            buffer.setData(data);
            buffer.open(QIODevice::ReadOnly);
            QImageReader reader(&buffer);
            const auto size = reader.size();
            const QByteArray format = reader.format();
            const QString expected = format == "jpg" ? "image/jpeg" : "image/" + QString::fromLatin1(format);
            if ((format == "png" || format == "jpeg" || format == "jpg" || format == "gif")
                && object.value("mime").toString() == expected && size.isValid()
                && size.width() <= 4096 && size.height() <= 4096
                && qint64(size.width()) * size.height() <= 16 * 1024 * 1024) {
                reader.setScaledSize(size.scaled(96, 96, Qt::KeepAspectRatio));
                const QImage image = reader.read();
                if (!image.isNull()) {
                    for (int i = 0; i < tree->topLevelItemCount(); ++i) {
                        auto *item = tree->topLevelItem(i);
                        if (item->data(0, Qt::UserRole).toString() == job.path
                            && item->data(0, Qt::UserRole + 2).toInt() == job.page) {
                            item->setIcon(0, QPixmap::fromImage(image));
                            break;
                        }
                    }
                }
            }
        }
    }
    pumpThumbnails();
}

void GalleryController::viewSelected()
{
    if (testing || !tree || preview) return;
    auto *item = tree->currentItem();
    if (!item || item->data(0, Qt::UserRole + 1).toBool()) return;
    const QString path = item->data(0, Qt::UserRole).toString();
    const int page = item->data(0, Qt::UserRole + 2).toInt();
    if (!safePath(path) || page < 0 || !previewName(path)) {
        showError("Preview supports JPEG, PNG, GIF and common video containers. Download other formats to open them separately.");
        return;
    }
    previewCache = std::make_shared<QTemporaryDir>(QDir::tempPath() + "/vivo-gallery-XXXXXX");
    if (!previewCache->isValid()) {
        previewCache.reset();
        showError("Cannot create a private media cache.");
        return;
    }
    const QString output = previewCache->filePath("media." + QFileInfo(path).suffix().toLower());
    auto *process = new QProcess(this);
    preview = process;
    previewOutput.clear();
    previewFailed = false;
    process->setStandardErrorFile(QProcess::nullDevice());
    connect(process, &QProcess::readyReadStandardOutput, this, [this, process] {
        if (preview == process && !collectOutput(process, previewOutput)) {
            previewFailed = true;
            process->kill();
        }
    });
    connect(process, &QProcess::finished, this, [this, process, output](int code, QProcess::ExitStatus) {
        finishPreview(process, code, output);
    });
    connect(process, &QProcess::errorOccurred, this, [this, process, output](QProcess::ProcessError error) {
        if (error == QProcess::FailedToStart) finishPreview(process, -1, output);
    });
    QTimer::singleShot(10 * 60 * 1000, process, [this, process] {
        if (preview == process) {
            previewFailed = true;
            process->kill();
        }
    });
    process->start(executable(), {"preview", "--remote", path, "--output", output, "--page", QString::number(page)});
}

void GalleryController::finishPreview(QProcess *process, int code, const QString &output)
{
    if (preview != process) return;
    if (!collectOutput(process, previewOutput)) previewFailed = true;
    const auto object = QJsonDocument::fromJson(previewOutput).object();
    const QString mime = object.value("mime").toString();
    const QFileInfo file(output);
    const qint64 bytes = qint64(object.value("bytes").toDouble(-1));
    const qint64 limit = mime.startsWith("image/") ? 32LL * 1024 * 1024 : 256LL * 1024 * 1024;
    const bool valid = !previewFailed && code == 0 && object.value("path").toString() == output
        && (mime.startsWith("image/") || mime.startsWith("video/")) && file.isFile() && !file.isSymLink()
        && file.size() > 0 && file.size() == bytes && file.size() <= limit
        && file.canonicalPath() == QFileInfo(previewCache->path()).canonicalFilePath();
    preview = nullptr;
    previewOutput.clear();
    process->deleteLater();
    auto cache = previewCache;
    previewCache.reset();
    if (!valid) {
        showError("Cannot prepare this preview. Check phone access, file size and supported media format.");
        return;
    }
    showPreview(output, mime, std::move(cache));
}

void GalleryController::showPreview(const QString &path, const QString &mime, std::shared_ptr<QTemporaryDir> cache)
{
    if (viewer) delete viewer.data();
    auto *dialog = new QDialog(window);
    viewer = dialog;
    dialog->setAttribute(Qt::WA_DeleteOnClose);
    dialog->setWindowTitle("Phone media preview");
    dialog->resize(900, 650);
    auto *layout = new QVBoxLayout(dialog);
    connect(dialog, &QObject::destroyed, this, [cache] { /* keep private cache alive until the viewer closes */ });
    if (mime.startsWith("image/")) {
        QImageReader reader(path);
        const auto size = reader.size();
        if (!size.isValid() || size.width() > 32768 || size.height() > 32768
            || qint64(size.width()) * size.height() > 100LL * 1024 * 1024) {
            delete dialog;
            showError("The image dimensions exceed preview limits.");
            return;
        }
        reader.setAutoTransform(true);
        reader.setScaledSize(size.scaled(1600, 1200, Qt::KeepAspectRatio));
        const auto image = reader.read();
        if (image.isNull()) {
            delete dialog;
            showError("The image could not be decoded.");
            return;
        }
        auto *scroll = new QScrollArea(dialog);
        auto *label = new QLabel(scroll);
        label->setAlignment(Qt::AlignCenter);
        label->setPixmap(QPixmap::fromImage(image));
        scroll->setWidget(label);
        scroll->setWidgetResizable(true);
        layout->addWidget(scroll);
    } else {
        auto *video = new QVideoWidget(dialog);
        layout->addWidget(video, 1);
        auto *player = new QMediaPlayer(dialog);
        auto *audio = new QAudioOutput(dialog);
        audio->setVolume(0.5f);
        player->setAudioOutput(audio);
        player->setVideoOutput(video);
        auto *controls = new QHBoxLayout;
        auto *play = new QPushButton("Play", dialog);
        auto *seek = new QSlider(Qt::Horizontal, dialog);
        seek->setRange(0, 0);
        controls->addWidget(play);
        controls->addWidget(seek, 1);
        layout->addLayout(controls);
        auto *message = new QLabel("Downloaded media is ready. Press Play.", dialog);
        message->setTextFormat(Qt::PlainText);
        layout->addWidget(message);
        connect(play, &QPushButton::clicked, player, [player] {
            if (player->playbackState() == QMediaPlayer::PlayingState) player->pause(); else player->play();
        });
        connect(player, &QMediaPlayer::playbackStateChanged, play, [play](QMediaPlayer::PlaybackState state) {
            play->setText(state == QMediaPlayer::PlayingState ? "Pause" : "Play");
        });
        connect(player, &QMediaPlayer::durationChanged, seek, [seek](qint64 duration) {
            seek->setRange(0, int(qMin(duration, qint64(std::numeric_limits<int>::max()))));
        });
        connect(player, &QMediaPlayer::positionChanged, seek, [seek](qint64 position) {
            if (!seek->isSliderDown()) seek->setValue(int(position));
        });
        connect(seek, &QSlider::sliderMoved, player, [player](int position) { player->setPosition(position); });
        connect(player, &QMediaPlayer::errorOccurred, message, [message](QMediaPlayer::Error, const QString &) {
            message->setText("This video could not be played with the installed codecs.");
        });
        player->setSource(QUrl::fromLocalFile(path));
    }
    dialog->show();
}

void GalleryController::showError(const QString &message)
{
    if (!testing && window) QMessageBox::information(window, "Media preview", message);
}
