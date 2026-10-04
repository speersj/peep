# peep

A small, low-latency viewer for RTSP camera feeds.

ffmpeg decodes the stream and pipes raw frames to peep, which smooths out
network jitter with a short playback buffer and draws the video with SDL3.
On Wayland it runs as a native Wayland client, and on X11 it uses X11. No
video is written to disk.

## Requirements

To build:

- Go 1.26 or newer. cgo is not needed: SDL3 is loaded at runtime.

To run:

- **ffmpeg** and **ffprobe**, on your `PATH`.
- **SDL3** (3.2 or newer), installed as a shared library (`libSDL3.so.0`).
- Optional:
  - **wl-copy** (Wayland) or **xclip** (X11) to copy screenshots to the
    clipboard. macOS uses `osascript`, which is built in.
  - A **Secret Service keyring**, such as gnome-keyring, KWallet or
    KeePassXC, to remember camera passwords. macOS uses the Keychain.
  - A **desktop notification server** (mako, dunst, swaync, Quickshell,
    GNOME, KDE, …) and **xdg-open** (from xdg-utils), to be told about
    screenshots and open them with a click. This is Linux and BSD only.
  - **ONNX Runtime** 1.23 or newer (`libonnxruntime.so`), for [detection](#detection).
  - A VA-API driver for hardware decoding, such as `intel-media-driver` for
    Intel GPUs (Broadwell and newer), `libva-mesa-driver` for AMD, or
    `libva-nvidia-driver` for NVIDIA. Without one, ffmpeg decodes on the CPU.

On Arch Linux:

```sh
sudo pacman -S go ffmpeg sdl3 wl-clipboard onnxruntime-cpu intel-media-driver
```

On Debian or Ubuntu, `libsdl3-0` is only in recent releases:

```sh
sudo apt install golang ffmpeg libsdl3-0 wl-clipboard
```

On macOS:

```sh
brew install go ffmpeg sdl3
```

## Building

```sh
git clone https://github.com/speersj/peep
cd peep
go build
```

This produces a `./peep` binary. To install it into `$(go env GOPATH)/bin`
instead, run:

```sh
go install github.com/speersj/peep@latest
```

## Running

```sh
peep [flags] [camera]
```

Cameras are saved under a name you choose, such as `frontdoor`.

- **`peep frontdoor`** connects to the saved camera called `frontdoor`
  (names are not case-sensitive):
  - If the camera has no username, or its password is saved in the keyring,
    it connects straight away.
  - Otherwise peep asks for the password, then offers to remember it.
  - If there is no camera called `frontdoor`, a short wizard adds one under
    that name. It asks for the host, stream path, which
    [detection](#detection) model to use, if any, and optionally a username
    and password.
- **`peep`** with no name lists the saved cameras, newest first, along with
  an option to add a new one.

| Key           | In the camera list                |
| ------------- | --------------------------------- |
| ↑ / ↓, j / k  | move                              |
| enter         | connect                           |
| e             | edit the selected camera          |
| t             | cycle the camera's detection model |
| d             | forget the selected camera        |
| q / esc       | quit                              |

In the wizard, enter moves to the next step and esc goes back. The host can
include a port (`host:port`, with `[addr]:port` for IPv6). If it does not, the
port defaults to **554**.

### Flags

| Flag       | Default | Description                                                                  |
| ---------- | ------- | ---------------------------------------------------------------------------- |
| `-buffer`  | `250ms` | Playback delay that smooths out network jitter; `0` for the lowest latency.  |
| `-hwaccel` | `auto`  | Hardware decoding: `auto`, `vaapi`, another ffmpeg method such as `cuda`, or `none`. See below. |
| `-stats`   | off     | Print playback statistics and ffmpeg warnings to stderr every second.        |

Flags and the camera name can be given in any order, e.g.
`peep frontdoor -buffer 0`.

With `-hwaccel auto` or `vaapi`, peep uses VA-API when a VA-API device is
available. The GPU then decodes and converts each frame, and only the
finished frame is copied back to memory, which takes less CPU than decoding
in software. If the GPU cannot decode the stream, for example because of an
unsupported codec, peep falls back to software decoding and says so.
Full-range (JPEG-style) streams are always decoded in software, because
VA-API scaling on Intel's driver does not convert their colour range. When
VA-API is not available, `auto` lets ffmpeg pick another method. `-stats`
shows which decoder is in use.

### In the video window

| Input          | Action                                                    |
| -------------- | --------------------------------------------------------- |
| esc, q         | quit                                                      |
| space          | save a screenshot, copy it to the clipboard and notify    |
| t              | cycle detection: YOLOX-s, D-FINE-S, off (saved)            |

Keys follow your keyboard layout, so a Caps Lock remapped to Escape (for
example with XKB's `caps:escape`) also quits.

Screenshots are saved at the stream's full resolution as
`~/Pictures/peep-YYYY-MM-DD-HH-MM.png`, using your local time zone. While
detection is on, the model is named too, as in
`peep-dfine-s-YYYY-MM-DD-HH-MM.png`. Further shots in the same minute get a
`-2`, `-3`, … suffix.

Each screenshot also shows a desktop notification. If your notification server
supports actions, clicking the notification opens the screenshot in your
default PNG viewer (`xdg-mime query default image/png`). A small detached peep
process waits for that click, so it still works after peep has quit. The
process exits when the notification is dismissed, or after an hour.

On Wayland, the window's app ID is `peep`, which you can use in compositor
window rules.

## Detection

Detection is a per-camera setting, with a choice of two models (see
[Models](#models)). The wizard asks which to use, if any, when you add or edit
a camera. `t` in the camera list, or in the video window while you watch,
cycles through YOLOX-s, D-FINE-S and off. The list shows each camera's model.

With detection on, peep looks for people, vehicles (bicycles, cars, motorcycles,
buses and trucks) and animals (birds, cats, dogs, horses, sheep, cows and
bears) about four times a second. It outlines each one in the video with its
label and confidence. Other objects, such as furniture, are ignored.

When something appears, peep shows a notification such as "Person detected",
naming the camera, the time, the confidence and the model. The notification has a
thumbnail, and clicking it opens the full frame with the detections outlined.
The frames are saved in `~/.cache/peep/detections`, with the camera, model
and class in the file name. While peep runs, it
checks that folder at startup and every 10 minutes. It deletes frames older
than a week, and if the folder still holds more than 1 GB, it deletes the
oldest 10% of frames until it is under that limit.

To avoid repeated or false alerts:

- an object must be seen in two analyses in a row before it is announced;
- an object that stays in view, such as a parked car, is announced once;
- a class is announced again only after it has been gone for 30 seconds.

### Models

Both models are Apache-2.0 licensed and trained on the COCO dataset. They run
on the CPU through ONNX Runtime, use under one core, and never hold up
playback. Each is downloaded the first time it is used, checked against a
fixed SHA-256, and kept in `~/.cache/peep/models`.

| Model    | Time per frame | Download | Notes |
| -------- | -------------- | -------- | ----- |
| YOLOX-s  | ~0.23s         | 36 MB    | From the YOLOX GitHub release. |
| D-FINE-S | ~0.33s         | 42 MB    | Pretrained on Objects365, then trained on COCO; scores about 10 points higher than YOLOX-s on COCO's accuracy benchmark. From Hugging Face's onnx-community export. |

Small or distant objects and night-time infrared footage are detected less
reliably by both.

## Saved cameras and passwords

A camera is saved in `~/.config/peep/cameras.json` (or the platform's
equivalent config directory) once it first connects successfully, so a
mistyped host is never saved. peep keeps the 20 most recently used. Cameras
saved by older versions of peep, which had no names, are named after their
host. Press `e` in the list to rename one.

Passwords are never written to that file. After you type a password, peep asks
whether to remember it. If you say yes, the password is stored in the system
keyring once the camera connects, and from then on that camera connects
without asking.

- **Changing a saved password:** select the camera, press `e`, enter the new
  password and answer `y`.
- **Removing a saved password:** answer `n` when asked to remember a newly
  entered password.
- **Forgetting a camera** with `d` also removes its saved password.
- **Renaming a camera** keeps its saved password. Changing its host, port,
  username or stream path removes the saved password.

## Development

```sh
go test ./...
go vet ./...
```

`TestDetectorOnFrame` runs a detection model on an image and can check for
an expected class. `PEEP_DETECT_MODEL` picks the model (`yolox-s`, the
default, or `dfine-s`). It needs ONNX Runtime and ffmpeg:

```sh
PEEP_DETECT_MODEL=dfine-s PEEP_DETECT_TEST=frame.png PEEP_DETECT_EXPECT=truck go test -run TestDetectorOnFrame -v .
```

`TestRenderMatchesSource` checks SDL's colour conversion and the screenshot
readback against ffmpeg's own conversion. It needs a display, a GPU, SDL3 and
ffmpeg, and opens a hidden window, so it only runs when asked:

```sh
PEEP_GPU_TEST=1 go test -run TestRenderMatchesSource -v .
```

To force a particular SDL backend, set `SDL_VIDEO_DRIVER=x11` or
`SDL_VIDEO_DRIVER=wayland`. To force a particular renderer, set
`SDL_RENDER_DRIVER=opengl` or `SDL_RENDER_DRIVER=opengles2`.
