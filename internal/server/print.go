package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/connordoman/escpos"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/render"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/gin-gonic/gin"
)

// maxWait bounds ?wait=.
const maxWait = 5 * time.Minute

// jobOptions are the query parameters every print endpoint accepts.
type jobOptions struct {
	finish   render.Finish
	cutSet   bool
	copies   int
	priority int
	label    string
	wait     time.Duration
	dryRun   bool
}

// queryError is a bad query parameter.
type queryError struct{ param, msg string }

func (e *queryError) Error() string { return e.param + ": " + e.msg }

type query struct {
	c   *gin.Context
	err error
}

func (q *query) has(name string) bool {
	_, ok := q.c.GetQuery(name)
	return ok
}

func (q *query) fail(name, msg string) {
	if q.err == nil {
		q.err = &queryError{name, msg}
	}
}

func (q *query) str(name, def string) string {
	if v, ok := q.c.GetQuery(name); ok {
		return v
	}
	return def
}

func (q *query) int(name string, def, lo, hi int) int {
	v, ok := q.c.GetQuery(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		q.fail(name, fmt.Sprintf("must be an integer from %d to %d", lo, hi))
		return def
	}
	return n
}

func (q *query) float(name string, def, lo, hi float64) float64 {
	v, ok := q.c.GetQuery(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < lo || n > hi {
		q.fail(name, fmt.Sprintf("must be a number from %g to %g", lo, hi))
		return def
	}
	return n
}

func (q *query) bool(name string, def bool) bool {
	v, ok := q.c.GetQuery(name)
	if !ok {
		return def
	}
	if v == "" {
		return true // ?bold
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		q.fail(name, "must be true or false")
		return def
	}
	return b
}

func (q *query) align(name string) escpos.Align {
	switch v := q.str(name, "left"); v {
	case "left", "center", "right":
		return render.ParseAlign(v)
	default:
		q.fail(name, "must be left, center or right")
		return escpos.AlignLeft
	}
}

func (q *query) jobOptions() jobOptions {
	var o jobOptions
	cut, err := render.ParseCutMode(q.str("cut", ""))
	if err != nil {
		q.fail("cut", err.Error())
	}
	o.cutSet = q.has("cut")
	o.finish = render.Finish{
		Cut:        cut,
		Feed:       uint8(q.int("feed", 0, 0, 255)),
		OpenDrawer: q.bool("open_drawer", false),
		Beep:       q.bool("beep", false),
	}
	o.copies = q.int("copies", 1, 1, 20)
	o.priority = q.int("priority", 0, -100, 100)
	o.label = q.str("label", "")
	if utf8.RuneCountInString(o.label) > 200 {
		q.fail("label", "must be at most 200 characters")
	}
	o.dryRun = q.bool("dry_run", false)
	switch w := q.str("wait", ""); w {
	case "", "false", "0":
	case "true":
		o.wait = time.Minute
	default:
		d, err := time.ParseDuration(w)
		if err != nil {
			if n, nerr := strconv.Atoi(w); nerr == nil {
				d, err = time.Duration(n)*time.Second, nil
			}
		}
		if err != nil || d < 0 || d > maxWait {
			q.fail("wait", "must be a duration such as 30s, up to "+maxWait.String())
		}
		o.wait = d
	}
	return o
}

func readBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(c.Request.Body)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		abort(c, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", mbe.Limit))
		return nil, false
	}
	if err != nil {
		abort(c, http.StatusBadRequest, "bad_request", "reading request body: "+err.Error())
		return nil, false
	}
	return body, true
}

// renderError reports a rendering failure, distinguishing the client's
// mistakes from the server's.
func renderError(c *gin.Context, err error) {
	var se *render.SchemaError
	var ie *render.ImageError
	var be *render.BlockError
	var qe *queryError
	switch {
	case errors.As(err, &se):
		abortWith(c, http.StatusUnprocessableEntity, "invalid_document", "the document does not match the schema (see /v1/schema/job.json)", se.Problems)
	case errors.As(err, &ie):
		abortWith(c, http.StatusUnprocessableEntity, "invalid_image", ie.Error(), blockPath(err))
	case errors.As(err, &be):
		abortWith(c, http.StatusUnprocessableEntity, "invalid_block", be.Error(), gin.H{"path": be.Path})
	case errors.As(err, &qe):
		abortWith(c, http.StatusBadRequest, "invalid_parameter", qe.Error(), gin.H{"parameter": qe.param})
	case errors.Is(err, escpos.ErrInvalidArgument):
		abort(c, http.StatusUnprocessableEntity, "invalid_argument", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		abort(c, http.StatusRequestTimeout, "timeout", "rendering did not finish in time")
	default:
		console.Error("rendering job: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "rendering failed")
	}
}

func blockPath(err error) any {
	var be *render.BlockError
	if errors.As(err, &be) {
		return gin.H{"path": be.Path}
	}
	return nil
}

func (s *Server) env() *render.Env {
	return render.NewEnv(s.Device.PaperWidth(), s.Images)
}

// renderTimeout bounds rendering, which may fetch images.
const renderTimeout = time.Minute

// submit stores a rendered job (or returns it for a dry run) and responds.
func (s *Server) submit(c *gin.Context, kind string, o jobOptions, body, payload []byte) {
	if o.dryRun {
		sum := sha256.Sum256(payload)
		c.Header("X-Payload-SHA256", hex.EncodeToString(sum[:]))
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", kind+".bin"))
		c.Data(http.StatusOK, "application/octet-stream", payload)
		return
	}
	if len(payload) == 0 {
		abort(c, http.StatusUnprocessableEntity, "empty_job", "the job has nothing to print")
		return
	}
	sum := sha256.Sum256(payload)
	key := currentKey(c)
	id, err := s.Queue.Submit(c, dbq.CreateJobParams{
		Kind:          kind,
		Priority:      int64(o.priority),
		Label:         store.NullString(o.label),
		Copies:        int64(o.copies),
		ApiKeyID:      store.NullString(key.ID),
		ClientIp:      store.NullString(c.ClientIP()),
		UserAgent:     store.NullString(truncate(c.Request.UserAgent(), 512)),
		ContentType:   store.NullString(truncate(c.ContentType(), 128)),
		Source:        body,
		SourceSize:    int64(len(body)),
		Payload:       payload,
		PayloadSize:   int64(len(payload)),
		PayloadSha256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		console.Error("storing job: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not queue the job")
		return
	}
	s.respondJob(c, id, o.wait)
}

// respondJob reports a newly queued job, after waiting for it to finish if
// asked. 200 means finished (check status), 202 means still in progress.
func (s *Server) respondJob(c *gin.Context, id int64, wait time.Duration) {
	c.Header("Location", fmt.Sprintf("/v1/jobs/%d", id))
	ctx := context.Context(c)
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(c, wait)
		defer cancel()
		job, err := s.Queue.Wait(ctx, id)
		if err == nil {
			code := http.StatusAccepted
			if jobFinished(job.Status) {
				code = http.StatusOK
			}
			c.JSON(code, gin.H{"job": s.jobJSON(c, job)})
			return
		}
	}
	job, err := s.Store.GetJob(c, id)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", "could not read the job")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job": s.jobJSON(c, job)})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func (s *Server) printRaw(c *gin.Context) {
	q := &query{c: c}
	o := q.jobOptions()
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	payload := body
	// Raw bytes go out untouched unless a cut was asked for explicitly.
	if o.cutSet || o.finish.OpenDrawer || o.finish.Beep || o.finish.Feed > 0 {
		b := escpos.NewBuilder(s.Device.PaperWidth())
		b.Raw(body...)
		if !o.cutSet {
			o.finish.Cut = render.CutNone
		}
		o.finish.Apply(b)
		payload = b.Bytes()
	}
	s.submit(c, "raw", o, body, payload)
}

func (s *Server) printText(c *gin.Context) {
	q := &query{c: c}
	o := q.jobOptions()
	size := uint8(q.int("size", 1, 1, 8))
	to := render.TextOptions{
		Style: render.Style{
			Bold:      q.bool("bold", false),
			Underline: uint8(q.int("underline", 0, 0, 2)),
			Invert:    q.bool("invert", false),
			FontB:     strings.EqualFold(q.str("font", "a"), "b"),
			Width:     uint8(q.int("width", int(size), 1, 8)),
			Height:    uint8(q.int("height", int(size), 1, 8)),
		},
		Align:  q.align("align"),
		NoWrap: !q.bool("wrap", true),
	}
	if f := strings.ToLower(q.str("font", "a")); f != "a" && f != "b" {
		q.fail("font", "must be a or b")
	}
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	payload, err := render.RenderText(s.env(), string(body), to, o.finish)
	if err != nil {
		renderError(c, err)
		return
	}
	s.submit(c, "text", o, body, payload)
}

func (s *Server) printMarkdown(c *gin.Context) {
	q := &query{c: c}
	o := q.jobOptions()
	mo := render.MarkdownOptions{
		Links:   render.LinkMode(q.str("links", "inline")),
		Unicode: render.UnicodeMode(q.str("unicode", "image")),
		Images:  q.bool("images", true),
	}
	switch mo.Links {
	case render.LinksInline, render.LinksFootnotes, render.LinksQR, render.LinksHide:
	default:
		q.fail("links", "must be inline, footnotes, qr or hide")
	}
	switch mo.Unicode {
	case render.UnicodeImage, render.UnicodeTransliterate:
	default:
		q.fail("unicode", "must be image or transliterate")
	}
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c, renderTimeout)
	defer cancel()
	payload, err := render.RenderMarkdown(ctx, s.env(), string(body), mo, o.finish)
	if err != nil {
		renderError(c, err)
		return
	}
	s.submit(c, "markdown", o, body, payload)
}

func (s *Server) printUnicode(c *gin.Context) {
	q := &query{c: c}
	o := q.jobOptions()
	uo := render.UnicodeOptions{
		Scale:  q.float("scale", 2, 1, 8),
		Bold:   q.bool("bold", false),
		Invert: q.bool("invert", false),
		Align:  q.align("align"),
		NoWrap: !q.bool("wrap", true),
	}
	uo.SolidEmoji = q.bool("solid_emoji", false)
	if q.has("weight") {
		wt := q.int("weight", 1, 0, 4)
		uo.Weight = &wt
	}
	if q.has("line_gap") {
		g := q.int("line_gap", 0, 0, 255)
		uo.LineGap = &g
	}
	format := q.str("format", "escpos")
	if format != "escpos" && format != "png" {
		q.fail("format", "must be escpos or png")
	}
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	if format == "png" {
		// A preview: nothing is queued.
		img, err := render.RenderUnicode(strings.TrimRight(string(body), "\n"), s.Device.PaperWidth(), uo)
		if err != nil {
			renderError(c, err)
			return
		}
		var buf bytes.Buffer
		png.Encode(&buf, img)
		c.Data(http.StatusOK, "image/png", buf.Bytes())
		return
	}
	payload, err := render.RenderUnicodeText(s.env(), string(body), uo, o.finish)
	if err != nil {
		renderError(c, err)
		return
	}
	s.submit(c, "utf8", o, body, payload)
}

func (s *Server) printJSON(c *gin.Context) {
	q := &query{c: c}
	o := q.jobOptions()
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	doc, err := render.ParseDocument(body)
	if err != nil {
		renderError(c, err)
		return
	}
	// Document options apply unless the query overrides them.
	if !q.has("copies") && doc.Options.Copies > 0 {
		o.copies = doc.Options.Copies
	}
	if !q.has("priority") {
		o.priority = doc.Options.Priority
	}
	if !q.has("label") {
		o.label = doc.Label()
	}
	if q.has("cut") {
		doc.Options.Cut = string(o.finish.Cut)
	}
	ctx, cancel := context.WithTimeout(c, renderTimeout)
	defer cancel()
	payload, err := render.RenderDocument(ctx, s.env(), doc)
	if err != nil {
		renderError(c, err)
		return
	}
	s.submit(c, "json", o, body, payload)
}
