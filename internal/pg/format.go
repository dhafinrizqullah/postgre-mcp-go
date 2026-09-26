package pg

import (
	"time"

	"github.com/dustin/go-humanize"
)

// Human-readable formatting for the health report. The report is read by an LLM,
// so "1.2 GB" and "5m30s" say more than 1288490188 and 330.

func humanBytes(n int64) string { return humanize.IBytes(uint64(n)) }

func humanCount(n int64) string { return humanize.Comma(n) }

func humanSeconds(f float64) string {
	if f <= 0 {
		return "0s"
	}
	return (time.Duration(f) * time.Second).Round(time.Second).String()
}
