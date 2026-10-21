cmake_minimum_required(VERSION 3.16)
project(vivo_desktop_helper LANGUAGES CXX)

find_package(Qt6 6.2 REQUIRED COMPONENTS Core Gui DBus)

add_executable(vivo-desktop-helper main.cpp)
target_compile_features(vivo-desktop-helper PRIVATE cxx_std_17)
target_link_libraries(vivo-desktop-helper PRIVATE Qt6::Core Qt6::Gui Qt6::DBus)
target_compile_options(vivo-desktop-helper PRIVATE -Wall -Wextra -Wpedantic)

option(BUILD_DESKTOP_WINDOW "Build the Qt Widgets desktop window" ON)
if(BUILD_DESKTOP_WINDOW)
  find_package(Qt6 6.2 REQUIRED COMPONENTS Widgets Network Multimedia MultimediaWidgets)
  find_package(PkgConfig REQUIRED)
  pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET libavcodec libavutil libswscale libswresample)
  add_executable(vivo-desktop window.cpp gallery.cpp nativepreview.cpp phoneaudio.cpp)
  target_compile_features(vivo-desktop PRIVATE cxx_std_17)
  target_link_libraries(vivo-desktop PRIVATE Qt6::Widgets Qt6::Network Qt6::Multimedia Qt6::MultimediaWidgets PkgConfig::FFMPEG)
  target_compile_options(vivo-desktop PRIVATE -Wall -Wextra -Wpedantic)
endif()

include(CTest)
if(BUILD_TESTING)
  add_test(NAME desktop-helper-self-test COMMAND vivo-desktop-helper --self-test)
  set_tests_properties(desktop-helper-self-test PROPERTIES
    ENVIRONMENT "QT_QPA_PLATFORM=offscreen" TIMEOUT 15)
  if(TARGET vivo-desktop)
    add_test(NAME desktop-window-self-test COMMAND vivo-desktop --self-test)
    set_tests_properties(desktop-window-self-test PROPERTIES
      ENVIRONMENT "QT_QPA_PLATFORM=offscreen" TIMEOUT 15)
  endif()
endif()
