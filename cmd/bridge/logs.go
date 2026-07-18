package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/utils"
)

func runLogs(cmd *cobra.Command, _ []string) error {
	lines, _ := cmd.Flags().GetInt("lines")
	follow, _ := cmd.Flags().GetBool("follow")
	// Load (best effort) honors a configured log-dir; Defaults yields the
	// <exe-dir>/logs fallback if config is absent.
	cfg, _ := config.Load()
	path := cfg.LogFile()

	// Clean exit on Ctrl+C: cancel ctx, print a newline for a fresh prompt,
	// return nil.
	ctx, stop := signalContext()
	defer stop()

	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if !follow {
			fmt.Printf("no logs yet at %s — start the service to generate logs\n", path)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}

	// Rotation caps the file (~10 MiB), so reading it whole is fine.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, _ = os.Stdout.Write(utils.TailLines(data, lines))
	offset := int64(len(data))
	if !follow {
		return nil
	}

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			continue // rotating mid-cycle; retry next tick
		}
		size := fi.Size()
		if size < offset {
			offset = 0 // rotated: base replaced with a fresh file
		}
		if size <= offset {
			continue
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			continue
		}
		if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
			f.Close()
			continue
		}
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				_, _ = os.Stdout.Write(buf[:n])
				offset += int64(n)
			}
			if rerr != nil {
				break
			}
		}
		f.Close()
	}
}
