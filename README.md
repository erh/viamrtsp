Prep Linux
===
* sudo apt install libswscale-dev libavcodec-dev libavformat-dev

Build for Linux
===
* Start canon `canon -arch arm64` or `canon -arch amd64`
* Install deps `make linux-dep`
* Create golang binary `make build`
* Create appimage `make package`
* Create module tar `make module`

Build for Android
===
* Use android rdk branch `make edit-android`
* Install FFmpeg `make ffmpeg-android`
* Build golang binary `GOOS=android GOARCH=arm64 make build`
* Create module tar `GOOS=android GOARCH=arm64 make module`

Notes
===
* Heavily cribbed from https://github.com/bluenviron/gortsplib/blob/main/examples/client-read-format-h264-convert-to-jpeg/main.go

Sample Config
===
```
{
      "rtsp_address": "rtsp://foo:bar@192.168.10.10:554/stream"
}
```
