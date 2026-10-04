package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/connordoman/escpos"
	"github.com/connordoman/thermal/internal/queue"
	"github.com/gin-gonic/gin"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

type statusJSON struct {
	Ready             bool   `json:"ready"`
	Summary           string `json:"summary"`
	CoverOpen         bool   `json:"cover_open"`
	PaperEnd          bool   `json:"paper_end"`
	FeedButtonPressed bool   `json:"feed_button_pressed"`
	DrawerSignalHigh  bool   `json:"drawer_signal_high"`
	CutterError       bool   `json:"cutter_error"`
	Unrecoverable     bool   `json:"unrecoverable_error"`
	AutoRecoverable   bool   `json:"auto_recoverable_error"`
	Raw               string `json:"raw"` // the four DLE EOT bytes in hex
}

func toStatusJSON(s escpos.Status) statusJSON {
	return statusJSON{
		Ready: s.Ready(), Summary: queue.DescribeStatus(s),
		CoverOpen: s.Offline.CoverOpen(), PaperEnd: s.Paper.PaperEnd(),
		FeedButtonPressed: s.Offline.FeedButtonPressed(), DrawerSignalHigh: s.Printer.DrawerSignalHigh(),
		CutterError: s.Error.AutoCutterError(), Unrecoverable: s.Error.UnrecoverableError(),
		AutoRecoverable: s.Error.AutoRecoverableError(),
		Raw:             fmt.Sprintf("%02x %02x %02x %02x", byte(s.Printer), byte(s.Offline), byte(s.Error), byte(s.Paper)),
	}
}

// liveStatus queries the printer, waiting at most a few seconds (longer if
// a job is being sent).
func (s *Server) liveStatus(ctx context.Context) (*statusJSON, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.Device.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := toStatusJSON(st)
	return &out, nil
}

func statusError(err error) string {
	switch {
	case errors.Is(err, escpos.ErrNotReadable):
		return "this connection cannot read responses from the printer"
	case errors.Is(err, context.DeadlineExceeded):
		return "the printer did not answer in time"
	}
	return err.Error()
}

func (s *Server) printerInfo(c *gin.Context) {
	resp := gin.H{}
	q := &query{c: c}
	if q.bool("status", true) {
		st, err := s.liveStatus(c)
		if err != nil {
			resp["status_error"] = statusError(err)
		} else {
			resp["status"] = st
		}
	}
	resp["device"] = s.Device.Info()
	resp["worker"] = s.Queue.State()
	c.JSON(http.StatusOK, resp)
}

func (s *Server) printerStatus(c *gin.Context) {
	st, err := s.liveStatus(c)
	if err != nil {
		abortWith(c, http.StatusServiceUnavailable, "printer_unavailable", statusError(err), gin.H{"device": s.Device.Info()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": st, "worker": s.Queue.State()})
}
