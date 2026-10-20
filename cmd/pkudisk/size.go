package main

import (
	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/display"
)

type sizeFormat struct{ bytes bool }

func (s *sizeFormat) add(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&s.bytes, "bytes", "b", false, "show exact byte counts instead of readable IEC units; JSON stays numeric")
}
func (s sizeFormat) value(n int64) string { return (display.SizeFormat{Bytes: s.bytes}).Value(n) }
func (s sizeFormat) label(n int64) string { return (display.SizeFormat{Bytes: s.bytes}).Label(n) }
