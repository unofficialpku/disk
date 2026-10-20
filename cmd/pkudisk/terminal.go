package main

import (
	"context"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/display"
	"github.com/unofficialpku/disk/internal/tui"
	"io"
	"time"
)

var ttyInput = tui.Input
var interactiveIO = tui.Interactive
var tuiText = display.Text

func runTUI(ctx context.Context, backend interface {
	Roots(context.Context) ([]anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
}, account, server string, timeout time.Duration, sizes sizeFormat, in io.Reader, out io.Writer) error {
	return tui.Run(ctx, backend, account, server, timeout, display.SizeFormat{Bytes: sizes.bytes}, in, out)
}

var pickRecipient = tui.PickRecipient
