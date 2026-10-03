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

On Arch Linux:

```sh
sudo pacman -S go ffmpeg sdl3 wl-clipboard
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

Run `peep` with no host to open the interactive picker:

```sh
peep
```

The picker lists cameras you have opened before, newest first, along with an
option to add a new one. Picking "New camera" starts a short wizard that asks
for the host, stream name, username and password.

| Key           | In the picker                     |
| ------------- | --------------------------------- |
| ↑ / ↓, j / k  | move                              |
| enter         | connect                           |
| e             | edit the selected camera          |
| d             | forget the selected camera        |
| q / esc       | quit                              |

To skip the picker, give the connection on the command line:

```sh
peep -n live/ch0 camera.local
peep -u admin -n Stream1 192.168.1.50:8554
peep -u admin -n live [fe80::1]:554
```

The host can include a port (`host:port`, with `[addr]:port` for IPv6). If it
does not, the port defaults to **554**. When `-user` is set without
`-password`, peep uses the password saved in the keyring if there is one, and
otherwise asks for it without echoing it.

### Flags

| Flag                 | Default | Description                                                         |
| -------------------- | ------- | ------------------------------------------------------------------- |
| `-name`, `-n`        |         | RTSP stream path, e.g. `live/ch0`. Required with a host.            |
| `-user`, `-u`        |         | RTSP username.                                                      |
| `-password`, `-pw`   |         | RTSP password. Avoid this flag: the password ends up in your shell history. |
| `-buffer`            | `250ms` | Playback delay that smooths out network jitter; `0` for the lowest latency. |
| `-hwaccel`           | `auto`  | ffmpeg hardware decoder, e.g. `vaapi` or `cuda`; `none` to disable. |
| `-stats`             | off     | Print playback statistics and ffmpeg warnings to stderr every second. |

Flags and the host can be given in any order.

### In the video window

| Input          | Action                                                    |
| -------------- | --------------------------------------------------------- |
| esc, q         | quit                                                      |
| space, click   | save a screenshot and copy it to the clipboard            |

Keys follow your keyboard layout, so a Caps Lock remapped to Escape (for
example with XKB's `caps:escape`) also quits.

Screenshots are saved at the stream's full resolution as
`~/Pictures/peep-YYYY-MM-DD-HH-MM.png`, using your local time zone. Further
shots in the same minute get a `-2`, `-3`, … suffix. On Wayland, the window's
app ID is `peep`, which you can use in compositor window rules.

## Saved cameras and passwords

Each camera that opens successfully is remembered in
`~/.config/peep/cameras.json` (or the platform's equivalent config directory).
peep keeps the 20 most recently used.

Passwords are never written to that file. After you type a password in the
picker, peep asks whether to remember it. If you say yes, the password is
stored in the system keyring once the camera connects, and that camera then
connects without asking. To change a saved password, select the camera, press
`e`, enter the new password and answer `y`. Answering `n`, or forgetting the
camera with `d`, removes the saved password from the keyring.

## Development

```sh
go test ./...
go vet ./...
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
