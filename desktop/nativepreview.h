#pragma once

#include <QWidget>

class NativePreview : public QWidget {
public:
    enum class Mode { Screen, Camera };
    explicit NativePreview(QWidget *parent = nullptr, bool test = false, Mode mode = Mode::Screen);
    ~NativePreview() override;
    void setConnected(bool connected);
    void stop();
    void start();
    bool isRunning() const;
    void setPreviewVisible(bool visible);
    bool selfTest();

private:
    struct Impl;
    Impl *d;
};
