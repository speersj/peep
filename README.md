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
| `-hwaccel` | `auto`  | ffmpeg hardware decoder, e.g. `vaapi` or `cuda`; `none` to disable.          |
| `-stats`   | off     | Print playback statistics and ffmpeg warnings to stderr every second.        |

Flags and the camera name can be given in any order, e.g.
`peep frontdoor -buffer 0`.

### In the video window

| Input          | Action                                                    |
| -------------- | --------------------------------------------------------- |
| esc, q         | quit                                                      |
| space, click   | save a screenshot, copy it to the clipboard and notify    |

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

`TestRenderMatchesSource` checks SDL's colour conversion and the screenshot
readback against ffmpeg's own conversion. It needs a display, a GPU, SDL3 and
ffmpeg, and opens a hidden window, so it only runs when asked:

```sh
PEEP_GPU_TEST=1 go test -run TestRenderMatchesSource -v .
```

To force a particular SDL backend, set `SDL_VIDEO_DRIVER=x11` or
`SDL_VIDEO_DRIVER=wayland`. To force a particular renderer, set
`SDL_RENDER_DRIVER=opengl` or `SDL_RENDER_DRIVER=opengles2`.
