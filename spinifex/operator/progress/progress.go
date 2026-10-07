// Package progress provides terminal progress and download helpers for operator commands.
package progress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/pterm/pterm"
)

// DownloadFileWithProgress downloads url to filename while presenting terminal
// progress appropriate to the response's known or unknown content length.
func DownloadFileWithProgress(url, name, filename string, timeout time.Duration) (err error) {
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	intCh := make(chan os.Signal, 1)
	signal.Notify(intCh, os.Interrupt)
	go func() {
		<-intCh
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("request error: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("file create error: %w", err)
	}
	defer f.Close()

	// The bar and spinner write cursor escapes and per-write lines even with
	// styling disabled, so plain output gets one start and one done line.
	if pterm.RawOutput {
		return downloadPlain(f, resp.Body, name, resp.ContentLength)
	}

	if resp.ContentLength > 0 {
		total := safecast.Int64ToUint64(resp.ContentLength)
		bar, update := NewByteProgressBar(fmt.Sprintf("Downloading %s", name), total)
		var current uint64
		lastPct := -1
		// io.Copy writes ~32 KiB per TeeReader call, so gate rendering on
		// integer-percentage change to cap the bar at <=101 renders instead of
		// re-rendering tens of thousands of times.
		reader := io.TeeReader(resp.Body, progressWriter(func(n int) {
			current += safecast.IntToUint64(n)
			pct := safecast.Uint64ToInt(current * 100 / total)
			if pct > lastPct {
				lastPct = pct
				update(current)
			}
		}))
		_, err = io.Copy(f, reader)
		_, _ = bar.Stop()
		if err != nil {
			return fmt.Errorf("copy error: %w", err)
		}
		return err
	}

	spin, _ := pterm.DefaultSpinner.
		WithText("Downloading (size unknown)...").
		Start()
	var written int64
	reader := io.TeeReader(resp.Body, progressWriter(func(n int) {
		written += int64(n)
		spin.UpdateText(fmt.Sprintf("Downloading %s (%s) ...", name, HumanBytes(safecast.Int64ToUint64(written))))
	}))
	_, err = io.Copy(f, reader)
	_ = spin.Stop()
	if err != nil {
		return fmt.Errorf("copy error: %w", err)
	}
	return nil
}

func downloadPlain(dst io.Writer, src io.Reader, name string, contentLength int64) error {
	size := "size unknown"
	if contentLength > 0 {
		size = HumanBytes(safecast.Int64ToUint64(contentLength))
	}
	pterm.Printfln("Downloading %s (%s) ...", name, size)

	written, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("copy error: %w", err)
	}
	pterm.Printfln("Downloaded %s (%s)", name, HumanBytes(safecast.Int64ToUint64(written)))
	return nil
}

// progressWriter turns byte counts into a callback for UI updates.
type progressWriter func(n int)

func (pw progressWriter) Write(p []byte) (int, error) {
	pw(len(p))
	return len(p), nil
}

// NewByteProgressBar starts a pterm progress bar that renders human-readable
// sizes in its title instead of raw byte counts, and returns an update func
// that performs one render per call. Callers must invoke update at a throttled
// cadence (integer-percentage change) because the render itself is expensive.
//
// pterm's elapsed-time display normally spawns a background goroutine that
// re-renders every second with no lock, tearing the line against our own
// low-frequency renders. Starting with it off means no timer is spawned; the
// flag is then set on the returned bar so pterm still appends its elapsed-time
// suffix, emitted only on our renders.
func NewByteProgressBar(title string, total uint64) (*pterm.ProgressbarPrinter, func(current uint64)) {
	totalHuman := HumanBytes(total)
	bar, _ := pterm.DefaultProgressbar.
		WithTitle(title).
		WithTotal(safecast.Uint64ToInt(total)).
		WithShowCount(false).
		WithShowElapsedTime(false).
		Start()
	bar.ShowElapsedTime = true
	update := func(current uint64) {
		bar.Current = safecast.Uint64ToInt(current)
		bar.UpdateTitle(fmt.Sprintf("%s — %s / %s", title, HumanBytes(current), totalHuman))
	}
	return bar, update
}

// HumanBytes formats a byte count using IEC binary suffixes (KiB, MiB, ...).
// Values below 1024 render as exact bytes.
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPEZY"[exp])
}
