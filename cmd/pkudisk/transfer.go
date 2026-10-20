package main

import (
	"context"
	"fmt"
	"io"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/transfer"
)

func runTransfer(ctx context.Context, client *anyshare.Client, direction, source, destination string, overwrite, archive, viaArchive, fast bool, sizes sizeFormat, out, errOut io.Writer) error {
	service := transfer.Service{Backend: client, Archive: archive, ViaArchive: viaArchive, Fast: fast}
	progress := newCLIProgress(direction, errOut, sizes)
	var result transfer.Result
	var err error
	if direction == "upload" {
		result, err = service.Upload(ctx, source, destination, overwrite, progress.Update)
	} else if direction == "extract" {
		result, err = service.Extract(ctx, source, destination, progress.Update)
	} else {
		result, err = service.Download(ctx, source, destination, overwrite, progress.Update)
	}
	progress.Finish(err)
	if err != nil {
		if result.Files > 0 && !viaArchive {
			return fmt.Errorf("%s %q: %w (%d files completed; directory transfers may be partial)", direction, source, err, result.Files)
		}
		return fmt.Errorf("%s %q: %w", direction, source, err)
	}
	if direction == "extract" {
		fmt.Fprintf(out, "Extracted %s -> %s\n", source, result.Entry.ID)
	} else {
		verb, from, to := "Downloaded", result.Remote, result.Local
		if direction == "upload" {
			verb, from, to = "Uploaded", result.Local, result.Remote
		}
		fmt.Fprintf(out, "%s %s -> %s (%s", verb, from, to, sizes.label(result.Bytes))
		if result.Entry.Dir {
			fmt.Fprintf(out, ", %d files, %d directories", result.Files, result.Dirs)
		}
		if result.FastFiles > 0 {
			fmt.Fprintf(out, ", %d instant upload(s)", result.FastFiles)
		}
		fmt.Fprintln(out, ")")
	}
	return nil
}
