package main

import (
	"context"
	"fmt"
	"io"

	"github.com/unofficialpku/disk/internal/anyshare"
)

type libraryStats = anyshare.LibraryStatsBackend

func printLibraries(ctx context.Context, client libraryStats, entries []anyshare.Entry, sizes sizeFormat, output, status io.Writer) error {
	rows := make([][]string, 0, len(entries))
	stats := anyshare.LibraryStats(ctx, client, entries)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		storage, access := "unavailable", "?"
		stat := stats[entry.ID]
		if stat.PermissionError != nil {
			fmt.Fprintf(status, "Library %q permissions: %v\n", tuiText(entry.Name), stat.PermissionError)
		} else if stat.Writable != nil && *stat.Writable {
			access = "RW"
		} else {
			access = "RO"
		}
		quota := stat.Quota
		if stat.QuotaError != nil {
			fmt.Fprintf(status, "Library %q usage: %v\n", tuiText(entry.Name), stat.QuotaError)
		} else if quota != nil {
			storage = sizes.value(quota.Used)
			if access == "RW" {
				storage += " / " + sizes.value(quota.Allocated)
				if quota.Allocated > 0 {
					storage += fmt.Sprintf(" (%.1f%%)", 100*float64(quota.Used)/float64(quota.Allocated))
				}
			}
		}
		rows = append(rows, []string{entry.Name, entry.Type, storage, access, entry.ID})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return printTable(output, []string{"NAME", "TYPE", "USED", "ACCESS", "ID"}, rows, 2)
}
