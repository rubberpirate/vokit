#include <QApplication>
#include <QButtonGroup>
#include <QDialog>
#include <QFrame>
#include <QListView>
#include <QSettings>
#include <QSortFilterProxyModel>
#include <QStackedWidget>
#include <QSet>
#include <QCheckBox>
#include <QCloseEvent>
#include <QDir>
#include <QFileDialog>
#include <QFileInfo>
#include <QFormLayout>
#include <QGroupBox>
#include <QHeaderView>
#include <QImageReader>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLabel>
#include <QLineEdit>
#include <QMainWindow>
#include <QMenuBar>
#include <QProcess>
#include <QPushButton>
#include <QRegularExpression>
#include <QSpinBox>
#include <QSplitter>
#include <QStandardPaths>
#include <QStyle>
#include <QSystemTrayIcon>
#include <QTimer>
#include <QTreeWidget>
#include <QVBoxLayout>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include "gallery.h"
#include "nativepreview.h"

namespace {
constexpr qsizetype pairLimit = 2 * 1024 * 1024;
constexpr qsizetype taskLimit = 1024 * 1024;

class Window : public QMainWindow {
public:
    explicit Window(bool testing = false) : testMode(testing)
    {
        QPalette light = palette();
        light.setColor(QPalette::Window, QColor("#f2f4f7"));
        light.setColor(QPalette::WindowText, QColor("#202630"));
        light.setColor(QPalette::Base, Qt::white);
        light.setColor(QPalette::AlternateBase, QColor("#f7f9fc"));
        light.setColor(QPalette::Text, QColor("#202630"));
        light.setColor(QPalette::Button, Qt::white);
        light.setColor(QPalette::ButtonText, QColor("#202630"));
        light.setColor(QPalette::Highlight, QColor("#e7f0ff"));
        light.setColor(QPalette::HighlightedText, QColor("#145ec4"));
        light.setColor(QPalette::PlaceholderText, QColor("#7f8997"));
        for (const auto role : {QPalette::WindowText,QPalette::Text,QPalette::ButtonText}) light.setColor(QPalette::Disabled,role,QColor("#97a1af"));
        setPalette(light);
        setWindowTitle("vivo Office Kit for Linux");
        resize(1260, 860);
        setMinimumSize(960, 680);
        setStyleSheet(R"(
            QMainWindow, QWidget#shell { background: #f2f4f7; color: #202630; }
            QWidget { font-size: 14px; color:#202630; }
            QWidget:disabled { color:#97a1af; }
            QDialog { background:#f2f4f7; }
            QFrame#card, QGroupBox { background: white; border: 1px solid #e9edf2; border-radius: 16px; }
            QGroupBox { margin-top: 12px; padding: 20px; font-weight: 600; }
            QGroupBox::title { subcontrol-origin: margin; left: 18px; padding: 0 5px; }
            QLabel#heading { font-size: 26px; font-weight: 650; }
            QLabel#subtle { color: #667181; }
            QPushButton { background: white; border: 1px solid #dce2eb; border-radius: 8px; padding: 9px 14px; }
            QPushButton:hover { background: #edf4ff; border-color: #a9cbff; }
            QPushButton:disabled { color: #97a1af; background: #f6f7f9; }
            QPushButton#primary { background: #1677ff; border-color: #1677ff; color: white; }
            QPushButton#primary:hover { background: #0969e8; }
            QPushButton#nav { text-align: left; border: 0; background: transparent; padding: 14px 18px; }
            QPushButton#nav:checked { color: #1677ff; background: #e6effe; font-weight: 600; }
            QLineEdit, QSpinBox, QComboBox { background: white; color:#202630; border: 1px solid #dce2eb; border-radius: 8px; padding: 9px; }
            QComboBox QAbstractItemView { background:white; color:#202630; selection-background-color:#e7f0ff; selection-color:#145ec4; }
            QTreeWidget, QListView { background: white; border: 0; outline: 0; selection-background-color: #e7f0ff; selection-color: #145ec4; }
            QListView::item { border-radius: 9px; padding: 6px; }
            QListView::item:hover { background: #f0f5ff; }
            QHeaderView::section { background: #f7f9fc; border: 0; padding: 10px; color: #667181; }
            QScrollBar:vertical { background: transparent; width: 9px; }
            QScrollBar::handle:vertical { background: #cbd2dc; border-radius: 4px; min-height: 24px; }
        )");
        auto *shell = new QWidget(this); shell->setObjectName("shell");
        auto *shellLayout = new QHBoxLayout(shell); shellLayout->setContentsMargins(16, 18, 22, 20); shellLayout->setSpacing(22);
        auto *sidebar = new QWidget(shell); sidebar->setFixedWidth(165);
        auto *navLayout = new QVBoxLayout(sidebar); navLayout->setContentsMargins(0, 8, 0, 0); navLayout->setSpacing(8);
        auto *brand = new QLabel("vivo", sidebar); brand->setStyleSheet("color: #1677ff; font-size: 34px; font-weight: 700; padding: 10px 18px;");
        navLayout->addWidget(brand);
        auto *edition = new QLabel("Office Kit · Linux", sidebar); edition->setObjectName("subtle"); edition->setMargin(16); navLayout->addWidget(edition);
        pages = new QStackedWidget(shell);
        auto *homePage = new QWidget(pages); auto *homeLayout = new QVBoxLayout(homePage); homeLayout->setContentsMargins(0,0,0,0); homeLayout->setSpacing(18);
        auto *filesPage = new QWidget(pages); auto *fileLayout = new QVBoxLayout(filesPage); fileLayout->setContentsMargins(0,0,0,0); fileLayout->setSpacing(14);
        auto *cameraPage = new QWidget(pages); auto *cameraLayout = new QVBoxLayout(cameraPage); cameraLayout->setContentsMargins(0,0,0,0);
        auto *settingsPage = new QWidget(pages); auto *settingsLayout = new QVBoxLayout(settingsPage); settingsLayout->setContentsMargins(0,0,0,0);
        pages->addWidget(homePage); pages->addWidget(filesPage); pages->addWidget(cameraPage); pages->addWidget(settingsPage);
        auto *navigation = new QButtonGroup(this); navigation->setExclusive(true);
        const QStringList destinations{"Home", "Phone files", "Images", "Videos", "Camera & mic", "Settings"};
        for (int i = 0; i < destinations.size(); ++i) {
            auto *button = new QPushButton(destinations[i], sidebar); button->setObjectName("nav"); button->setCheckable(true); navigation->addButton(button,i); navLayout->addWidget(button);
            if (i == 0) button->setChecked(true);
            connect(button, &QPushButton::clicked, this, [this,i] {
                filterKind = i == 2 ? "images" : i == 3 ? "videos" : QString{};
                pages->setCurrentIndex(i == 0 ? 0 : i <= 3 ? 1 : i == 4 ? 2 : 3);
                fileHeading->setText(i == 2 ? "Images in this folder" : i == 3 ? "Videos in this folder" : "Phone files");
                filterFiles();
                if (i >= 1 && i <= 3 && connected && tree->topLevelItemCount() == 0 && taskKind.isEmpty()) listFiles();
            });
        }
        navLayout->addStretch();
        auto *privacy = new QLabel("Native Linux connection\nYour files stay yours", sidebar); privacy->setObjectName("subtle"); privacy->setWordWrap(true); privacy->setMargin(14); navLayout->addWidget(privacy);
        shellLayout->addWidget(sidebar); shellLayout->addWidget(pages,1); setCentralWidget(shell);
        auto *welcome = new QLabel("Your phone, on your computer",homePage); welcome->setObjectName("heading"); homeLayout->addWidget(welcome);
        auto *connectionCard = new QFrame(homePage); connectionCard->setObjectName("card");
        auto *connectionLayout = new QVBoxLayout(connectionCard); connectionLayout->setContentsMargins(26,24,26,24); connectionLayout->setSpacing(14);
        deviceLabel = new QLabel("vivo phone",connectionCard); deviceLabel->setStyleSheet("font-size: 23px; font-weight: 650;"); connectionLayout->addWidget(deviceLabel);
        status = new QLabel("Not connected. Connect to create a pairing QR.",connectionCard); status->setTextFormat(Qt::PlainText); status->setObjectName("subtle"); status->setWordWrap(true); connectionLayout->addWidget(status);
        auto *connectionActions = new QHBoxLayout;
        connectButton = new QPushButton("Connect phone",connectionCard); connectButton->setObjectName("primary"); connectionActions->addWidget(connectButton);
        auto *connectionSettings = new QPushButton("Connection settings",connectionCard); connectionActions->addWidget(connectionSettings); connectionActions->addStretch(); connectionLayout->addLayout(connectionActions);
        connect(connectionSettings,&QPushButton::clicked,this,[this,navigation] { pages->setCurrentIndex(3); navigation->button(5)->setChecked(true); });
        connect(connectButton,&QPushButton::clicked,this,[this] { if (ownPair()) stopPair(); else { if (phone->text().trimmed().isEmpty() || (!trust->isChecked() && pin->text().isEmpty())) pages->setCurrentIndex(3); startPair(); } });
        qr = new QLabel(connectionCard); qr->setAlignment(Qt::AlignCenter); qr->hide(); connectionLayout->addWidget(qr);
        homeLayout->addWidget(connectionCard);
        auto *features = new QFrame(homePage); features->setObjectName("card"); auto *featureLayout = new QVBoxLayout(features); featureLayout->setContentsMargins(26,24,26,24);
        auto *mirrorTitle = new QLabel("Phone mirroring",features); mirrorTitle->setStyleSheet("font-size: 21px; font-weight: 600;"); featureLayout->addWidget(mirrorTitle);
        auto *mirrorDescription = new QLabel("Open a separate phone window to tap, type and swipe.\nCamera sharing keeps running independently.",features); mirrorDescription->setObjectName("subtle"); featureLayout->addWidget(mirrorDescription);
        mirrorButton = new QPushButton("Open phone mirror",features); mirrorButton->setObjectName("primary"); featureLayout->addWidget(mirrorButton,0,Qt::AlignLeft); homeLayout->addWidget(features); homeLayout->addStretch();
        mirrorWindow = new QDialog(this); mirrorWindow->setWindowTitle("Phone mirror"); mirrorWindow->resize(420,850); mirrorWindow->setMinimumSize(330,550);
        auto *mirrorLayout = new QVBoxLayout(mirrorWindow); mirrorLayout->setContentsMargins(5,5,5,5);
        phonePreview = new NativePreview(mirrorWindow,testMode,NativePreview::Mode::Screen); mirrorLayout->addWidget(phonePreview);
        connect(mirrorWindow,&QDialog::finished,this,[this] { phonePreview->stop(); });
        connect(mirrorButton,&QPushButton::clicked,this,[this] { phonePreview->setConnected(connected); mirrorWindow->show(); mirrorWindow->raise(); mirrorWindow->activateWindow(); if (!phonePreview->isRunning()) phonePreview->start(); });
        fileHeading = new QLabel("Phone files",filesPage); fileHeading->setObjectName("heading"); fileLayout->addWidget(fileHeading);
        auto *scope = new QLabel("Browse phone storage. Images, videos and search filter the current folder and page.",filesPage); scope->setObjectName("subtle"); scope->setWordWrap(true); fileLayout->addWidget(scope);
        auto *fileCard = new QFrame(filesPage); fileCard->setObjectName("card"); auto *fileCardLayout = new QVBoxLayout(fileCard); fileCardLayout->setContentsMargins(18,18,18,18); fileCardLayout->setSpacing(12);
        auto *fileRow = new QHBoxLayout;
        auto *up = new QPushButton("↑",fileCard); up->setToolTip("Parent folder"); up->setAccessibleName("Parent folder"); fileRow->addWidget(up);
        auto *root = new QPushButton("Storage",fileCard); fileRow->addWidget(root);
        directory = new QLineEdit("/storage/emulated/0/Download",fileCard); directory->setAccessibleName("Phone directory"); fileRow->addWidget(directory,1);
        page = new QSpinBox(fileCard); page->setRange(0,1000000); page->setPrefix("Page "); fileRow->addWidget(page);
        browse = new QPushButton("Refresh",fileCard); fileRow->addWidget(browse); fileCardLayout->addLayout(fileRow);
        connect(up,&QPushButton::clicked,this,[this] { const auto path=QDir::cleanPath(directory->text().trimmed()+"/.."); directory->setText(path); page->setValue(0); listFiles(); });
        connect(root,&QPushButton::clicked,this,[this] { directory->setText("/"); page->setValue(0); listFiles(); });
        connect(directory,&QLineEdit::returnPressed,this,[this] { page->setValue(0); listFiles(); });
        auto *actions = new QHBoxLayout;
        upload = new QPushButton("Import computer file",fileCard); upload->setObjectName("primary"); actions->addWidget(upload);
        download = new QPushButton("Save to computer",fileCard); actions->addWidget(download);
        viewMedia = new QPushButton("Open preview",fileCard); actions->addWidget(viewMedia);
        search = new QLineEdit(fileCard); search->setPlaceholderText("Search this folder"); search->setClearButtonEnabled(true); actions->addWidget(search,1);
        auto *gridButton = new QPushButton("Grid",fileCard); auto *listButton = new QPushButton("List",fileCard); actions->addWidget(gridButton); actions->addWidget(listButton); fileCardLayout->addLayout(actions);
        tree = new QTreeWidget(fileCard); tree->setHeaderLabels({"Name","Bytes","Type"}); tree->setRootIsDecorated(false); tree->setSelectionMode(QAbstractItemView::SingleSelection); tree->header()->setSectionResizeMode(0,QHeaderView::Stretch); tree->setIconSize(QSize(40,40));
        fileProxy = new QSortFilterProxyModel(this); fileProxy->setSourceModel(tree->model()); fileProxy->setFilterRole(Qt::UserRole+3); fileProxy->setFilterFixedString("visible");
        grid = new QListView(fileCard); grid->setModel(fileProxy); grid->setModelColumn(0); grid->setViewMode(QListView::IconMode); grid->setResizeMode(QListView::Adjust); grid->setMovement(QListView::Static); grid->setWrapping(true); grid->setWordWrap(true); grid->setIconSize(QSize(118,118)); grid->setGridSize(QSize(156,178)); grid->setSpacing(8); grid->setSelectionMode(QAbstractItemView::SingleSelection); grid->setUniformItemSizes(true);
        fileViews = new QStackedWidget(fileCard); fileViews->addWidget(grid); fileViews->addWidget(tree); fileCardLayout->addWidget(fileViews,1); fileLayout->addWidget(fileCard,1);
        connect(gridButton,&QPushButton::clicked,this,[this] { fileViews->setCurrentIndex(0); if (!testMode) QSettings().setValue("files/list",false); });
        connect(listButton,&QPushButton::clicked,this,[this] { fileViews->setCurrentIndex(1); if (!testMode) QSettings().setValue("files/list",true); });
        connect(search,&QLineEdit::textChanged,this,[this] { filterFiles(); });
        connect(grid->selectionModel(),&QItemSelectionModel::currentChanged,this,[this](const QModelIndex &index) { if (index.isValid()) tree->setCurrentItem(tree->topLevelItem(fileProxy->mapToSource(index).row())); else tree->setCurrentItem(nullptr); });
        connect(tree,&QTreeWidget::itemSelectionChanged,this,[this] { auto *item=tree->currentItem(); if(item) { const auto index=fileProxy->mapFromSource(tree->model()->index(tree->indexOfTopLevelItem(item),0)); if(index.isValid() && grid->currentIndex()!=index) grid->setCurrentIndex(index); } updateControls(); });
        auto openItem=[this](QTreeWidgetItem *item) { if(!item) return; if(item->data(0,Qt::UserRole+1).toBool()) { directory->setText(item->data(0,Qt::UserRole).toString()); page->setValue(0); listFiles(); } else gallery->viewSelected(); };
        connect(tree,&QTreeWidget::itemDoubleClicked,this,[openItem](QTreeWidgetItem *item,int) { openItem(item); });
        connect(grid,&QListView::doubleClicked,this,[this,openItem](const QModelIndex &index) { openItem(tree->topLevelItem(fileProxy->mapToSource(index).row())); });
        receipt = new QLabel(filesPage); receipt->setTextFormat(Qt::PlainText); receipt->setObjectName("subtle"); receipt->setWordWrap(true); receipt->setTextInteractionFlags(Qt::TextSelectableByMouse); fileLayout->addWidget(receipt);
        gallery = new GalleryController(tree,this,testMode);
        connect(browse,&QPushButton::clicked,this,[this] { listFiles(); }); connect(download,&QPushButton::clicked,this,[this] { downloadFile(); }); connect(upload,&QPushButton::clicked,this,[this] { uploadFile(); }); connect(viewMedia,&QPushButton::clicked,this,[this] { gallery->viewSelected(); });
        auto *cameraHeading = new QLabel("Camera & microphone",cameraPage); cameraHeading->setObjectName("heading"); cameraLayout->addWidget(cameraHeading);
        nativePreview = new NativePreview(cameraPage,testMode,NativePreview::Mode::Camera); cameraLayout->addWidget(nativePreview,1);
        auto *settingsHeading = new QLabel("Connection settings",settingsPage); settingsHeading->setObjectName("heading"); settingsLayout->addWidget(settingsHeading);
        settings = new QGroupBox("Pair your phone",settingsPage); auto *form = new QFormLayout(settings); form->setSpacing(15);
        phone = new QLineEdit(settings); phone->setPlaceholderText("192.168.1.20"); form->addRow("Phone Wi-Fi IP",phone);
        pin = new QLineEdit(settings); pin->setPlaceholderText("Optional independently verified certificate fingerprint"); form->addRow("Certificate SHA-256",pin);
        trust = new QCheckBox("Trust the scanned phone's first certificate for this connection",settings);
        clipboard = new QCheckBox("Share plain-text clipboard changes",settings); notifications = new QCheckBox("Show connection notifications",settings); files = new QCheckBox("Request phone file access",settings); notifications->setChecked(true); files->setChecked(true);
        for(auto *box:{trust,clipboard,notifications,files}) form->addRow(box);
        connect(trust,&QCheckBox::toggled,pin,[this](bool checked) { pin->setEnabled(!checked); });
        settingsLayout->addWidget(settings);
        auto *pairHere = new QPushButton("Create pairing QR",settingsPage); pairHere->setObjectName("primary"); settingsLayout->addWidget(pairHere,0,Qt::AlignLeft);
        connect(pairHere,&QPushButton::clicked,this,[this,navigation] { if(!ownPair() && !externalSession) startPair(); if(ownPair()) { pages->setCurrentIndex(0); navigation->button(0)->setChecked(true); } });
        auto *trustHelp = new QLabel("First-use trust applies only to the phone you approve after scanning. A certificate fingerprint can be supplied instead. Trust is never remembered.",settingsPage); trustHelp->setObjectName("subtle"); trustHelp->setWordWrap(true); settingsLayout->addWidget(trustHelp); settingsLayout->addStretch();
        if(!testMode) { QSettings preferences; phone->setText(preferences.value("connection/phone").toString()); clipboard->setChecked(preferences.value("connection/clipboard",false).toBool()); notifications->setChecked(preferences.value("connection/notifications",true).toBool()); files->setChecked(preferences.value("connection/files",true).toBool()); fileViews->setCurrentIndex(preferences.value("files/list",false).toBool()?1:0); }
        auto *quit = menuBar()->addMenu("File")->addAction("Quit");
        connect(quit, &QAction::triggered, this, [this] { quitWindow(); });
        if (!testMode && QSystemTrayIcon::isSystemTrayAvailable()) {
            tray = new QSystemTrayIcon(style()->standardIcon(QStyle::SP_ComputerIcon), this);
            tray->setToolTip("Vivo Desktop");
            auto *menu = new QMenu(this);
            connect(menu->addAction("Open"), &QAction::triggered, this, [this] { showNormal(); raise(); activateWindow(); });
            connect(menu->addAction("Quit"), &QAction::triggered, this, [this] { quitWindow(); });
            tray->setContextMenu(menu);
            connect(tray, &QSystemTrayIcon::activated, this, [this](QSystemTrayIcon::ActivationReason reason) {
                if (reason == QSystemTrayIcon::Trigger || reason == QSystemTrayIcon::DoubleClick) {
                    showNormal(); raise(); activateWindow();
                }
            });
            tray->show();
        }
        pair.setStandardErrorFile(QProcess::nullDevice());
        task.setStandardErrorFile(QProcess::nullDevice());
        connect(&pair, &QProcess::readyReadStandardOutput, this, [this] { readPair(); });
        connect(&pair, &QProcess::finished, this, [this](int code, QProcess::ExitStatus) {
            readPair(); pairBuffer.clear(); connected = fileAllowed = externalSession = false;
            clearQR(); tree->clear(); updateControls();
            status->setText(code == 0 || stopping ? "Disconnected." : "Connection stopped. Check the phone, LAN address, trust setting, and required tools.");
            if (quitting) qApp->quit();
        });
        connect(&pair, &QProcess::errorOccurred, this, [this](QProcess::ProcessError error) {
            if (error == QProcess::FailedToStart) {
                status->setText("Cannot start vivolinux. Install it beside this desktop application.");
                updateControls();
                if (quitting) qApp->quit();
            }
        });
        connect(&task, &QProcess::readyReadStandardOutput, this, [this] { readTask(); });
        connect(&task, &QProcess::finished, this, [this](int code, QProcess::ExitStatus) { finishTask(code); });
        connect(&task, &QProcess::errorOccurred, this, [this](QProcess::ProcessError error) {
            if (error == QProcess::FailedToStart) finishTask(-1);
        });
        taskTimer.setSingleShot(true);
        connect(&taskTimer, &QTimer::timeout, this, [this] { taskFailed = true; task.kill(); });
        connect(&poll, &QTimer::timeout, this, [this] {
            if (taskKind.isEmpty() && !quitting) runTask("status", {"native-status"});
        });
        updateControls();
        if (!testMode) {
            probing = true;
            updateControls();
            QTimer::singleShot(0, this, [this] { runTask("status", {"native-status"}); });
            poll.start(4000);
        }
    }

    bool selfTest()
    {
        const bool defaults = testMode && !ownPair() && task.state() == QProcess::NotRunning
            && !trust->isChecked() && !clipboard->isChecked()
            && notifications->isChecked() && files->isChecked() && !browse->isEnabled()
            && !upload->isEnabled();
        if (!defaults || !nativePreview->selfTest()) return false;
        runTask("status", {"native-status"});
        startPair();
        if (ownPair() || task.state() != QProcess::NotRunning) return false;
        qr->setText("test QR placeholder"); qr->show();
        pairEvent({{"stage", "control_connected"}});
        if (!upload->isEnabled() || !browse->isEnabled()) return false;
        pairEvent({{"stage", "file_permission_granted"}});
        if (!qr->isHidden() || !browse->isEnabled() || !upload->isEnabled()) return false;
        taskKind = "files"; listedPage = 2;
        taskBuffer = R"({"dataList":[{"fileName":"fixture.txt","savePath":"/storage/emulated/0/Download/fixture.txt","fileSize":7,"isDirectory":false}],"totalCount":1})";
        finishTask(0);
        auto *item = tree->topLevelItem(0);
        if (!item || item->data(0, Qt::UserRole).toString() != "/storage/emulated/0/Download/fixture.txt"
            || item->data(0, Qt::UserRole + 2).toInt() != 2) return false;
        if (fileProxy->rowCount()!=1 || grid->model()!=fileProxy) return false;
        search->setText("absent"); if(fileProxy->rowCount()!=0) return false;
        search->clear(); if(fileProxy->rowCount()!=1) return false;
        tree->setCurrentItem(item);
        if (!download->isEnabled()) return false;
        pairEvent({{"stage", "file_permission_denied"}});
        if (!browse->isEnabled() || !download->isEnabled() || !upload->isEnabled()) return false;
        taskKind = "download";
        taskBuffer = QJsonDocument(QJsonObject{{"bytes", 7}, {"sha256", QString(64, 'a')}}).toJson();
        finishTask(0);
        if (!receipt->text().contains(QString(64, 'a'))) return false;
        taskKind = "upload";
        updateControls();
        if (upload->isEnabled()) return false;
        taskBuffer = QJsonDocument(QJsonObject{{"bytes", 7}, {"fileName", "generated.txt"},
            {"savePath", "/storage/emulated/0/Download/generated.txt"},
            {"local_sha256", QString(64, 'b')}}).toJson();
        finishTask(0);
        if (!upload->isEnabled() || !receipt->text().contains("Local SHA-256: " + QString(64, 'b'))
            || !receipt->text().contains("Peer integrity has not been verified.")) return false;
        pairEvent({{"stage", "control_disconnected"}});
        if (connected || browse->isEnabled() || upload->isEnabled() || !qr->isHidden()) return false;
        close();
        return isVisible() && isMinimized() && !ownPair() && task.state() == QProcess::NotRunning;
    }

protected:
    void closeEvent(QCloseEvent *event) override
    {
        if (quitting) { event->accept(); return; }
        event->ignore();
        if (tray && tray->isVisible() && QSystemTrayIcon::isSystemTrayAvailable()) hide();
        else showMinimized();
    }

private:
    QString cli() const { return QCoreApplication::applicationDirPath() + "/vivolinux"; }
    bool ownPair() const { return pair.state() != QProcess::NotRunning; }
    void clearQR() { qr->clear(); qr->hide(); }
    void updateControls()
    {
        settings->setEnabled(!ownPair() && !externalSession);
        connectButton->setText(ownPair() ? "Disconnect" : externalSession ? "Connected" : "Connect phone");
        deviceLabel->setText(connected ? "vivo phone · Connected" : "vivo phone");
        mirrorButton->setEnabled(connected && !quitting);
        phonePreview->setConnected(connected && !quitting);
        connectButton->setEnabled(!quitting && !probing && (ownPair() || !externalSession));
        const bool available = connected && taskKind.isEmpty() && !quitting;
        browse->setEnabled(available);
        directory->setEnabled(available);
        page->setEnabled(available);
        upload->setEnabled(connected && taskKind.isEmpty() && !quitting);
        auto *item = tree->currentItem();
        download->setEnabled(available && item && !item->data(0, Qt::UserRole + 1).toBool());
        viewMedia->setEnabled(available && item && !item->data(0, Qt::UserRole + 1).toBool());
        nativePreview->setConnected(connected && !quitting);
        if (!connected) gallery->clear();
    }
    void filterFiles()
    {
        const QSet<QString> images{"jpg","jpeg","png","gif","heic","heif","avif","webp","bmp"};
        const QSet<QString> videos{"mp4","m4v","mov","webm","mkv","avi","3gp"};
        for(int i=0;i<tree->topLevelItemCount();++i) {
            auto *item=tree->topLevelItem(i); const bool folder=item->data(0,Qt::UserRole+1).toBool();
            const auto extension=QFileInfo(item->text(0)).suffix().toLower();
            const bool visible=item->text(0).contains(search->text(),Qt::CaseInsensitive)
                && (filterKind.isEmpty() || folder || (filterKind=="images"?images.contains(extension):videos.contains(extension)));
            item->setData(0,Qt::UserRole+3,visible?"visible":"hidden"); tree->setRowHidden(i,QModelIndex(),!visible);
            if(!visible && tree->currentItem()==item) tree->setCurrentItem(nullptr);
        }
    }
    void startPair()
    {
        if (ownPair() || externalSession || probing || testMode) return;
        if (phone->text().trimmed().isEmpty()) { status->setText("Enter the phone's private LAN IP address."); return; }
        if (!trust->isChecked() && pin->text().trimmed().isEmpty()) {
            status->setText("Provide a verified certificate fingerprint or explicitly enable first-use trust."); return;
        }
        QStringList args{"pair", "--phone", phone->text().trimmed(), "--persistent", "--state-dir",
                         QStandardPaths::writableLocation(QStandardPaths::AppLocalDataLocation) + "/pairing"};
        if (trust->isChecked()) args << "--trust-scanned-phone";
        else args << "--phone-cert-sha256" << pin->text().trimmed();
        if (clipboard->isChecked()) args << "--clipboard";
        if (notifications->isChecked()) args << "--notifications";
        if (files->isChecked()) args << "--file-access";
        QSettings preferences; preferences.setValue("connection/phone",phone->text().trimmed()); preferences.setValue("connection/clipboard",clipboard->isChecked()); preferences.setValue("connection/notifications",notifications->isChecked()); preferences.setValue("connection/files",files->isChecked());
        stopping = false; pairBuffer.clear(); receipt->clear(); tree->clear(); clearQR();
        status->setText("Creating a pairing QR…");
        pair.start(cli(), args);
        updateControls();
    }
    void stopPair()
    {
        if (!ownPair()) return;
        stopping = true; clearQR(); status->setText("Disconnecting…"); pair.terminate();
        QTimer::singleShot(5000, this, [this] { if (stopping && ownPair()) pair.kill(); });
    }
    void quitWindow()
    {
        nativePreview->stop();phonePreview->stop();mirrorWindow->hide();gallery->clear();
        quitting = true; poll.stop(); task.kill();
        if (ownPair()) stopPair(); else qApp->quit();
    }
    void readPair()
    {
        while (pair.bytesAvailable() > 0) {
            const QByteArray chunk = pair.read(8192);
            for (const char byte : chunk) {
                if (byte != '\n') {
                    if (pairBuffer.size() >= pairLimit) {
                        pairBuffer.clear(); stopPair(); status->setText("Connection output exceeded its safety limit."); return;
                    }
                    pairBuffer.append(byte);
                } else {
                    QJsonParseError error;
                    const auto doc = QJsonDocument::fromJson(pairBuffer, &error);
                    pairBuffer.clear();
                    if (error.error != QJsonParseError::NoError || !doc.isObject()) {
                        stopPair(); status->setText("Connection returned an invalid status response."); return;
                    }
                    pairEvent(doc.object());
                }
            }
        }
    }
    void pairEvent(const QJsonObject &event)
    {
        const QString stage = event.value("stage").toString();
        if (stage == "qr_ready") {
            const QFileInfo image(event.value("qr_path").toString());
            const QPixmap pixmap(image.isFile() && image.size() <= pairLimit ? image.absoluteFilePath() : QString{});
            if (pixmap.isNull()) status->setText("Pairing QR unavailable. Check qrencode and reconnect.");
            else { qr->setPixmap(pixmap.scaled(280, 280, Qt::KeepAspectRatio, Qt::FastTransformation)); qr->show(); status->setText("Scan this QR with Office Kit on the phone."); }
        } else if (stage == "control_connected") {
            connected = true; clearQR(); status->setText("Connected. Approve requested permissions on the phone.");
        } else if (stage == "control_disconnected") {
            connected = fileAllowed = false; clearQR(); status->setText("Phone disconnected.");
        } else if (stage == "file_permission_granted") {
            fileAllowed = true; status->setText("Connected. Phone file access is ready.");
        } else if (stage == "file_permission_denied" || stage == "file_permission_timeout") {
            fileAllowed = false; status->setText("Connected. File access was not approved on the phone.");
        } else if (stage == "mirror_permission_denied" || stage == "mirror_preview_stopped") {
            status->setText("Connected. Experimental video preview is unavailable or stopped.");
        }
        updateControls();
    }
    void runTask(const QString &kind, const QStringList &args)
    {
        if (!taskKind.isEmpty() || testMode || quitting) return;
        taskKind = kind; taskBuffer.clear(); taskFailed = false;
        task.start(cli(), args);
        taskTimer.start(kind == "status" ? 3000 : (kind == "download" || kind == "upload") ? 180000 : 45000);
        updateControls();
    }
    void readTask()
    {
        while (task.bytesAvailable() > 0) {
            const QByteArray chunk = task.read(8192);
            if (chunk.size() > taskLimit - taskBuffer.size()) { taskFailed = true; task.kill(); return; }
            taskBuffer.append(chunk);
        }
    }
    void finishTask(int code)
    {
        if (taskKind.isEmpty()) return;
        readTask(); taskTimer.stop();
        const QString kind = taskKind;
        taskKind.clear();
        QJsonParseError error;
        const auto doc = QJsonDocument::fromJson(taskBuffer, &error);
        const bool ok = code == 0 && !taskFailed && error.error == QJsonParseError::NoError && doc.isObject();
        taskBuffer.clear();
        const auto object = doc.object();
        if (kind == "status") {
            probing = false;
            if (!ownPair()) {
                externalSession = ok;
                connected = ok && object.value("connected").toBool();
                fileAllowed = connected && object.value("file_permission").toBool();
                status->setText(connected ? "Connected through an existing native session." : externalSession ? "An existing native session is starting or stopping." : "Not connected. Connect to create a pairing QR.");
                if (!connected) { clearQR(); tree->clear(); }
            } else if (ok) {
                fileAllowed = object.value("file_permission").toBool();
            }
        } else if (!ok) {
            if (object.value("error").toString() == "file_permission_required")
                receipt->setText("Approve file access in Office Kit on the phone, then retry. Your next file action will request access when needed.");
            else receipt->setText(kind == "download" ? "Download failed. Check phone permission and choose a new destination file." : kind == "upload" ? "Upload failed. Check the local file, phone destination, and session log." : "Cannot browse files. Check phone connection and directory.");
        }
        else if (kind == "files") {
            tree->clear();
            const auto entries = object.value("dataList").toArray();
            if (!object.value("dataList").isArray() || entries.size() > 2000) receipt->setText("Phone file listing was invalid or too large.");
            else {
                for (const auto &value : entries) {
                    const auto entry = value.toObject();
                    auto *item = new QTreeWidgetItem(tree, {entry.value("fileName").toString(), QString::number(entry.value("fileSize").toInteger()), entry.value("isDirectory").toBool() ? "Folder" : "File"});
                    item->setData(0, Qt::UserRole, entry.value("savePath").toString());
                    item->setData(0, Qt::UserRole + 1, entry.value("isDirectory").toBool());
                    item->setData(0, Qt::UserRole + 2, listedPage);
                }
                receipt->setText(QString("%1 entries shown; %2 total. Double-click a folder to open it.").arg(entries.size()).arg(object.value("totalCount").toInteger()));
                for(int i=0;i<tree->topLevelItemCount();++i) { auto *item=tree->topLevelItem(i); item->setIcon(0,style()->standardIcon(item->data(0,Qt::UserRole+1).toBool()?QStyle::SP_DirIcon:QStyle::SP_FileIcon)); }
                filterFiles(); gallery->setEntries();
            }
        } else if (kind == "download") {
            const QString hash = object.value("sha256").toString();
            if (!QRegularExpression("^[a-fA-F0-9]{64}$").match(hash).hasMatch()) receipt->setText("Download finished without a valid SHA-256 receipt.");
            else receipt->setText(QString("Downloaded %1 bytes.\nSHA-256: %2").arg(object.value("bytes").toInteger()).arg(hash));
        } else if (kind == "upload") {
            const QString hash = object.value("local_sha256").toString();
            if (!QRegularExpression("^[a-fA-F0-9]{64}$").match(hash).hasMatch()) receipt->setText("Upload finished without a valid local SHA-256 receipt.");
            else receipt->setText(QString("Uploaded %1 bytes to %2.\nLocal SHA-256: %3\nPeer integrity has not been verified.")
                .arg(object.value("bytes").toInteger()).arg(object.value("savePath").toString(), hash));
        }
        updateControls();
    }
    void listFiles()
    {
        if (!connected || !taskKind.isEmpty()) return;
        listedPage = page->value();
        receipt->setText("Reading phone directory…");
        runTask("files", {"files", "--path", directory->text().trimmed(), "--page", QString::number(listedPage)});
    }
    void downloadFile()
    {
        auto *item = tree->currentItem();
        if (!connected || !taskKind.isEmpty() || !item || item->data(0, Qt::UserRole + 1).toBool()) return;
        const QString remote = item->data(0, Qt::UserRole).toString();
        const QString selectedPage = QString::number(item->data(0, Qt::UserRole + 2).toInt());
        const QString output = QFileDialog::getSaveFileName(this, "Download phone file", QDir::homePath() + "/" + item->text(0), {}, nullptr, QFileDialog::DontConfirmOverwrite);
        if (output.isEmpty()) return;
        if (QFileInfo::exists(output) || QFileInfo(output).isSymLink()) { receipt->setText("Choose a new file name. Existing files are never replaced."); return; }
        receipt->setText("Downloading selected phone file…");
        runTask("download", {"download", "--remote", remote, "--output", output, "--page", selectedPage});
    }
    void uploadFile()
    {
        if (!connected || !taskKind.isEmpty()) return;
        const QString local = QFileDialog::getOpenFileName(this, "Upload local file", QDir::homePath());
        if (local.isEmpty()) return;
        const QFileInfo selected(local);
        if (!selected.isFile() || selected.isSymLink()) { receipt->setText("Choose a regular local file, not a symbolic link."); return; }
        if (!connected || !taskKind.isEmpty()) { receipt->setText("Connect before uploading. File access will be requested when needed."); return; }
        receipt->setText("Uploading selected local file…");
        runTask("upload", {"upload", "--local", local, "--to", directory->text().trimmed()});
    }

    bool testMode, connected = false, fileAllowed = false, externalSession = false;
    bool probing = false, quitting = false, stopping = false, taskFailed = false;
    int listedPage = 0;
    QGroupBox *settings;
    QLineEdit *phone, *pin, *directory, *search;
    QStackedWidget *pages, *fileViews;
    QListView *grid;
    QSortFilterProxyModel *fileProxy;
    QDialog *mirrorWindow;
    NativePreview *phonePreview;
    QLabel *fileHeading, *deviceLabel;
    QPushButton *mirrorButton;
    QString filterKind;
    QCheckBox *trust, *clipboard, *notifications, *files;
    QPushButton *connectButton, *browse, *download, *upload, *viewMedia;
    GalleryController *gallery;
    NativePreview *nativePreview;
    QLabel *status, *qr, *receipt;
    QTreeWidget *tree;
    QSpinBox *page;
    QSystemTrayIcon *tray = nullptr;
    QProcess pair, task;
    QTimer poll, taskTimer;
    QByteArray pairBuffer, taskBuffer;
    QString taskKind;
};
} // namespace

int main(int argc, char **argv)
{
    const bool testing = argc == 2 && std::strcmp(argv[1], "--self-test") == 0;
    if (argc != 1 && !testing) { std::fputs("desktop window: invalid arguments\n", stderr); return 1; }
    if (testing) qputenv("QT_QPA_PLATFORM", "offscreen");
    qInstallMessageHandler([](QtMsgType type, const QMessageLogContext &, const QString &) {
        std::fputs("desktop window: Qt diagnostic\n", stderr);
        if (type == QtFatalMsg) std::abort();
    });
    QApplication app(argc, argv);
    app.setApplicationName("vivo-desktop");
    app.setQuitOnLastWindowClosed(false);
    QImageReader::setAllocationLimit(32);
    Window window(testing);
    window.show();
    if (testing) QTimer::singleShot(50, &app, [&] {
        const bool ok = window.selfTest();
        std::fputs(ok ? "desktop window self-test passed\n" : "desktop window self-test failed\n", stderr);
        app.exit(ok ? 0 : 1);
    });
    return app.exec();
}
