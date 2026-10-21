#pragma once
#include <QByteArray>
#include <QString>
#include <memory>

class PhoneAudio {
public:
    PhoneAudio();
    ~PhoneAudio();
    bool push(const QByteArray &adts);
    QString status() const;
    bool active() const;
    void stop();
    static bool selfTest();
private:
    struct Impl;
    std::unique_ptr<Impl> d;
};
