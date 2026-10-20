module github.com/unofficialpku/disk/mobile

go 1.26.9

require github.com/unofficialpku/disk v0.0.0

require (
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/hanwen/go-fuse/v2 v2.8.0 // indirect
	github.com/winfsp/cgofuse v1.6.0 // indirect
	golang.org/x/mobile v0.0.0-20261011034528-197770444867 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
)

replace github.com/unofficialpku/disk => ..

tool (
	golang.org/x/mobile/cmd/gobind
	golang.org/x/mobile/cmd/gomobile
)
