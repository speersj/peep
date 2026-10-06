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
    that name. It asks for the host, stream path, and optionally a username
    and password.
- **`peep`** with no name lists the saved cameras, newest first, along with
  an option to add a new one.

| Key           | In the camera list                |
| ------------- | --------------------------------- |
| ↑ / ↓, j / k  | move                              |
| enter         | connect                           |
| e             | edit the selected camera          |
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
| t              | turn detection on or off (saved)                          |
| n              | turn detection notifications off or on (saved)            |

Keys follow your keyboard layout, so a Caps Lock remapped to Escape (for
example with XKB's `caps:escape`) also quits.

Screenshots are saved at the stream's full resolution as
`~/Pictures/peep-YYYY-MM-DD-HH-MM.png`, using your local time zone. Further
shots in the same minute get a `-2`, `-3`, … suffix.

Each screenshot also shows a desktop notification. If your notification server
supports actions, clicking the notification opens the screenshot in your
default PNG viewer (`xdg-mime query default image/png`). A small detached peep
process waits for that click, so it still works after peep has quit. The
process exits when the notification is dismissed, or after an hour.

On Wayland, the window's app ID is `peep`, which you can use in compositor
window rules.

## Detection

Detection is off at first. Press `t` in the video window to turn it on or
off, or set it in the [settings file](#settings). The choice applies to every
camera and is remembered.

With detection on, peep looks for people, vehicles (bicycles, cars, motorcycles,
buses and trucks) and animals (birds, cats, dogs, horses, sheep, cows and
bears) about four times a second. It outlines each one in the video with its
label and confidence. Objects need a confidence of at least 50%, and birds at
least 60%. Other objects, such as furniture, are ignored.

When something appears, peep shows a notification such as "Person detected",
naming the camera, the time and the confidence. The notification has a
thumbnail, and clicking it opens the full frame with the detections outlined.
The frames are saved in `~/.cache/peep/detections`. While peep runs, it
checks that folder at startup and every 10 minutes. It deletes frames older
than a week, and if the folder still holds more than 1 GB, it deletes the
oldest 10% of frames until it is under that limit, which can be changed in
the [settings file](#settings).

Press `n` in the video window to stop the notifications while still saving
the frames, and again to bring them back. This is remembered for every
camera, and "notifications off" shows in the top right corner while they are
off.
Screenshot notifications are not affected.

To avoid repeated or false alerts:

- an object must be seen in two analyses in a row before it is announced;
- an object that stays in view, such as a parked car, is announced once;
- a class is announced again only after it has been gone for 30 seconds;
- something that reappears exactly where one was seen before is not announced
  again. This covers a garden lamp mistaken for a bird, or a parked car that
  flickers in and out of detection at night. Its box is still drawn. A place
  is forgotten after an hour with nothing seen there.

Detection uses D-FINE-S (Apache-2.0), a model pretrained on the Objects365
dataset and then trained on COCO. It runs on the CPU through ONNX Runtime,
takes about a third of a second per frame and uses under one core, and never
holds up playback. The model (42 MB) is downloaded from Hugging Face's
onnx-community export the first time you use detection, checked against a
fixed SHA-256, and kept in `~/.cache/peep/models`.

Small or distant objects and night-time infrared footage are detected less
reliably.

## Settings

Settings that apply to every camera are kept in `~/.config/peep/settings.json`
(or the platform's equivalent config directory), which peep creates the first
time it runs. You can edit it with any text editor:

```json
{
  "detect": false,
  "quiet": false,
  "max_detection_storage": "1 GB"
}
```

| Setting                 | Default  | Description                                                          |
| ----------------------- | -------- | -------------------------------------------------------------------- |
| `detect`                | `false`  | [Detection](#detection) on; `t` in the video window changes it.      |
| `quiet`                 | `false`  | No notifications for detections; `n` in the video window changes it. |
| `max_detection_storage` | `"1 GB"` | Space kept for saved detection frames.                               |

Sizes can be in KB, MB, GB or TB (powers of 1024), and decimals such as
`"1.5 GB"` work. A new storage limit applies at peep's next check of the
folder, without restarting. `detect` and `quiet` are read when peep starts.
Pressing `t` or `n` rewrites the file. If the file can't be read, peep uses
the defaults and says why in the terminal.

Older versions of peep kept `detect` and `quiet` for each camera. The first
time this version runs, it copies them from the most recently used camera
that had detection on.

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

`TestDetectorOnFrame` runs the detection model on an image and can check for
an expected class. It needs ONNX Runtime and ffmpeg:

```sh
PEEP_DETECT_TEST=frame.png PEEP_DETECT_EXPECT=truck go test -run TestDetectorOnFrame -v .
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
