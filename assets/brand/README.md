The logo in `source.png` was supplied by the project owner as `~/Downloads/下载.png`.
Generated web, tray, ICO and ICNS assets preserve this artwork. The macOS tray
template extracts its white foreground for AppKit's automatic light/dark tint.

Regenerate with Pillow 12.3.0:

```sh
python scripts/generate-icons.py
go run github.com/tc-hib/go-winres@v0.3.3 make \
  --in desktop/winres.json --out cmd/pkudisk/rsrc --arch amd64,arm64
```

The checked-in `.syso` files embed the application icon in both Windows builds
without requiring a resource compiler or CGO during normal Go builds.
