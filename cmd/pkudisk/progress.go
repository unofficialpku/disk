package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/unofficialpku/disk/internal/transfer"
	"golang.org/x/term"
)

type cliProgress struct {
	label  string
	output io.Writer
	bar    *progressbar.ProgressBar
	job    bool
	total  int64
	sizes  sizeFormat
	phase  transfer.Phase
}

func newCLIProgress(direction string, output io.Writer, sizes sizeFormat) *cliProgress {
	return &cliProgress{label: strings.ToUpper(direction[:1]) + direction[1:], output: output, sizes: sizes}
}
func (p *cliProgress) Update(state transfer.Progress) {
	file, ok := p.output.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return
	}
	job := state.Phase == transfer.Preparing || state.Phase == transfer.Extracting
	total := state.Total
	if job {
		total = 100
	}
	if total < 0 {
		total = -1
	}
	if p.bar == nil || p.job != job || p.total != total || p.phase != state.Phase && (p.phase == transfer.Hashing || state.Phase == transfer.Hashing) {
		if p.bar != nil {
			_ = p.bar.Clear()
		}
		p.job, p.total = job, total
		p.phase = state.Phase
		// Reserve the last step for publication/file installation. This keeps
		// the displayed percentage below 100% until the operation succeeds.
		maximum := total + 1
		if total < 0 {
			maximum = -1
		}
		p.bar = progressbar.NewOptions64(maximum,
			progressbar.OptionSetWriter(p.output), progressbar.OptionSetWidth(24),
			progressbar.OptionShowBytes(!job && !p.sizes.bytes), progressbar.OptionShowTotalBytes(false),
			progressbar.OptionUseIECUnits(true), progressbar.OptionSetPredictTime(true),
			progressbar.OptionThrottle(100*time.Millisecond), progressbar.OptionUseANSICodes(true),
			progressbar.OptionOnCompletion(func() { fmt.Fprint(p.output, "\r\n") }),
		)
	}
	phase := ""
	if job {
		p.bar.Describe(fmt.Sprintf("%s: %s (%d%%)", p.label, state.Phase, state.Percent))
		_ = p.bar.Set64(int64(min(state.Percent, 99)))
		return
	}
	if state.Phase == transfer.Finalizing {
		phase = " finishing..."
	}
	detail := fmt.Sprintf("%s/%s%s", p.sizes.value(state.Done), p.sizes.value(state.Total), phase)
	if state.Total < 0 {
		detail = p.sizes.value(state.Done) + " (total unknown)"
	}
	p.bar.Describe(p.label + " " + detail)
	if state.Phase == transfer.Hashing {
		p.bar.Describe(p.label + " hashing " + detail)
	}
	if state.Phase == transfer.Complete {
		_ = p.bar.Finish()
	} else {
		_ = p.bar.Set64(state.Done)
	}
}
func (p *cliProgress) Finish(err error) {
	if p.bar == nil {
		return
	}
	if err != nil {
		p.bar.Describe(p.label + " failed")
		_ = p.bar.Exit()
	}
}
