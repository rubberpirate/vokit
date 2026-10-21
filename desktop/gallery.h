#pragma once

#include <QHash>
#include <QObject>
#include <QPointer>
#include <QQueue>
#include <QString>
#include <memory>

class QDialog;
class QProcess;
class QTemporaryDir;
class QTreeWidget;
class QWidget;

class GalleryController : public QObject {
public:
    GalleryController(QTreeWidget *tree, QWidget *parent, bool testMode = false);
    ~GalleryController() override;
    void setEntries();
    void clear();
    void viewSelected();

private:
    struct Job {
        QString path;
        int page = 0;
        quint64 generation = 0;
        QByteArray output;
        bool failed = false;
    };
    QPointer<QTreeWidget> tree;
    QPointer<QWidget> window;
    bool testing;
    quint64 generation = 0;
    QQueue<Job> pending;
    QHash<QProcess *, Job> thumbnails;
    QPointer<QProcess> preview;
    QByteArray previewOutput;
    bool previewFailed = false;
    std::shared_ptr<QTemporaryDir> previewCache;
    QPointer<QDialog> viewer;

    void pumpThumbnails();
    void finishThumbnail(QProcess *process, int code);
    void finishPreview(QProcess *process, int code, const QString &output);
    void showPreview(const QString &path, const QString &mime, std::shared_ptr<QTemporaryDir> cache);
    void showError(const QString &message);
};
